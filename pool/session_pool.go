package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

type SessionPool struct {
	pool *ConnectionPool

	transientID atomic.Int64

	size        int
	bufferSize  int
	confirmable bool
	sessions    chan *Session

	ctx    context.Context
	cancel context.CancelFunc
}

func NewSessionPool(pool *ConnectionPool, size int, options ...SessionPoolOption) (*SessionPool, error) {
	if size < 1 {
		panic("max pool size is negative or 0")
	}

	// use sane defaults
	option := sessionPoolOption{
		Size:        size,
		Confirmable: false,
		BufferSize:  1,        // fault tolerance over throughput
		Ctx:         pool.ctx, // derive context from parent
	}

	for _, o := range options {
		o(&option)
	}

	return newSessionPoolFromOption(pool, option)
}

func newSessionPoolFromOption(pool *ConnectionPool, option sessionPoolOption) (*SessionPool, error) {
	// decouple from parent context, in case we want to close this context ourselves.
	ctx, cancel := context.WithCancel(option.Ctx)

	sessionPool := &SessionPool{
		pool: pool,

		size:        option.Size,
		bufferSize:  option.BufferSize,
		confirmable: option.Confirmable,
		sessions:    make(chan *Session, option.Size),

		ctx:    ctx,
		cancel: cancel,
	}

	err := sessionPool.initCachedSessions()
	if err != nil {
		return nil, err
	}

	return sessionPool, nil
}

// Size returns the size of the session pool which indicate sthe number of available cached sessions.
func (sp *SessionPool) Size() int {
	return sp.size
}

// GetSession gets a pooled session.
// blocks until a session is acquired from the pool.
func (sp *SessionPool) GetSession() (*Session, error) {
	select {
	case <-sp.catchShutdown():
		return nil, ErrClosed
	case session, ok := <-sp.sessions:
		if !ok {
			return nil, fmt.Errorf("failed to get session: %w", ErrClosed)
		}
		return session, nil
	}
}

// GetTransientSession returns a transient session.
// This method may return an error when the context ha sbeen closed before a session could be obtained.
// A transient session creates a transient connection under the hood.
func (sp *SessionPool) GetTransientSession(ctx context.Context) (*Session, error) {
	conn, err := sp.pool.GetTransientConnection(ctx)
	if err != nil {
		return nil, err
	}

	transientId := sp.transientID.Add(1)
	return NewSession(conn, transientId,
		SessionWithContext(ctx),
		SessionWithBufferSize(sp.size),
		SessionWithConfirms(sp.confirmable),
		SessionWithCached(false),
		SessionWithAutoCloseConnection(true),
	)
}

// ReturnSession returns a Session.
// If Session is not a cached channel, it is simply closed here.
// If Cache Session, we check if erred, new Session is created instead and then returned to the cache.
func (sp *SessionPool) ReturnSession(session *Session, erred bool) {

	// don't ass non-managed sessions back to the channel
	if !session.IsCached() {
		session.Close()
		return
	}

	if erred {
		err := session.Recover()
		if err != nil {
			// error is only returned on shutdown,
			// don't recover upon shutdown
			return
		}
	} else {
		// healthy sessions may contain pending confirmation messages
		// cleanup confirmations from previous session usage
		session.flushConfirms()
	}

	select {
	case <-sp.catchShutdown():
		session.Close()
	case sp.sessions <- session:
	}
}

func (sp *SessionPool) catchShutdown() <-chan struct{} {
	return sp.ctx.Done()
}

// Closes the session pool with all of its sessions
func (sp *SessionPool) Close() {

	wg := &sync.WaitGroup{}

	// close all sessions
SessionClose:
	for {
		select {
		// flush sessions channel
		case session := <-sp.sessions:
			wg.Add(1)
			go func(*Session) {
				defer wg.Done()
				session.Close()
			}(session)

		default:
			break SessionClose
		}
	}

	wg.Wait()
}

func (sp *SessionPool) initCachedSessions() error {
	for i := 0; i < sp.size; i++ {
		session, err := sp.initCachedSession(i)
		if err != nil {
			return err
		}
		sp.sessions <- session
	}
	return nil
}

// initCachedSession allows you create a pooled Session.
func (sp *SessionPool) initCachedSession(id int) (*Session, error) {

	// retry until we get a channel
	// or until shutdown
	for {
		conn, err := sp.pool.GetConnection()
		if err != nil {
			// error is only returned upon shutdown
			return nil, err
		}

		session, err := sp.deriveSession(conn, id, true)
		if err != nil {
			sp.pool.ReturnConnection(conn, true)
			continue
		}

		sp.pool.ReturnConnection(conn, false)
		return session, nil
	}
}

func (sp *SessionPool) deriveSession(conn *Connection, id int, cached bool) (*Session, error) {
	return NewSession(conn, int64(id),
		SessionWithContext(sp.ctx),
		SessionWithBufferSize(sp.size),
		SessionWithCached(cached),
		SessionWithConfirms(sp.confirmable),
	)
}
