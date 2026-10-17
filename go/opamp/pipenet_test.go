// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// An in-memory network for testing/synctest bubbles: a socket on loopback isn't "durably
// blocking", so a bubble never goes idle while a goroutine waits on one; net.Pipe is (it is
// channels). A pipeNet hands one end of a fresh net.Pipe to each dial and the other to its
// listener's Accept, and counts the dialled (client) ends still open, so a test can say that
// nothing the client opened is still open after Stop.
//
// net.Pipe is synchronous: a write waits until the other side reads it all. Two sides writing at
// once (a server pushing a message while the client sends its Close) would each wait for the
// other, where a socket's buffers take both. So each end writes through a buffer of its own,
// emptied by a goroutine: a write never blocks, as on a socket, and a close sends what is
// buffered first (as a socket's close does), then closes.
//
// The client reaches it through Config.HTTPClient: its *http.Transport's DialContext carries
// OpAMP over HTTP and package downloads, and the WebSocket dialer takes the same DialContext
// (connectionFrom). Nothing in the client changes.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeNet is the network: every address dials its one listener.
type pipeNet struct {
	mu      sync.Mutex
	accept  chan net.Conn
	closed  chan struct{}
	once    sync.Once
	dialled int // client ends made
	open    map[*pipeEnd]bool
	ends    []*pipeEnd // every end, closed at shutdown
}

func newPipeNet() *pipeNet {
	return &pipeNet{accept: make(chan net.Conn), closed: make(chan struct{}), open: map[*pipeEnd]bool{}}
}

// pipeEnd is one end of a pipe; the client's ends are counted while open.
type pipeEnd struct {
	net.Conn // the net.Pipe end: reads, addresses, read deadlines
	n        *pipeNet
	peer     *pipeEnd // the other end
	client   bool
	closed   atomic.Bool

	mu      sync.Mutex
	buf     [][]byte      // written, not yet through the pipe
	wake    chan struct{} // holds a token when buf or closing changed
	err     error         // why the pipe took no more (the other end closed)
	closing bool
}

func newPipeEnd(n *pipeNet, c net.Conn, client bool) *pipeEnd {
	e := &pipeEnd{Conn: c, n: n, client: client, wake: make(chan struct{}, 1)}
	go e.pump()
	return e
}

func (e *pipeEnd) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// pump passes what is written through the pipe, in order; once closing and empty, it closes it.
func (e *pipeEnd) pump() {
	for {
		e.mu.Lock()
		if len(e.buf) == 0 || e.err != nil {
			closing := e.closing
			e.mu.Unlock()
			if closing {
				_ = e.Conn.Close()
				return
			}
			<-e.wake
			continue
		}
		chunk := e.buf[0]
		e.buf = e.buf[1:]
		e.mu.Unlock()
		if _, err := e.Conn.Write(chunk); err != nil {
			e.mu.Lock()
			e.err, e.buf = err, nil
			e.mu.Unlock()
		}
	}
}

// Write buffers b: it never blocks.
func (e *pipeEnd) Write(b []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closing:
		return 0, net.ErrClosed
	case e.err != nil:
		return 0, e.err
	}
	e.buf = append(e.buf, bytes.Clone(b))
	e.signal()
	return len(b), nil
}

// Close ends this end at once for its owner (a read waiting returns), and closes the pipe once
// what was written has gone through (or the other end has gone).
func (e *pipeEnd) Close() error {
	// Marked closed before any Close returns: a connection has two closers (its goroutine's, and
	// its cancel's), and the one that comes second returns at once.
	e.mu.Lock()
	already := e.closing
	if !already {
		e.closing = true
		e.closed.Store(true)
		e.n.mu.Lock()
		delete(e.n.open, e)
		e.n.mu.Unlock()
	}
	e.mu.Unlock()
	if already {
		return net.ErrClosed
	}
	_ = e.Conn.SetReadDeadline(time.Unix(1, 0))
	e.signal()
	return nil
}

// abort closes the pipe now, whatever is buffered (teardown).
func (e *pipeEnd) abort() {
	_ = e.Close()
	_ = e.Conn.Close()
}

func (e *pipeEnd) SetDeadline(t time.Time) error { return e.Conn.SetReadDeadline(t) }

func (e *pipeEnd) SetWriteDeadline(time.Time) error { return nil } // writes never block

// pipeKey is the context key under which a pipeNet's server finds the *pipeEnd a request came on
// (http.Server.ConnContext: pipeConnContext).
type pipeKey struct{}

func pipeConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, pipeKey{}, c)
}

// pipeOf is the server's end of the connection r came on (nil if not a pipe).
func pipeOf(r *http.Request) *pipeEnd {
	e, _ := r.Context().Value(pipeKey{}).(*pipeEnd)
	return e
}

// DialContext connects to the listener (whatever the address), as a net.Dialer would.
func (n *pipeNet) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	c, s := net.Pipe()
	ce, se := newPipeEnd(n, c, true), newPipeEnd(n, s, false)
	ce.peer, se.peer = se, ce
	n.mu.Lock()
	n.dialled++
	n.open[ce] = true
	n.ends = append(n.ends, ce, se)
	n.mu.Unlock()
	select {
	case n.accept <- se:
		return ce, nil
	case <-ctx.Done():
		ce.abort()
		se.abort()
		return nil, ctx.Err()
	case <-n.closed:
		ce.abort()
		se.abort()
		return nil, errors.New("pipenet: connection refused")
	}
}

// openClientEnds is how many dialled ends are still open.
func (n *pipeNet) openClientEnds() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.open)
}

func (n *pipeNet) dials() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dialled
}

// transport is an *http.Transport over the network, as an application would pass in
// Config.HTTPClient (it owns it: CloseIdleConnections is the application's).
func (n *pipeNet) transport() *http.Transport {
	return &http.Transport{DialContext: n.DialContext, MaxIdleConns: 10}
}

// Listener.

func (n *pipeNet) Accept() (net.Conn, error) {
	select {
	case c := <-n.accept:
		return c, nil
	case <-n.closed:
		return nil, net.ErrClosed
	}
}

func (n *pipeNet) Close() error {
	n.once.Do(func() { close(n.closed) })
	return nil
}

func (n *pipeNet) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// shutdown closes the listener and every end, whatever is buffered (hijacked WebSocket
// connections included, and any a client left open), so that every goroutine on them ends.
func (n *pipeNet) shutdown() {
	_ = n.Close()
	n.mu.Lock()
	ends := n.ends
	n.ends = nil
	n.mu.Unlock()
	for _, e := range ends {
		e.abort()
	}
}

// The harness's own measure (TestSchedules once said a WebSocket connection was still open at the
// client when Stop returned: its two closers, the cancel's and the connection's goroutine's, had
// both been called, and the one that came second returned before the first had marked the end
// closed): once any Close of an end has returned, the end counts as closed.
//
// covers: RULE-SHELL-07
func TestPipeEndIsClosedOnceAnyCloseReturns(t *testing.T) {
	n := newPipeNet()
	defer n.shutdown()
	var early atomic.Int64
	for range 20000 {
		c, s := net.Pipe()
		e := newPipeEnd(n, c, true)
		n.mu.Lock()
		n.open[e] = true
		n.mu.Unlock()
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = e.Close()
				n.mu.Lock()
				open := n.open[e]
				n.mu.Unlock()
				if !e.closed.Load() || open {
					early.Add(1)
				}
			}()
		}
		wg.Wait()
		_ = s.Close()
	}
	if k := early.Load(); k > 0 {
		t.Errorf("%d times of 20000, a Close returned before the end counted as closed", k)
	}
}
