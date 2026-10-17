// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// Rust mode's executor (streams.go) on its own, the test playing the loop: it carries out stream
// effects as the core would hand them over, and takes each report as the loop does (deliver, then
// the step's effects, then settled). Its register: a close, a cancel and stop end what they name
// at once; a connect, a TLS handshake or a resolution that completes after its close is closed at
// once and reported to no one; stop is bounded. Backpressure both ways, splices, and download
// files: made new, deleted on discard, cancel and failure, durable before downloaded, and every
// operation in the file journal.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// streamRig is a Rust-mode executor with the test as its loop.
type streamRig struct {
	t     *testing.T
	x     *executor
	s     *streams
	state string

	mu      sync.Mutex
	journal []map[string]any
}

func newStreamRig(t *testing.T, p *streamPlatform, fsys fileSystem) *streamRig {
	t.Helper()
	r := &streamRig{t: t, state: filepath.Join(t.TempDir(), "state")}
	r.x = newExecutor(&connection{})
	r.x.setStateFile(r.state)
	if fsys == nil {
		fsys = osFS{}
	}
	r.x.fs = journaled(fsys, r.state, func(op []byte) {
		var m map[string]any
		_ = json.Unmarshal(op, &m)
		r.mu.Lock()
		r.journal = append(r.journal, m)
		r.mu.Unlock()
	})
	r.s = newStreams(r.x, &tls.Config{}, newRustProxies(Proxy{Direct: true}, nil), p)
	r.x.streams = r.s
	t.Cleanup(func() {
		if !r.x.fl.closed {
			r.x.shutdown(5 * time.Second)
		}
	})
	return r
}

// do carries out effects as the loop does, in order (rep: the report whose step they are, or
// nil): the outcomes known at once.
func (r *streamRig) do(rep *report, effs ...*corepb.Effect) []report {
	if rep != nil {
		r.s.reading = rep.src
	}
	var back []report
	for _, e := range effs {
		if b := r.x.do(e); b != nil {
			back = append(back, *b)
		}
	}
	r.s.reading = nil
	if rep != nil {
		rep.settled()
	}
	return back
}

// next is the next report that reaches the core (dropping, and settling, any that doesn't). The
// caller settles it, with do.
func (r *streamRig) next() report {
	r.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case rep := <-r.x.reports():
			if r.x.deliver(rep) {
				return rep
			}
			rep.settled()
		case <-timeout:
			r.t.Fatal("no report")
		}
	}
}

// take is the next report, settled with no effects.
func (r *streamRig) take() *corepb.Input {
	r.t.Helper()
	rep := r.next()
	r.do(&rep)
	return rep.in
}

// quiet checks that nothing reaches the core for d.
func (r *streamRig) quiet(d time.Duration) {
	r.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case rep := <-r.x.reports():
			if r.x.deliver(rep) {
				r.t.Fatalf("reported: %v", rep.in)
			}
			rep.settled()
		case <-deadline:
			return
		}
	}
}

func (r *streamRig) ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.journal {
		out = append(out, m["op"].(string)+" "+m["path"].(string))
	}
	return out
}

func sOpen(id uint64, host string, port int) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamOpen{StreamOpen: &corepb.StreamOpen{Id: id, Host: host, Port: uint32(port)}}}
}

func sWrite(id uint64, data []byte) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamWrite{StreamWrite: &corepb.WsMessage{Id: id, Data: data}}}
}

func sClose(id uint64, abort bool) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamClose{StreamClose: &corepb.StreamClose{Id: id, Abort: abort}}}
}

func sTLS(id uint64, host string) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamStartTls{StreamStartTls: &corepb.StreamName{Id: id, Host: host}}}
}

func sResolve(id uint64, host string) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamResolve{StreamResolve: &corepb.StreamName{Id: id, Host: host}}}
}

func sSplice(id, download, n uint64) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_StreamSplice{StreamSplice: &corepb.StreamSplice{Id: id, Download: download, Bytes: n}}}
}

func dData(id uint64, data []byte) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_DownloadData{DownloadData: &corepb.WsMessage{Id: id, Data: data}}}
}

func dDone(id uint64) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_DownloadDone{DownloadDone: &corepb.Id{Id: id}}}
}

func discardEffect(id uint64) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_Discard{Discard: &corepb.Id{Id: id}}}
}

// pipeDialer hands out the client ends of net.Pipe (synchronous: a write waits for the peer to
// read), the server ends on its channel; a dial waits for release when gated.
type pipeDialer struct {
	servers chan net.Conn
	gate    chan struct{} // nil: dial at once
	dialled atomic.Int32
	mu      sync.Mutex
	clients []net.Conn
}

func newPipeDialer() *pipeDialer { return &pipeDialer{servers: make(chan net.Conn, 16)} }

func (d *pipeDialer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	if d.gate != nil {
		<-d.gate // a dial that completes whatever its context says, late
	}
	c, s := net.Pipe()
	d.dialled.Add(1)
	d.mu.Lock()
	d.clients = append(d.clients, c)
	d.mu.Unlock()
	d.servers <- s
	return c, nil
}

func (d *pipeDialer) server(t *testing.T) net.Conn {
	t.Helper()
	select {
	case s := <-d.servers:
		t.Cleanup(func() { s.Close() })
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no connection")
		return nil
	}
}

// closedSoon says whether c's peer has closed it, within a while (a read gets EOF or an error).
func closedSoon(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64<<10)
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}

// A stream to a real server: opened, written, read, and the peer's end then its gone; data goes
// in order, and what the core wrote before the stream opened goes once it has.
func TestStreamsCarryBytesBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b := make([]byte, 11)
		_, _ = io.ReadFull(c, b)
		got <- string(b)
		_, _ = c.Write([]byte("world"))
		c.Close()
	}()
	r := newStreamRig(t, nil, nil)
	port := ln.Addr().(*net.TCPAddr).Port
	r.do(nil, sOpen(1, "127.0.0.1", port), sWrite(1, []byte("hello ")))
	if in := r.take(); in.GetStreamOpened().GetId() != 1 {
		t.Fatalf("got %v", in)
	}
	r.do(nil, sWrite(1, []byte("there")))
	if s := <-got; s != "hello there" {
		t.Fatalf("the server got %q", s)
	}
	var data []byte
	for {
		in := r.take()
		if d := in.GetStreamData(); d != nil {
			data = append(data, d.Data...)
			continue
		}
		if in.GetStreamEof() == nil {
			t.Fatalf("got %v", in)
		}
		break
	}
	if string(data) != "world" {
		t.Fatalf("read %q", data)
	}
	// Then gone, as on Node's and Python's sockets (unless the core closed it on the end).
	if in := r.take(); in.GetStreamFailed().GetId() != 1 {
		t.Fatalf("got %v", in)
	}
	r.quiet(100 * time.Millisecond)
}

// A connect that completes after its stream was closed: the connection is closed at once, and
// nothing is reported. The same at stop.
func TestStreamsAConnectAfterItsCloseIsClosed(t *testing.T) {
	for _, how := range []string{"close", "abort", "stop"} {
		t.Run(how, func(t *testing.T) {
			d := newPipeDialer()
			d.gate = make(chan struct{})
			r := newStreamRig(t, &streamPlatform{dial: d.dial}, nil)
			r.do(nil, sOpen(1, "example.com", 80), sWrite(1, []byte("GET / HTTP/1.1\r\n\r\n")))
			switch how {
			case "stop":
				done := make(chan struct{})
				go func() { r.x.shutdown(5 * time.Second); close(done) }()
				time.Sleep(50 * time.Millisecond)
				close(d.gate)
				within(t, "stop", done)
			default:
				r.do(nil, sClose(1, how == "abort"))
				close(d.gate)
			}
			srv := d.server(t)
			if !closedSoon(srv) {
				t.Fatal("the late connection is still open")
			}
			if how != "stop" {
				r.quiet(100 * time.Millisecond)
			}
		})
	}
}

// A TLS handshake that completes after its stream was closed: closed at once, unreported.
func TestStreamsATLSHandshakeAfterItsCloseIsClosed(t *testing.T) {
	d := newPipeDialer()
	began, release := make(chan struct{}), make(chan struct{})
	var handed net.Conn
	r := newStreamRig(t, &streamPlatform{dial: d.dial, startTLS: func(_ context.Context, c net.Conn, cfg *tls.Config) (net.Conn, error) {
		if cfg.ServerName != "example.com" {
			t.Errorf("TLS for %q", cfg.ServerName)
		}
		handed = c
		close(began)
		<-release // a handshake that completes whatever its context says, late
		return c, nil
	}}, nil)
	r.do(nil, sOpen(1, "example.com", 443))
	srv := d.server(t)
	opened := r.next()
	r.do(&opened, sTLS(1, "example.com"))
	within(t, "the handshake", began)
	r.do(nil, sClose(1, false))
	close(release)
	if !closedSoon(srv) {
		t.Fatal("the connection is still open")
	}
	r.quiet(100 * time.Millisecond)
	_ = handed
}

// A resolution that completes after its close (or stop) is reported to no one.
func TestStreamsAResolutionAfterItsCloseIsUnreported(t *testing.T) {
	release := make(chan struct{})
	var asked atomic.Int32
	r := newStreamRig(t, &streamPlatform{resolve: func(ctx context.Context, host string) ([]string, error) {
		asked.Add(1)
		<-release
		return []string{"192.0.2.1"}, nil
	}}, nil)
	r.do(nil, sResolve(3, "a.example"), sResolve(4, "b.example"))
	r.do(nil, sClose(3, true))
	close(release)
	in := r.take()
	if in.GetStreamResolved().GetId() != 4 || in.GetStreamResolved().Addresses[0] != "192.0.2.1" {
		t.Fatalf("got %v", in)
	}
	r.quiet(100 * time.Millisecond)
	// And a real one: an address resolves to itself.
	r2 := newStreamRig(t, nil, nil)
	r2.do(nil, sResolve(5, "127.0.0.1"))
	if in := r2.take(); in.GetStreamResolved().GetAddresses()[0] != "127.0.0.1" {
		t.Fatalf("got %v", in)
	}
}

// Stop ends everything at once (streams connecting, open, closing and lingering, resolutions),
// reports nothing after, and is bounded even by a connect that never ends.
func TestStreamsStopEndsEverythingBounded(t *testing.T) {
	d := newPipeDialer()
	stuck := make(chan struct{})
	defer close(stuck)
	r := newStreamRig(t, &streamPlatform{
		dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			if a == "stuck:1" {
				<-stuck // ignores its context
				return nil, errors.New("never")
			}
			return d.dial(ctx, n, a)
		},
		resolve: func(ctx context.Context, host string) ([]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}, nil)
	r.do(nil, sOpen(1, "a", 1), sOpen(2, "b", 1), sOpen(3, "stuck", 1), sResolve(4, "x"))
	s1, s2 := d.server(t), d.server(t)
	for range 2 {
		rep := r.next()
		r.do(&rep)
	}
	// Stream 2 closed with what was written still going (the peer reads nothing): it lingers.
	r.do(nil, sWrite(2, bytes.Repeat([]byte("x"), 1024)), sClose(2, false))
	began := time.Now()
	r.x.shutdown(300 * time.Millisecond)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("stop took %v", took)
	}
	if !closedSoon(s1) || !closedSoon(s2) {
		t.Fatal("a stream is still open after stop")
	}
	select {
	case rep := <-r.x.reports():
		t.Fatalf("reported after stop: %v", rep.in)
	default:
	}
}

// Core to socket: while too much written is unsent, the stream reads nothing (the peer's ping waits
// in the kernel, here the pipe); once it has gone, reading goes on. Past maxUnsent, the next write
// fails the stream.
func TestStreamsHoldReadingWhileWritesAreUnsent(t *testing.T) {
	d := newPipeDialer()
	r := newStreamRig(t, &streamPlatform{dial: d.dial}, nil)
	r.do(nil, sOpen(1, "a", 1))
	srv := d.server(t)
	r.take()
	r.do(nil, sWrite(1, bytes.Repeat([]byte("w"), sendHighWater+1)))
	// A read already under way may take the ping; nothing reads the pong.
	pinged, ponged := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = srv.Write([]byte("ping"))
		close(pinged)
		_, _ = srv.Write([]byte("pong"))
		close(ponged)
	}()
	var read []byte
	deadline := time.After(300 * time.Millisecond)
collect:
	for {
		select {
		case rep := <-r.x.reports():
			if r.x.deliver(rep) {
				read = append(read, rep.in.GetStreamData().GetData()...)
			}
			rep.settled()
		case <-deadline:
			break collect
		}
	}
	select {
	case <-ponged:
		t.Fatalf("the stream read while its writes were unsent (read %q)", read)
	default:
	}
	// The peer reads: what was written goes, and the rest is read.
	go func() { _, _ = io.CopyN(io.Discard, srv, sendHighWater+1) }()
	for len(read) < len("pingpong") {
		read = append(read, r.take().GetStreamData().GetData()...)
	}
	if string(read) != "pingpong" {
		t.Fatalf("read %q", read)
	}
	// A peer that reads nothing more: past maxUnsent, the next write fails the stream.
	r.do(nil, sWrite(1, bytes.Repeat([]byte("w"), maxUnsent+1)))
	back := r.do(nil, sWrite(1, []byte("more")))
	if len(back) != 1 || back[0].in.GetStreamFailed().GetId() != 1 || !strings.Contains(back[0].in.GetStreamFailed().Detail, "has not taken") {
		t.Fatalf("got %v", back)
	}
	if !r.x.deliver(back[0]) {
		t.Fatal("the failure didn't reach the core")
	}
	if !closedSoon(srv) {
		t.Fatal("the stream is still open")
	}
	r.quiet(100 * time.Millisecond)
}

// A close (not an abort) sends everything written, then ends; an abort resets at once.
func TestStreamsCloseFlushesAbortResets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	r := newStreamRig(t, nil, nil)
	payload := bytes.Repeat([]byte("0123456789"), 300_000)
	for i, abort := range []bool{false, true} {
		id := uint64(i + 1)
		r.do(nil, sOpen(id, "127.0.0.1", port))
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		r.take()
		r.do(nil, sWrite(id, payload), sClose(id, abort))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, err := io.ReadAll(c)
		c.Close()
		if !abort && (err != nil || !bytes.Equal(got, payload)) {
			t.Fatalf("closed: the server read %d of %d bytes (%v)", len(got), len(payload), err)
		}
		if abort && err == nil && len(got) == len(payload) {
			t.Fatalf("aborted: everything went, and the stream ended cleanly")
		}
	}
	r.quiet(100 * time.Millisecond)
}

// slowFS holds each write to a download's file until let through.
type slowFS struct {
	osFS
	pass chan struct{} // a token per write; nil: no hold
	fail atomic.Bool   // writes fail
}

func (f *slowFS) createNew(path string) (writeFile, error) {
	w, err := f.osFS.createNew(path)
	if err != nil {
		return nil, err
	}
	return &slowFile{w, f}, nil
}

type slowFile struct {
	writeFile
	fs *slowFS
}

func (w *slowFile) Write(b []byte) (int, error) {
	if w.fs.pass != nil {
		<-w.fs.pass
	}
	if w.fs.fail.Load() {
		return 0, errors.New("disk on fire")
	}
	return w.writeFile.Write(b)
}

// Splice: the next n bytes go to the file, exactly (what follows comes as data), then
// stream_spliced; the download ends synced, with the digest of what was written.
func TestStreamsSpliceThenDownloaded(t *testing.T) {
	d := newPipeDialer()
	r := newStreamRig(t, &streamPlatform{dial: d.dial}, nil)
	r.do(nil, sOpen(1, "a", 1))
	srv := d.server(t)
	r.take()
	go func() { _, _ = srv.Write([]byte("HEAD")) }()
	head := r.next()
	if string(head.in.GetStreamData().GetData()) != "HEAD" {
		t.Fatalf("got %v", head.in)
	}
	// The core's answer: what it has of the body so far, then the rest spliced.
	r.do(&head, dData(9, []byte("bo")), sSplice(1, 9, 9))
	go func() { _, _ = srv.Write([]byte("dy-is-tenTAIL")) }()
	if in := r.take(); in.GetStreamSpliced().GetId() != 1 {
		t.Fatalf("got %v", in)
	}
	if in := r.take(); string(in.GetStreamData().GetData()) != "TAIL" {
		t.Fatalf("got %v", in)
	}
	r.do(nil, dDone(9))
	in := r.take()
	want := sha256.Sum256([]byte("body-is-ten"))
	if in.GetDownloaded().GetId() != 9 || !bytes.Equal(in.GetDownloaded().Sha256, want[:]) {
		t.Fatalf("got %v", in)
	}
	path := r.x.downloadPath(9)
	if b, err := os.ReadFile(path); err != nil || string(b) != "body-is-ten" {
		t.Fatalf("the file holds %q (%v)", b, err)
	}
	// Durable before the core heard: the journal has its sync before the answer.
	rel := filepath.ToSlash(strings.TrimPrefix(path, filepath.Dir(r.state)+string(filepath.Separator)))
	ops := r.ops()
	want2 := []string{"mkdir " + filepath.ToSlash(filepath.Base(downloadsDir(r.state))), "create " + rel, "write " + rel, "write " + rel, "sync " + rel}
	if strings.Join(ops[1:], ",") != strings.Join(want2, ",") {
		t.Fatalf("journal %v, want %v", ops, want2)
	}
	// Discarded once installed: deleted, and journaled.
	r.do(nil, discardEffect(9))
	r.x.work.settle()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file is left: %v", err)
	}
	if ops := r.ops(); ops[len(ops)-1] != "remove "+rel {
		t.Fatalf("journal %v", ops)
	}
}

// Socket to file: once more than fileHighWater bytes wait for a file's writes, the stream they
// come from reads nothing more until they are down to fileLowWater; nothing is lost.
func TestStreamsHoldReadingWhileTheFileIsBehind(t *testing.T) {
	d := newPipeDialer()
	fsys := &slowFS{pass: make(chan struct{})}
	r := newStreamRig(t, &streamPlatform{dial: d.dial}, fsys)
	r.do(nil, sOpen(1, "a", 1))
	srv := d.server(t)
	r.take()
	total := 6 << 20
	body := bytes.Repeat([]byte("0123456789abcdef"), total/16)
	r.do(nil, sSplice(1, 7, uint64(total)))
	sent := make(chan struct{})
	go func() { _, _ = srv.Write(body); close(sent) }()
	// The disk takes nothing: what waits stops just past the high-water mark (one read more).
	time.Sleep(300 * time.Millisecond)
	if n := r.s.unwrittenBytes(); n < fileHighWater || n > fileHighWater+readChunk {
		t.Fatalf("%d bytes wait for the file", n)
	}
	select {
	case <-sent:
		t.Fatal("the stream read everything while the file was behind")
	default:
	}
	close(fsys.pass) // the disk catches up (a closed channel lets every write through)
	within(t, "the rest sent", sent)
	if in := r.take(); in.GetStreamSpliced().GetId() != 1 {
		t.Fatalf("got %v", in)
	}
	r.do(nil, dDone(7))
	want := sha256.Sum256(body)
	if in := r.take(); !bytes.Equal(in.GetDownloaded().GetSha256(), want[:]) {
		t.Fatalf("got %v", in)
	}
}

// A download's file: made new (one left at its path is never written over, nor deleted); deleted
// when cancelled mid-write (nothing reported) and when a write fails (download_failed, once
// download_done came).
func TestStreamsDownloadFiles(t *testing.T) {
	t.Run("made new", func(t *testing.T) {
		r := newStreamRig(t, nil, nil)
		path := r.x.downloadPath(4)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("someone else's"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.do(nil, dData(4, []byte("x")), dDone(4))
		if in := r.take(); in.GetDownloadFailed().GetId() != 4 {
			t.Fatalf("got %v", in)
		}
		if b, _ := os.ReadFile(path); string(b) != "someone else's" {
			t.Fatalf("the file there is now %q", b)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		fsys := &slowFS{pass: make(chan struct{})}
		r := newStreamRig(t, nil, fsys)
		r.do(nil, dData(5, []byte("abc")), dData(5, []byte("def")))
		path := r.x.downloadPath(5)
		if !eventually(5*time.Second, func() bool { _, err := os.Stat(path); return err == nil }) {
			t.Fatal("no file")
		}
		r.do(nil, cancelEffect(5), dDone(5)) // (the core says nothing after a cancel; were it to)
		close(fsys.pass)
		r.x.work.settle()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the file is left: %v", err)
		}
		r.quiet(100 * time.Millisecond)
	})
	t.Run("failed, then cancelled before the core took the answer", func(t *testing.T) {
		r := newStreamRig(t, nil, nil)
		path := r.x.downloadPath(6)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("someone else's"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.do(nil, dData(6, []byte("x")), dDone(6))
		var rep report
		select {
		case rep = <-r.x.reports():
		case <-time.After(10 * time.Second):
			t.Fatal("no answer")
		}
		// The core gave up on it while its failure was on its way: never answered.
		r.do(nil, cancelEffect(6))
		if r.x.deliver(rep) {
			t.Fatalf("answered after the cancel: %v", rep.in)
		}
		rep.settled()
		r.quiet(100 * time.Millisecond)
	})
	t.Run("failed", func(t *testing.T) {
		fsys := &slowFS{}
		fsys.fail.Store(true)
		r := newStreamRig(t, nil, fsys)
		r.do(nil, dData(6, []byte("abc")))
		path := r.x.downloadPath(6)
		r.x.work.settle()
		r.quiet(50 * time.Millisecond) // nothing until download_done
		r.do(nil, dData(6, []byte("more")), dDone(6))
		in := r.take()
		if in.GetDownloadFailed().GetId() != 6 || !strings.Contains(in.GetDownloadFailed().Detail, "disk on fire") {
			t.Fatalf("got %v", in)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the file is left: %v", err)
		}
	})
	t.Run("stop", func(t *testing.T) {
		fsys := &slowFS{pass: make(chan struct{})}
		r := newStreamRig(t, nil, fsys)
		r.do(nil, dData(7, []byte("abc")))
		path := r.x.downloadPath(7)
		if !eventually(5*time.Second, func() bool { _, err := os.Stat(path); return err == nil }) {
			t.Fatal("no file")
		}
		go func() { time.Sleep(50 * time.Millisecond); close(fsys.pass) }()
		r.x.shutdown(5 * time.Second)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the file is left after stop: %v", err)
		}
	})
}

// The core's proxy question is answered at once; a stream effect for a stream not open is
// ignored; a cancel of a save goes on to the executor.
func TestStreamsProxyAnswerAndStrays(t *testing.T) {
	r := newStreamRig(t, nil, nil)
	r.s.proxies = newRustProxies(Proxy{URL: "socks5://u:p@proxy:1080"}, nil)
	back := r.do(nil, &corepb.Effect{Kind: &corepb.Effect_ProxyQuery{ProxyQuery: &corepb.Url{Id: 3, Url: "https://example.com/"}}})
	if len(back) != 1 || back[0].in.GetProxyAnswer().GetProxy() != "socks5://u:p@proxy:1080" || back[0].in.GetProxyAnswer().Id != 3 {
		t.Fatalf("got %v", back)
	}
	if back := r.do(nil, sWrite(9, []byte("x")), sTLS(9, "h"), sClose(9, false), sSplice(9, 1, 1)); len(back) != 0 {
		t.Fatalf("got %v", back)
	}
	r.quiet(50 * time.Millisecond)
	if mine, _ := r.s.do(cancelEffect(12)); mine {
		t.Fatal("a cancel taken as Rust mode's alone")
	}
}

// An output that says stopped starts nothing new.
func TestAStoppedOutputStartsNothing(t *testing.T) {
	for _, e := range []*corepb.Effect{sOpen(1, "a", 1), sResolve(1, "a"), wsOpen(1, "ws://a/"),
		{Kind: &corepb.Effect_Download{Download: &corepb.Download{Id: 1}}}} {
		if !starts(e) {
			t.Errorf("%v starts nothing", e)
		}
	}
	for _, e := range []*corepb.Effect{sWrite(1, nil), sClose(1, true), httpRequest(1, "http://a/"), wsClose(1), dDone(1)} {
		if starts(e) {
			t.Errorf("%v starts something", e)
		}
	}
}

// tapOutputs is every effect the core's outputs in the tap file at path carry, in order.
func tapOutputs(t *testing.T, path string) []*corepb.Effect {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []*corepb.Effect
	for len(b) >= 13 {
		kind, n := b[0], int(b[1])|int(b[2])<<8|int(b[3])<<16|int(b[4])<<24
		if len(b) < 13+n {
			t.Fatal("a torn tap record")
		}
		if kind == 2 || kind == 4 {
			reply := &corepb.Reply{}
			if err := proto.Unmarshal(b[13:13+n], reply); err != nil {
				t.Fatal(err)
			}
			out = append(out, reply.Output.GetEffects()...)
		}
		b = b[13+n:]
	}
	return out
}

// Rust mode end to end: a package offered over HTTP and WebSocket is downloaded by the core over
// Go's connections into a file that holds exactly what the server sent, handed over, then deleted
// once installed. With splicing (the default) the body goes from the connection to the file;
// with NoRustSplice it passes through the core (download_data) instead.
func TestRustModeDownloadsAPackage(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, splice := range []bool{true, false} {
			t.Run(fmt.Sprintf("ws=%v/splice=%v", ws, splice), func(t *testing.T) { rustModeDownload(t, ws, splice) })
		}
	}
}

func rustModeDownload(t *testing.T, ws, splice bool) {
	file := bytes.Repeat([]byte("package bytes "), 150_000) // 2.1 MB
	sum := sha256.Sum256(file)
	var offered atomic.Bool
	var srvURL string
	offer := func() *protobufs.ServerToAgent {
		m := &protobufs.ServerToAgent{}
		if offered.CompareAndSwap(false, true) {
			m.PackagesAvailable = &protobufs.PackagesAvailable{AllPackagesHash: []byte{1}, Packages: map[string]*protobufs.PackageAvailable{
				"p": {Type: protobufs.PackageType_PackageType_TopLevel, Version: "1", Hash: []byte{1},
					File: &protobufs.DownloadableFile{DownloadUrl: srvURL + "/file", ContentHash: sum[:]}}}}
		}
		return m
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/file":
			_, _ = w.Write(file)
		case websocket.IsWebSocketUpgrade(r):
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			for {
				_, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				m := &protobufs.AgentToServer{}
				if len(data) == 0 || proto.Unmarshal(data[1:], m) != nil {
					continue
				}
				out := offer()
				out.InstanceUid = m.InstanceUid
				b, _ := proto.Marshal(out)
				_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...))
			}
		default:
			body, _ := io.ReadAll(r.Body)
			m := &protobufs.AgentToServer{}
			_ = proto.Unmarshal(body, m)
			out := offer()
			out.InstanceUid = m.InstanceUid
			b, _ := proto.Marshal(out)
			_, _ = w.Write(b)
		}
	}))
	defer ts.Close()
	srvURL = ts.URL
	dir := t.TempDir()
	cfg := Config{Endpoint: endpoint(ts, ws), Mode: ModeRust, NoRustSplice: !splice, Proxy: Proxy{Direct: true},
		InstanceUID: [16]byte{7}, HeartbeatInterval: 50 * time.Millisecond, StateFile: filepath.Join(dir, "state"),
		AcceptPackages: true, tapFile: filepath.Join(dir, "core.tap")}
	installed := make(chan string, 1)
	c, err := Start(context.Background(), cfg, Status{}, func(c *Client, n Notification) {
		if n.Kind != InstallPackage {
			return
		}
		got, err := os.ReadFile(n.Path)
		if err != nil || !bytes.Equal(got, file) {
			t.Errorf("the file handed over holds %d bytes (%v), the server sent %d", len(got), err, len(file))
		}
		c.PackageInstalled(n.PackageName, n.Package.GetHash(), nil)
		installed <- n.Path
	})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	select {
	case path = <-installed:
	case <-time.After(20 * time.Second):
		t.Fatal("no package installed")
	}
	// Deleted once installed (the core discards it).
	if !eventually(10*time.Second, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) }) {
		t.Error("the file is left after it was installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	spliced, data := 0, 0
	for _, e := range tapOutputs(t, cfg.tapFile) {
		if e.GetStreamSplice() != nil {
			spliced++
		}
		if e.GetDownloadData() != nil {
			data++
		}
	}
	if splice && spliced == 0 {
		t.Error("no stream_splice with splicing on")
	}
	if !splice && (spliced > 0 || data == 0) {
		t.Errorf("with NoRustSplice: %d splices, %d download_data", spliced, data)
	}
	if err := checkTap(dir); err != nil {
		t.Error(err)
	}
}

// Rust mode: a 503 whose Retry-After is an HTTP-date a few seconds ahead is retried after about
// that long, the core reading the date against the wall clock each step carries (not 1970, which
// makes it the hour's cap).
func TestRustModeRetryAfterDate(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("ws=%v", ws), func(t *testing.T) {
			var mu sync.Mutex
			var refused time.Time
			again := make(chan time.Time, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if refused.IsZero() {
					refused = time.Now()
					w.Header().Set("Retry-After", refused.Add(3*time.Second).UTC().Format(http.TimeFormat))
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				select {
				case again <- time.Now():
				default:
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer ts.Close()
			cfg := Config{Endpoint: endpoint(ts, ws), Mode: ModeRust, Proxy: Proxy{Direct: true},
				InstanceUID: [16]byte{7}, HeartbeatInterval: 50 * time.Millisecond}
			c, err := Start(context.Background(), cfg, Status{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = c.Stop(ctx)
			}()
			select {
			case at := <-again:
				mu.Lock()
				waited := at.Sub(refused)
				mu.Unlock()
				// The date has whole seconds: 2 to 3 s after the 503.
				if waited < 1500*time.Millisecond {
					t.Errorf("tried again %v after a Retry-After 3 s ahead", waited)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("not tried again within 15 s of a Retry-After 3 s ahead")
			}
		})
	}
}

// What can't apply in the mode the client runs in stops Start with the reason: nothing one mode
// alone honours is ignored.
func TestAModeRefusesWhatItCantTake(t *testing.T) {
	named := &tls.Config{ServerName: "opamp.example.com"}
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeRust, HTTPClient: &http.Client{}}, "HTTPClient in Rust mode"},
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: "python"}, "Mode \"python\""},
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeRust, Proxy: Proxy{URL: "socks4://p:1"}}, "SOCKS4"},
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeRust, TLS: TLS{CAPEM: []byte("no")}}, "CAPEM"},
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeRust, TLS: TLS{Config: named}}, "TLS.Config.ServerName in Rust mode"},
		{Config{Endpoint: "http://127.0.0.1:9/", TLS: TLS{Config: named}}, "TLS.Config.ServerName in Rust mode"},
		{Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeGo, NoRustSplice: true}, "NoRustSplice in Go mode"},
		{Config{Endpoint: "http://127.0.0.1:9/", HTTPClient: &http.Client{}, NoRustSplice: true}, "NoRustSplice in Go mode"},
	} {
		if _, err := Start(context.Background(), c.cfg, Status{}, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
}

// The mode a client runs in: Rust mode with nothing said, Go mode when asked for, by Mode or by
// an HTTP client of the application's (which only Go mode can use).
func TestTheModeAClientRunsIn(t *testing.T) {
	own := &http.Client{}
	for _, c := range []struct {
		name string
		cfg  Config
		rust bool
	}{
		{"nothing said", Config{}, true},
		{"TLS and a proxy", Config{TLS: TLS{MinVersion: tls.VersionTLS13}, Proxy: Proxy{URL: "socks5://p:1"}}, true},
		{"ModeRust", Config{Mode: ModeRust}, true},
		{"ModeGo", Config{Mode: ModeGo}, false},
		{"an HTTPClient", Config{HTTPClient: own}, false},
		{"ModeGo and an HTTPClient", Config{Mode: ModeGo, HTTPClient: own}, false},
		{"Go mode's tls.Config, named", Config{Mode: ModeGo, TLS: TLS{Config: &tls.Config{ServerName: "a"}}}, false},
	} {
		if rust, err := c.cfg.rustMode(); err != nil || rust != c.rust {
			t.Errorf("%s: Rust mode %v (%v), want %v", c.name, rust, err, c.rust)
		}
	}
}

// With nothing said about the mode, the client's connection is Rust mode's: which proxy to use is
// asked of Rust mode's rule (the environment the test gives; Go mode never reads it), and the
// server hears the client.
func TestWithNothingSaidTheClientRunsInRustMode(t *testing.T) {
	heard := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case heard <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	var asked atomic.Int32
	cfg := Config{Endpoint: ts.URL, HeartbeatInterval: time.Hour, environ: func(string) (string, bool) {
		asked.Add(1)
		return "", false
	}}
	c, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-heard:
	case <-time.After(15 * time.Second):
		t.Error("the server heard nothing within 15 s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Stop(ctx)
	if asked.Load() == 0 {
		t.Error("Rust mode's proxy rule was never asked: the client ran in Go mode")
	}
}
