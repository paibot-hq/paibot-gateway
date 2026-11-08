package pg

import (
	"context"
	"errors"
	"sync"
)

// Pool keeps up to max connections open and lends them out one at a time.
// Broken connections are dropped, and a new one is opened on demand.
// Nothing is retried automatically: a failed write may or may not have
// been applied, and only the caller knows whether repeating it is safe.
type Pool struct {
	cfg   *Config
	slots chan struct{} // one token per open connection
	idle  chan *Conn

	mu     sync.Mutex
	closed bool
}

var ErrPoolClosed = errors.New("pg: pool is closed")

func NewPool(cfg *Config, max int) *Pool {
	if max < 1 {
		max = 1
	}
	return &Pool{cfg: cfg, slots: make(chan struct{}, max), idle: make(chan *Conn, max)}
}

// Acquire returns an idle connection, opens a new one if the pool has room,
// or waits for one to be released.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	if p.isClosed() {
		return nil, ErrPoolClosed
	}
	select {
	case c := <-p.idle:
		return c, nil
	default:
	}
	select {
	case c := <-p.idle:
		return c, nil
	case p.slots <- struct{}{}:
		c, err := Connect(ctx, p.cfg)
		if err != nil {
			<-p.slots
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Release hands a connection back. Broken ones are closed and their slot
// freed.
func (p *Pool) Release(c *Conn) {
	if c.broken || p.isClosed() {
		c.Close()
		<-p.slots
		return
	}
	p.idle <- c // never blocks: idle has room for every slot
}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	c, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer p.Release(c)
	return c.Query(ctx, sql, args...)
}

func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (string, error) {
	c, err := p.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer p.Release(c)
	return c.Exec(ctx, sql, args...)
}

// Close closes idle connections now, and lent ones as they come back.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	for {
		select {
		case c := <-p.idle:
			c.Close()
			<-p.slots
		default:
			return
		}
	}
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
