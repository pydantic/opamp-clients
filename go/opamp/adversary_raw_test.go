// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// The adversary's raw server: what the
// streams of a Rust mode run connect to. A stream is bytes, not HTTP, so the adversary tells the
// server what to do in the first line it writes:
//
//	opamp-raw tag=7 send=1000 chunk=100 gap=5 end=fin
//
// The server answers "ok\n", then does what the line says, and keeps for each connection that
// said who it was (tag) how many bytes it read after the line, whether each was the pattern's
// (byte i is i % 251), and how the connection ended; the adversary asks for that on another
// stream (ask=TAG). One server per run, on a port of its own, so every connection is that run's,
// those that never say who they are too.

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// advRawLine is the longest first line the server reads.
const advRawLine = 256

// advRaw is one run's raw server.
type advRaw struct {
	ln       net.Listener
	tls      *tls.Config   // what tls=1 starts TLS with (nil: it can't)
	released chan struct{} // closed once the run's stop has returned: read=0 connections read on
	quit     chan struct{} // closed at teardown
	wg       sync.WaitGroup

	mu      sync.Mutex
	changed chan struct{}            // closed, and made anew, each time a connection says who it is or ends
	conns   map[*advRawConn]struct{} // every connection, but the harness's marker
	tags    map[uint64]*advRawConn   // those that said who they were
	marked  time.Time                // when the marker's connection was accepted
	onceRel sync.Once
}

// advRawConn is what the server knows of one connection.
type advRawConn struct {
	c        net.Conn
	accepted time.Time
	said     bool   // its first line came
	got      int64  // bytes read after the line
	bad      bool   // one of them wasn't the pattern's
	end      string // "": open; fin: the peer closed its side; reset: it failed; closed: the server ended it
}

// newAdvRaw starts a raw server on a port of its own.
func newAdvRaw(cfg *tls.Config) (*advRaw, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &advRaw{ln: ln, tls: cfg, released: make(chan struct{}), quit: make(chan struct{}),
		changed: make(chan struct{}), conns: map[*advRawConn]struct{}{}, tags: map[uint64]*advRawConn{}}
	s.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			rc := &advRawConn{c: c, accepted: time.Now()}
			s.wg.Go(func() { s.serve(rc) })
		}
	})
	return s, nil
}

func (s *advRaw) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// release lets the connections told not to read (read=0) read on: the run's stop has returned, and
// only a read tells the server a connection has gone.
func (s *advRaw) release() { s.onceRel.Do(func() { close(s.released) }) }

// close ends the server and every connection.
func (s *advRaw) close() {
	s.release()
	close(s.quit)
	_ = s.ln.Close()
	s.mu.Lock()
	for rc := range s.conns {
		_ = rc.c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// note changes rc under the lock, and wakes whoever waits for a connection to say who it is or end.
func (s *advRaw) note(change func()) {
	s.mu.Lock()
	change()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// open: the connections still open, and how many of them never said who they were.
func (s *advRaw) open() (all, anonymous int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for rc := range s.conns {
		if rc.end == "" {
			all++
			if !rc.said {
				anonymous++
			}
		}
	}
	return all, anonymous
}

// afterMark: what breaks RULE-SHELL-07 in when the run's connections were accepted ("": nothing
// does): one accepted more than advArrivalGrace after the marker's, which the harness opened the
// moment stop returned. (The server accepts in the order the connections were made.)
func (s *advRaw) afterMark() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.marked.IsZero() {
		return "the raw server never got the harness's marker"
	}
	for rc := range s.conns {
		if late := rc.accepted.Sub(s.marked); late > advArrivalGrace {
			return fmt.Sprintf("RULE-SHELL-07: a stream's connection reached the raw server %v after one the harness made as stop returned",
				late.Round(time.Millisecond))
		}
	}
	return ""
}

// advRawMark opens the harness's marker on the raw server at addr, as stop returns: the channel
// says how it went (nil: the server said ok).
func advRawMark(addr string) <-chan error {
	done := make(chan error, 1)
	c, err := net.DialTimeout("tcp", addr, advMarkBound)
	if err != nil {
		done <- err
		return done
	}
	go func() {
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(advMarkBound))
		if _, err := io.WriteString(c, "opamp-raw mark=1\n"); err != nil {
			done <- err
			return
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err == nil && line != "ok\n" {
			err = fmt.Errorf("the raw server answered the marker %q", line)
		}
		done <- err
	}()
	return done
}

// bufConn reads through r what a bufio.Reader took from the connection with the first line.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// sleep waits ms; false if the server is torn down first.
func (s *advRaw) sleep(ms int64) bool {
	if ms <= 0 {
		return true
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.quit:
		return false
	}
}

func (s *advRaw) serve(rc *advRawConn) {
	c := rc.c
	defer func() { _ = c.Close() }()
	// Its run's from the moment it is accepted, whether or not it ever says who it is.
	s.note(func() { s.conns[rc] = struct{}{} })
	br := bufio.NewReaderSize(c, 64<<10)
	line, err := advRawFirstLine(br)
	set := map[string]string{}
	if err == nil {
		rest, ok := strings.CutPrefix(line, "opamp-raw ")
		if !ok {
			err = errors.New("not the adversary's")
		}
		for _, kv := range strings.Fields(rest) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				set[k] = v
			}
		}
	}
	num := func(name string, def int64) int64 {
		if n, perr := strconv.ParseInt(set[name], 10, 64); perr == nil {
			return n
		}
		return def
	}
	if err == nil && num("mark", 0) == 1 {
		// The harness's own, not the run's: when what was opened as stop returned got here.
		s.note(func() {
			delete(s.conns, rc)
			s.marked = rc.accepted
		})
		_, _ = io.WriteString(c, "ok\n")
		return
	}
	ended := func(how string) {
		s.note(func() {
			if rc.end == "" {
				rc.end = how
			}
		})
	}
	if err != nil {
		// It went, or said something else, before it said who it was.
		ended("closed")
		return
	}
	tag := uint64(num("tag", 0))
	s.note(func() {
		rc.said = true
		s.tags[tag] = rc
	})

	var rw io.ReadWriter = bufConn{c, br}
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := rw.Write(b)
		return err
	}
	delay := num("delay", 0)
	held := delay < 0 // never answered: only read, until it goes
	if !held {
		if !s.sleep(delay) || write([]byte("ok\n")) != nil {
			ended("closed")
			return
		}
		if num("tls", 0) == 1 {
			if s.tls == nil {
				ended("closed")
				return
			}
			tc := tls.Server(bufConn{c, br}, s.tls)
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			if err := tc.Handshake(); err != nil {
				ended("closed")
				return
			}
			_ = c.SetDeadline(time.Time{})
			rw = tc
			defer func() { _ = tc.Close() }()
		}
	}

	// What the line asks the server to send, then how it ends the stream, beside the reading.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		if held {
			return
		}
		if about, ok := set["ask"]; ok {
			t, _ := strconv.ParseUint(about, 10, 64)
			if write([]byte(s.answer(t, num("wait", 0)))) != nil {
				return
			}
		}
		body := advBody(max(num("send", 0), 0))
		chunk := int(num("chunk", 0))
		if chunk <= 0 {
			chunk = max(len(body), 1)
		}
		for off := 0; off < len(body); off += chunk {
			if off > 0 && !s.sleep(num("gap", 0)) {
				return
			}
			if write(body[off:min(off+chunk, len(body))]) != nil {
				return
			}
		}
		end := set["end"]
		if end == "" || end == "hold" {
			return
		}
		if !s.sleep(num("after", 0)) {
			return
		}
		switch end {
		case "fin":
			// Its side only: it still reads.
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		case "close":
			ended("closed")
			if tc, ok := rw.(*tls.Conn); ok {
				_ = tc.Close()
			}
			_ = c.Close()
		case "reset":
			ended("closed")
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = c.Close()
		}
	}()

	if num("read", 1) == 0 {
		// Reads nothing until the run is over.
		select {
		case <-s.released:
		case <-s.quit:
		}
	}
	echo := num("echo", 0) == 1
	buf := make([]byte, 64<<10)
	for {
		n, err := rw.Read(buf)
		if n > 0 {
			s.mu.Lock()
			for i, b := range buf[:n] {
				if b != byte((rc.got+int64(i))%251) {
					rc.bad = true
				}
			}
			rc.got += int64(n)
			s.mu.Unlock()
			if echo && write(buf[:n]) != nil {
				err = io.ErrClosedPipe
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				ended("fin")
			} else {
				ended("reset")
			}
			break
		}
	}
	_ = c.Close()
	<-sent
}

// answer waits until connection tag has ended (tag 0: until no connection that never said who it
// was is open), wait ms at most, and says what the server knows of it: "STATE GOT BAD\n".
func (s *advRaw) answer(tag uint64, wait int64) string {
	deadline := time.NewTimer(time.Duration(wait) * time.Millisecond)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		state, got, bad := "none", int64(0), 0
		if tag == 0 {
			for rc := range s.conns {
				if rc.end == "" && !rc.said {
					state = "open"
					got++
				}
			}
		} else if rc := s.tags[tag]; rc != nil {
			state, got = rc.end, rc.got
			if state == "" {
				state = "open"
			}
			if rc.bad {
				bad = 1
			}
		}
		changed := s.changed
		s.mu.Unlock()
		answer := fmt.Sprintf("%s %d %d\n", state, got, bad)
		if state != "open" {
			return answer
		}
		select {
		case <-changed:
		case <-deadline.C:
			return answer
		case <-s.quit:
			return answer
		}
	}
}

// advRawFirstLine reads the first line, without its newline: advRawLine bytes at most.
func advRawFirstLine(br *bufio.Reader) (string, error) {
	var line []byte
	for len(line) < advRawLine {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return string(line), nil
		}
		line = append(line, b)
	}
	return "", errors.New("no line")
}

// The raw server does what its first line says, and tells what it saw of a connection.
func TestAdversaryRawServer(t *testing.T) {
	ca := newTestCA(t, "raw CA")
	_, _, pair := ca.issue(t, "server", true)
	s, err := newAdvRaw(&tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	addr := s.ln.Addr().String()
	dial := func(line string) (net.Conn, *bufio.Reader) {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := io.WriteString(c, line+"\n"); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(c)
		if ok, err := br.ReadString('\n'); err != nil || ok != "ok\n" {
			t.Fatalf("%q: answered %q, %v", line, ok, err)
		}
		return c, br
	}
	ask := func(tag int) string {
		t.Helper()
		_, br := dial(fmt.Sprintf("opamp-raw tag=%d ask=%d wait=5000", 1000+tag, tag))
		answer, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return answer
	}

	// send, in pieces, then its side closed: the pattern, then the end.
	_, br := dial("opamp-raw tag=1 send=1000 chunk=300 gap=1 end=fin")
	body, err := io.ReadAll(br)
	if err != nil || string(body) != string(advBody(1000)) {
		t.Fatalf("send=1000: %d bytes, %v", len(body), err)
	}

	// echo, and what it read: all of the pattern, then the peer's end.
	c, br := dial("opamp-raw tag=2 echo=1")
	if _, err := c.Write(advBody(5000)); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, 5000)
	if _, err := io.ReadFull(br, back); err != nil || string(back) != string(advBody(5000)) {
		t.Fatalf("echo: %v", err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	if got := ask(2); got != "fin 5000 0\n" {
		t.Errorf("after the pattern and a close: %q", got)
	}

	// Bytes that aren't the pattern's, and a reset.
	c, _ = dial("opamp-raw tag=3")
	_, _ = c.Write([]byte{0, 1, 9})
	time.Sleep(100 * time.Millisecond)
	_ = c.(*net.TCPConn).SetLinger(0)
	_ = c.Close()
	if got := ask(3); got != "reset 3 1\n" {
		t.Errorf("after three bytes, one wrong, and a reset: %q", got)
	}

	// One still open is said to be, once the wait is over; one never seen is none.
	dial("opamp-raw tag=4")
	_, br = dial("opamp-raw tag=1004 ask=4 wait=200")
	if got, _ := br.ReadString('\n'); got != "open 0 0\n" {
		t.Errorf("a connection still open: %q", got)
	}
	if got := ask(99); got != "none 0 0\n" {
		t.Errorf("a connection never made: %q", got)
	}

	// A connection that never says who it is counts until it goes.
	anon, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, br = dial("opamp-raw tag=1005 ask=0 wait=300")
	if got, _ := br.ReadString('\n'); got != "open 1 0\n" {
		t.Errorf("with a connection that never said who it was: %q", got)
	}
	_ = anon.Close()
	if got := ask(0); got != "none 0 0\n" {
		t.Errorf("once it has gone: %q", got)
	}

	// TLS after ok, under the certificate's name; then the pattern inside it, and a close.
	c, _ = dial("opamp-raw tag=6 tls=1 send=100 end=close")
	tc := tls.Client(c, &tls.Config{RootCAs: ca.pool, ServerName: "localhost"})
	if body, err := io.ReadAll(tc); err != nil || string(body) != string(advBody(100)) {
		t.Fatalf("tls=1 send=100: %d bytes, %v", len(body), err)
	}

	// read=0 reads nothing until the run is released; delay=-1 never answers; reset is one.
	c, _ = dial("opamp-raw tag=7 read=0")
	_, _ = c.Write(advBody(10))
	_ = c.(*net.TCPConn).CloseWrite()
	_, br = dial("opamp-raw tag=1007 ask=7 wait=200")
	if got, _ := br.ReadString('\n'); got != "open 0 0\n" {
		t.Errorf("read=0 before the release: %q", got)
	}
	s.release()
	if got := ask(7); got != "fin 10 0\n" {
		t.Errorf("read=0 after the release: %q", got)
	}
	c, _ = dial("opamp-raw tag=8 end=reset after=50")
	if _, err := io.ReadAll(c); err == nil {
		t.Error("end=reset ended the stream cleanly")
	}
	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	_, _ = io.WriteString(silent, "opamp-raw tag=9 delay=-1\n")
	_ = silent.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var ne net.Error
	if n, err := silent.Read(make([]byte, 1)); n != 0 || !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("delay=-1 answered (%d bytes, %v)", n, err)
	}

	// The marker: a connection accepted after it is late, one before it isn't.
	if said := s.afterMark(); !strings.Contains(said, "never got") {
		t.Errorf("before the marker: %q", said)
	}
	if err := <-advRawMark(addr); err != nil {
		t.Fatal(err)
	}
	if said := s.afterMark(); said != "" {
		t.Errorf("nothing came after the marker: %q", said)
	}
	time.Sleep(2 * advArrivalGrace)
	dial("opamp-raw tag=10")
	if said := s.afterMark(); !strings.Contains(said, "RULE-SHELL-07") {
		t.Errorf("a connection made after the marker passed: %q", said)
	}
}
