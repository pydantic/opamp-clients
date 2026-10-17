// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Request heads as the client wrote them. net/http hands a handler the headers in a map, with
// the names in its own casing and their order lost; the checks want each name as sent, in order.
// So the server keeps what it reads from each connection and finds each request's head in it.
// Plain-text connections only: a TLS scenario logs no raw headers.

type rawListener struct{ net.Listener }

func (l rawListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &rawConn{Conn: c}, nil
}

// rawConn keeps what was read from it, from the start of the first request not yet found.
type rawConn struct {
	net.Conn
	mu     sync.Mutex
	buf    []byte
	halted bool
}

// rawKeep bounds what a connection keeps: a head is never this far behind.
const rawKeep = 4 << 20

func (c *rawConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		if !c.halted {
			c.buf = append(c.buf, p[:n]...)
			if len(c.buf) > rawKeep {
				c.buf = append([]byte(nil), c.buf[len(c.buf)-rawKeep:]...)
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

// halt stops keeping what is read: the connection is a WebSocket now.
func (c *rawConn) halt() {
	c.mu.Lock()
	c.halted, c.buf = true, nil
	c.mu.Unlock()
}

// head finds the head of r (its request line as sent) in what was read, drops what came before
// its end, and returns its header lines, name and value, in order. Requests on a connection are
// handled one after another, so the first head with r's request line is r's.
func (c *rawConn) head(r *http.Request) [][2]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := bytes.Index(c.buf, []byte(r.Method+" "+r.RequestURI+" HTTP/"))
	if i < 0 {
		return nil
	}
	end := bytes.Index(c.buf[i:], []byte("\r\n\r\n"))
	if end < 0 {
		return nil
	}
	lines := strings.Split(string(c.buf[i:i+end]), "\r\n")[1:]
	c.buf = append([]byte(nil), c.buf[i+end+4:]...)
	out := [][2]string{}
	for _, l := range lines {
		name, value, _ := strings.Cut(l, ":")
		value = strings.TrimSpace(value)
		if len(value) > 200 {
			value = value[:200] + "..."
		}
		out = append(out, [2]string{name, value})
	}
	return out
}

type rawConnKey struct{}
type rawHeadKey struct{}

// connContext hands each request its connection: a plain-text one for withRawHead, a TLS one
// (a scenario with TLS keeps no raw heads) for connTLS.
func connContext(ctx context.Context, c net.Conn) context.Context {
	switch c := c.(type) {
	case *rawConn:
		return context.WithValue(ctx, rawConnKey{}, c)
	case *countingConn:
		return context.WithValue(ctx, connKey{}, c)
	}
	return ctx
}

// withRawHead finds every request's head before any handler runs (so none is left behind to be
// taken for a later one), and sets the scenario's cookie on every response.
func (s *server) withRawHead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := r.Context().Value(rawConnKey{}).(*rawConn); ok {
			if h := rc.head(r); h != nil {
				r = r.WithContext(context.WithValue(r.Context(), rawHeadKey{}, h))
			}
		}
		if s.sc.Cookie {
			w.Header().Add("Set-Cookie", cookie)
		}
		next.ServeHTTP(w, r)
	})
}

// cookie is the one the server sets (scenario "cookie"): for every path, host-only.
const cookie = "opamp_conformance=set-by-server; Path=/"

// rawHeaders: r's header lines as sent, or nil if they weren't kept.
func rawHeaders(r *http.Request) any {
	if h, ok := r.Context().Value(rawHeadKey{}).([][2]string); ok {
		return h
	}
	return nil
}

// haltRaw stops keeping what r's connection reads (it was upgraded to a WebSocket).
func haltRaw(r *http.Request) {
	if rc, ok := r.Context().Value(rawConnKey{}).(*rawConn); ok {
		rc.halt()
	}
}
