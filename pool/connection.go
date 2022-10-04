package pool

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"net/url"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Connection is an internal representation of amqp.Connection.
type Connection struct {
	url     string
	name    string
	id      int64
	flagged bool // whether an error occurred on this connection or not, indicating the connectionmust be recovered

	tls *tls.Config

	conn *amqp.Connection

	errorBackoff BackoffFunc

	heartbeat   time.Duration
	connTimeout time.Duration

	errors   chan *amqp.Error
	blockers chan amqp.Blocking

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// NewConnection creates a connection wrapper.
// name: unique connection name
func NewConnection(connectUrl, name string, id int64, options ...ConnectionOption) (*Connection, error) {
	// use sane defaults
	option := connectionOption{
		HeartbeatInterval: 15 * time.Second,
		ConnectionTimeout: 30 * time.Second,
		BackoffPolicy:     newDefaultBackoffPolicy(time.Second, 15*time.Second),
		Ctx:               context.Background(),
	}

	// apply options
	for _, o := range options {
		o(&option)
	}

	u, err := url.Parse(connectUrl)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConnectURL, err)
	}

	if option.TLSConfig != nil {
		u.Scheme = "amqps"
	}

	// we derive a new context from the parent one in order to
	// be able to close it without affecting the parent
	ctx, cancel := context.WithCancel(option.Ctx)

	conn := &Connection{
		url:     u.String(),
		name:    name,
		id:      id,
		flagged: false,
		tls:     option.TLSConfig,

		conn: nil, // will be initialized below

		heartbeat:    option.HeartbeatInterval,
		connTimeout:  option.ConnectionTimeout,
		errorBackoff: option.BackoffPolicy,

		errors:   make(chan *amqp.Error, 10),
		blockers: make(chan amqp.Blocking, 10),

		ctx:    ctx,
		cancel: cancel,
	}

	err = conn.Connect()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (ch *Connection) ID() int64 {
	// no mutex lock, as we do read only the id and write only upon initialization
	return ch.id
}

// Flag flags the connection as broken which must be recovered.
// A flagged connection implies a closed connection.
// Flagging of a connectioncan only be undone by Recover-ing the connection.
func (ch *Connection) Flag(flagged bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	if !ch.flagged && flagged {
		ch.flagged = flagged
	}
}

// Connect tries to connect (or reconnect)
// Does not block indefinitly, but returns an error
// upon connection failure.
func (ch *Connection) Connect() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.connect()
}

func (ch *Connection) connect() error {

	// not closed, reuse
	if !ch.isClosed() {
		return nil
	}

	amqpConn, err := amqp.DialConfig(ch.url,
		amqp.Config{
			Heartbeat:       ch.heartbeat,
			Dial:            amqp.DefaultDial(ch.connTimeout),
			TLSClientConfig: ch.tls,
			Properties: amqp.Table{
				"connection_name": ch.name,
			},
		})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}

	// override upon reconnect
	ch.conn = amqpConn
	ch.errors = make(chan *amqp.Error, 10)
	ch.blockers = make(chan amqp.Blocking, 10)

	// ch.Errors is closed by streadway/amqp in some scenarios :(
	ch.conn.NotifyClose(ch.errors)
	ch.conn.NotifyBlocked(ch.blockers)

	return nil
}

// PauseOnFlowControl allows you to wait and sleep while receiving flow control messages.
// Sleeps for one second, repeatedly until the blocking has stopped.
// Such messages will most likely be received when the broker hits its memory or disk limits.
func (ch *Connection) PauseOnFlowControl() {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	ch.pauseOnFlowControl()
}

// not threadsafe
func (ch *Connection) pauseOnFlowControl() {
	timer := time.NewTimer(time.Second)
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()

	for !ch.isClosed() {

		select {
		case blocker := <-ch.blockers: // Check for flow control issues.
			if !blocker.Active {
				return
			}

			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(time.Second)

			select {
			case <-ch.catchShutdown():
				return
			case <-timer.C:
				continue
			}

		default:
			return
		}
	}
}

// not threadsafe
func (ch *Connection) isClosed() bool {
	return ch.flagged || ch.conn == nil || ch.conn.IsClosed() || ch.isShutdown()
}

func (ch *Connection) IsClosed() bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	// connection closed 							-> cannot access it
	// connection not closed but shutdown triggered -> is closed
	return ch.isClosed()
}

func (ch *Connection) Close() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	ch.cancel() // close derived context
	if ch.conn != nil && !ch.conn.IsClosed() {
		return ch.conn.Close() // close internal channel
	}
	return nil
}

// Error returns the first error from the errors channel
// and flushes all other pending errors from the channel
// In case that there are no errors, nil is returned.
func (ch *Connection) Error() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.error()
}

// not threadsafe
func (ch *Connection) error() error {
	var (
		err error = nil
	)
	for {
		select {
		case <-ch.catchShutdown():
			return ErrConnectionClosed
		case e, ok := <-ch.errors:
			if !ok {
				// because the amqp library might close this
				// channel, we asume that closing was done due to
				// a library error
				return ErrConnectionClosed
			}
			// only overwrite with the first error
			if err == nil {
				err = e
			} else {
				// flush all other errors after the first one
				continue
			}
		default:
			return err
		}
	}
}

// Recover tries to recover the connection until
// a shutdown occurs via context cancelation.
func (ch *Connection) Recover() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.recover()
}

func (ch *Connection) recover() error {
	healthy := ch.error() == nil

	if healthy && !ch.isClosed() {
		ch.pauseOnFlowControl()
		return nil
	}

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()

	for retry := 0; ; retry++ {
		err := ch.connect()
		if err != nil {
			// reset to exponential backoff
			timer.Reset(ch.errorBackoff(retry))
			select {
			case <-ch.catchShutdown():
				// catch shutdown signal
				return fmt.Errorf("connection recovery failed: %w", ErrPoolClosed)
			case <-timer.C:
				if !timer.Stop() {
					<-timer.C
				}
				// retry after sleep
				continue
			}
		}

		// connection established successfully
		break
	}

	ch.flagged = false

	return nil
}

func (ch *Connection) catchShutdown() <-chan struct{} {
	return ch.ctx.Done()
}

func (ch *Connection) isShutdown() bool {
	select {
	case <-ch.ctx.Done():
		return true
	default:
		return false
	}
}