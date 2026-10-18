// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself, and this carries out
// only what needs the platform: TCP connections, TLS on them (crypto/tls, with the operator's CAs,
// certificate and versions), name resolution (only for a SOCKS5 proxy with Proxy.ResolveLocally:
// otherwise the proxy resolves), the proxy core mode's rule picks for a URL (coreproxy.go), and the
// files of downloads. It decides nothing: every byte it writes, the core wrote, and the core keeps
// every time (it closes a stream it gives up on, and cancels a download's file). As
// node/opamp/src/streams.ts and python/src/pydantic_opamp_client/_streams.py do.
//
// One register (inFlight, as the platform executor's) owns everything under way: each stream (its
// connection, the goroutine connecting it, starting its TLS and reading it, the one writing it,
// and its linger once closed), each name being resolved, each file's writes. A close, from the
// core or at shutdown, ends what it names at once; a connection, a TLS handshake or a resolution
// that completes after its stream was closed is closed as it arrives, and reported to no one
// (inFlight.deliver drops a report whose entry has gone, even one already queued). Shutdown waits
// for what is left at most the bound stop gives it.
//
// A stream's goroutine reads in step with the core: it hands over what it read and waits until the
// core has taken it and its effects are carried out before it reads again. So a stream_splice,
// a hold (below) or a stream_start_tls the core asked for in answer applies to the very next
// bytes, as on Node's and Python's event loops.
//
// Download files are written beside the loop, one write at a time per file, in the order the
// bytes came: what is queued meanwhile goes in the next write, and is hashed as it is written.
// Each download ends here once, answering the shell's download_done (synced, then downloaded;
// or download_failed if a write failed: what came after is dropped, and the file deleted), or
// not at all when the core discards or cancels it first: then the file goes once the write in
// flight is over, and nothing more is said about it.
//
// Backpressure, both ways:
//   - Socket to file. Once more than fileHighWater bytes wait for a file's writes, the stream that
//     brought the last of them stops reading (its spliced bytes, or the data the core turned into
//     download_data just then), until they are down to fileLowWater. Nothing is dropped or
//     reordered: the kernel's window stops the server.
//   - Core to socket. The core writes without hearing back. While more than sendHighWater bytes
//     written to a stream are still unsent, the stream reads nothing more, so a peer that doesn't
//     read hears nothing more to answer; and a write that finds more than maxUnsent bytes still
//     unsent fails the stream (stream_failed: the core reconnects, as for any lost connection)
//     rather than buffering without end.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

const (
	// streamLinger is how long a closed stream has to flush what was written before it is reset.
	streamLinger = 2 * time.Second
	// readChunk is the most one read of a stream takes.
	readChunk = 64 << 10
	// maxWritev is the most buffers one file write takes.
	maxWritev = 256
	// fileHighWater: bytes waiting for a file's writes beyond which the stream they come from
	// stops reading...
	fileHighWater = 4 << 20
	// fileLowWater: ...until they are down to this.
	fileLowWater = 1 << 20
	// sendHighWater: bytes written to a stream and not yet sent beyond which it stops reading
	// until they have gone.
	sendHighWater = 256 << 10
	// maxUnsent: bytes written to a stream and not yet sent beyond which the next write fails it.
	maxUnsent = 16 << 20
)

// streamPlatform is what core mode does its network work with: Go's dialer, crypto/tls and
// resolver, unless a test stands in for them (Config.streamNet).
type streamPlatform struct {
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	startTLS func(ctx context.Context, conn net.Conn, cfg *tls.Config) (net.Conn, error)
	resolve  func(ctx context.Context, host string) ([]string, error)
}

// goStreams is the platform's own, with p's functions where it has them.
func goStreams(p *streamPlatform) streamPlatform {
	// No connect or handshake timeouts: how long anything may take is the core's.
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	out := streamPlatform{
		dial: dialer.DialContext,
		startTLS: func(ctx context.Context, conn net.Conn, cfg *tls.Config) (net.Conn, error) {
			tc := tls.Client(conn, cfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			return tc, nil
		},
		resolve: func(ctx context.Context, host string) ([]string, error) {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var addrs []string
			seen := map[string]bool{}
			for _, ip := range ips {
				a := ip.IP.String()
				if !seen[a] {
					seen[a] = true
					addrs = append(addrs, a)
				}
			}
			return addrs, nil
		},
	}
	if p != nil {
		if p.dial != nil {
			out.dial = p.dial
		}
		if p.startTLS != nil {
			out.startTLS = p.startTLS
		}
		if p.resolve != nil {
			out.resolve = p.resolve
		}
	}
	return out
}

// streams carries out core mode's effects (do takes those that are its own) for the executor x,
// whose channel its reports go out on, and whose file system, state file and download paths its
// files use.
type streams struct {
	x       *executor
	tls     *tls.Config // the operator's, HTTP/1.1 only: each handshake takes a copy for its host
	proxies *coreProxies
	p       streamPlatform

	// reg holds every stream and resolution by stream id (the shell's ids), and runs every
	// goroutine of core mode; files holds each download file under way by download id (the
	// core's), for what is reported about it.
	reg   *inFlight
	files *inFlight

	mu       sync.Mutex
	live     map[*stream]bool   // streams whose connection is still to be closed (lingering ones too)
	fileOf   map[uint64]*dlFile // download files under way
	answered map[uint64]bool    // downloads answered and not deleted yet: their file is complete
	closed   bool               // shut down

	// reading is the stream whose report the loop is stepping: the download_data the core makes
	// of it came from it. The loop's alone.
	reading *stream
}

func newStreams(x *executor, cfg *tls.Config, proxies *coreProxies, p *streamPlatform) *streams {
	return &streams{x: x, tls: http1Only(cfg), proxies: proxies, p: goStreams(p), reg: newInFlight(), files: newInFlight(),
		live: map[*stream]bool{}, fileOf: map[uint64]*dlFile{}, answered: map[uint64]bool{}}
}

// stream is one stream: its connection, and what it waits for.
type stream struct {
	s  *streams
	id uint64
	e  *effect // its entry in s.reg: done once closed (or gone, or shut down)

	dead    chan struct{} // closed once its connection is closed
	wake    chan struct{} // holds a token when the writer has something new to look at
	written chan struct{} // closed when the writer is done (everything sent, or it failed)

	// Under s.mu:
	raw     net.Conn // the TCP connection, once made
	conn    net.Conn // what the bytes go through: raw, or TLS on it; nil until ready, and in a handshake
	tlsHost string   // the core asked for TLS on it, for this host (the reader starts it)
	tlsAsk  bool
	inRead  bool          // the reader is in a Read
	splice  *spliceTo     // its next bytes go to a download's file
	holds   int           // reasons not to read: files it waits for, and a full write queue
	resume  chan struct{} // closed when holds is back to 0
	blocked bool          // one of the holds: too much written is unsent
	queue   [][]byte      // written, not yet sent
	unsent  int           // bytes in queue and in the write under way
	drained chan struct{} // closed when unsent is back to 0 (nil while it is)
	closing bool          // closed by the core, not aborted: FIN once what was written has gone
	ended   bool          // its connection is closed (dead is closed)
}

type spliceTo struct {
	download uint64
	left     uint64
}

func (st *stream) signal() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// hold stops it reading, for one more reason. Under s.mu.
func (st *stream) hold() {
	st.holds++
	if st.holds == 1 {
		st.resume = make(chan struct{})
	}
}

// release is one reason fewer not to read: it reads again if it was the last. Under s.mu.
func (st *stream) release() {
	if st.holds == 0 {
		return
	}
	st.holds--
	if st.holds == 0 {
		close(st.resume)
	}
}

// end closes its connection: at once with a reset (abort), or a plain close. It is no longer
// anyone's to close after.
func (st *stream) end(reset bool) {
	st.s.mu.Lock()
	if st.ended {
		st.s.mu.Unlock()
		return
	}
	st.ended = true
	close(st.dead)
	delete(st.s.live, st)
	raw := st.raw
	st.splice, st.queue = nil, nil
	st.s.mu.Unlock()
	if raw == nil {
		return // still connecting: its dial is cancelled, and a connection made anyway is closed
	}
	if tcp, ok := raw.(*net.TCPConn); ok && reset {
		_ = tcp.SetLinger(0)
	}
	_ = raw.Close()
}

func (st *stream) isEnded() bool {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	return st.ended
}

// fail ends the stream with a reset and reports it gone (unless it was closed already).
func (st *stream) fail(err error, otherwise string) {
	st.end(true)
	detail := otherwise
	if err != nil && err.Error() != "" {
		detail = err.Error()
	}
	st.s.x.emit(report{e: st.e, in: streamFailed(st.id, detail), last: true})
}

// report hands in to the core as the stream's, and waits until the loop has stepped it and
// carried out its effects: false if the stream is closed (or the client gone) by then.
func (st *stream) report(in *corepb.Input) bool {
	r := report{e: st.e, in: in, ack: make(chan struct{}), src: st}
	st.s.x.emit(r)
	select {
	case <-r.ack:
		return st.e.ctx.Err() == nil
	case <-st.e.ctx.Done():
		return false
	case <-st.s.x.done:
		return false
	}
}

func streamFailed(id uint64, detail string) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_StreamFailed{StreamFailed: &corepb.Failed{Id: id, Detail: lossy(detail)}}}
}

// do carries out e if it is one of core mode's: whether it was, and an outcome known at once for
// the loop to queue. A cancel is also the executor's (it may name a save): do says it isn't only
// this one's.
func (s *streams) do(eff *corepb.Effect) (bool, *report) {
	switch k := eff.Kind.(type) {
	case *corepb.Effect_ProxyQuery:
		return true, &report{in: s.proxies.answer(k.ProxyQuery.Id)}
	case *corepb.Effect_StreamResolve:
		s.resolve(k.StreamResolve.Id, k.StreamResolve.Host)
	case *corepb.Effect_StreamOpen:
		s.open(k.StreamOpen.Id, k.StreamOpen.Host, k.StreamOpen.Port)
	case *corepb.Effect_StreamStartTls:
		s.startTLS(k.StreamStartTls.Id, k.StreamStartTls.Host)
	case *corepb.Effect_StreamWrite:
		return true, s.write(k.StreamWrite.Id, k.StreamWrite.Data)
	case *corepb.Effect_StreamClose:
		s.close(k.StreamClose.Id, k.StreamClose.Abort)
	case *corepb.Effect_StreamSplice:
		return true, s.splice(k.StreamSplice)
	case *corepb.Effect_DownloadData:
		s.mu.Lock()
		s.appendFile(k.DownloadData.Id, k.DownloadData.Data, s.reading)
		s.mu.Unlock()
	case *corepb.Effect_DownloadDone:
		s.mu.Lock()
		if !s.closed {
			f := s.file(k.DownloadDone.Id)
			f.done = true
			s.pump(f)
		}
		s.mu.Unlock()
	case *corepb.Effect_Discard:
		return s.drop(k.Discard.Id), nil
	case *corepb.Effect_Cancel:
		// A download's file the core gave up on (its stream is closed already), or a save: the
		// executor's.
		s.drop(k.Cancel.Id)
		return false, nil
	default:
		return false, nil
	}
	return true, nil
}

// stream is stream id, if it is open (or opening).
func (s *streams) stream(id uint64) *stream {
	if e := s.reg.get(id); e != nil {
		return e.stream
	}
	return nil
}

// forget gives up on whatever has id now (a stream, a resolution): it goes at once, unreported.
func (s *streams) forget(id uint64) {
	if e := s.reg.cancel(id); e != nil && e.stream != nil {
		e.stream.end(true)
	}
}

func (s *streams) resolve(id uint64, host string) {
	s.forget(id)
	e := s.reg.add(id)
	if e.ctx.Err() != nil {
		return // shut down
	}
	s.reg.spawn(func() {
		addrs, err := s.p.resolve(e.ctx, host)
		var in *corepb.Input
		switch {
		case err != nil:
			in = streamFailed(id, err.Error())
		case len(addrs) == 0:
			in = streamFailed(id, "no address for "+host)
		default:
			in = &corepb.Input{Kind: &corepb.Input_StreamResolved{StreamResolved: &corepb.StreamResolved{Id: id, Addresses: addrs}}}
		}
		// After its close, or shutdown: nothing (emit, and deliver for one already queued).
		s.x.emit(report{e: e, in: in, last: true})
	})
}

func (s *streams) open(id uint64, host string, port uint32) {
	s.forget(id)
	e := s.reg.add(id)
	if e.ctx.Err() != nil {
		return // shut down
	}
	st := &stream{s: s, id: id, e: e, dead: make(chan struct{}), wake: make(chan struct{}, 1), written: make(chan struct{})}
	e.stream = st
	s.mu.Lock()
	s.live[st] = true
	s.mu.Unlock()
	addr := net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
	s.reg.spawn(func() { st.run(addr) })
	s.reg.spawn(st.write)
}

// run connects the stream, then reads it until it ends, in step with the core.
func (st *stream) run(addr string) {
	conn, err := st.s.p.dial(st.e.ctx, "tcp", addr)
	if err != nil {
		st.fail(err, "connecting failed")
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	st.s.mu.Lock()
	if st.ended || st.e.ctx.Err() != nil {
		// Closed while it connected: it goes at once.
		st.s.mu.Unlock()
		_ = conn.Close()
		st.end(true)
		return
	}
	st.raw, st.conn = conn, conn
	st.s.mu.Unlock()
	st.signal() // what was written meanwhile goes now
	if st.report(&corepb.Input{Kind: &corepb.Input_StreamOpened{StreamOpened: &corepb.Id{Id: st.id}}}) {
		st.read()
	} else {
		st.linger()
	}
}

// read passes on what arrives, one read at a time, each once the core has taken the one before:
// as data, or straight to a download's file while a splice lasts; and starts TLS when asked.
func (st *stream) read() {
	buf := make([]byte, readChunk)
	for {
		if st.e.ctx.Err() != nil {
			st.linger()
			return
		}
		if st.isEnded() {
			return // failed, or shut down: its report (if any) is on its way
		}
		if host, ok := st.tlsAsked(); ok {
			if !st.handshake(host) {
				st.linger()
				return
			}
			continue
		}
		if !st.waitReadable() {
			continue // closed: lingers, at the top
		}
		st.s.mu.Lock()
		conn, max := st.conn, len(buf)
		if sp := st.splice; sp != nil && sp.left < uint64(max) {
			max = int(sp.left)
		}
		if st.tlsAsk || conn == nil {
			st.s.mu.Unlock()
			continue
		}
		st.inRead = true
		st.s.mu.Unlock()
		n, err := conn.Read(buf[:max])
		st.s.mu.Lock()
		st.inRead = false
		asked := st.tlsAsk
		st.s.mu.Unlock()
		if st.e.ctx.Err() != nil {
			continue // closed meanwhile: what came goes nowhere, and the stream lingers
		}
		if n > 0 && !st.got(buf[:n]) {
			continue
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if asked && errors.As(err, &ne) && ne.Timeout() {
			continue // interrupted to start TLS (tlsAsked clears the deadline)
		}
		if errors.Is(err, io.EOF) {
			if !st.report(&corepb.Input{Kind: &corepb.Input_StreamEof{StreamEof: &corepb.Id{Id: st.id}}}) {
				continue
			}
			// The peer is done; so is the connection, as on Node's and Python's sockets: our side
			// closes too, once what was written has gone, and the core hears it is gone (unless it
			// closed it on hearing the end).
			st.s.mu.Lock()
			graceful := !st.ended && st.conn != nil
			st.closing = graceful
			st.s.mu.Unlock()
			if graceful {
				st.signal()
				st.s.lingerAtMost(st)
			}
			st.s.x.emit(report{e: st.e, in: streamFailed(st.id, "the connection closed"), last: true})
			st.linger()
			return
		}
		st.fail(err, "reading failed")
		return
	}
}

// got passes on b, read from the stream: to a download's file while a splice lasts (then
// stream_spliced), else to the core. False if the stream is closed by then.
func (st *stream) got(b []byte) bool {
	st.s.mu.Lock()
	sp := st.splice
	if sp == nil {
		st.s.mu.Unlock()
		return st.report(&corepb.Input{Kind: &corepb.Input_StreamData{StreamData: &corepb.WsMessage{Id: st.id, Data: bytes.Clone(b)}}})
	}
	// Body bytes: straight to the file, past the core (read up to what the splice has left).
	st.s.appendFile(sp.download, bytes.Clone(b), st)
	sp.left -= uint64(len(b))
	if sp.left > 0 {
		st.s.mu.Unlock()
		return st.e.ctx.Err() == nil
	}
	st.splice = nil
	st.s.mu.Unlock()
	return st.report(&corepb.Input{Kind: &corepb.Input_StreamSpliced{StreamSpliced: &corepb.Id{Id: st.id}}})
}

// waitReadable waits until nothing holds the stream: false if it is closed meanwhile.
func (st *stream) waitReadable() bool {
	for {
		st.s.mu.Lock()
		if st.holds == 0 || st.tlsAsk {
			st.s.mu.Unlock()
			return true
		}
		resume := st.resume
		st.s.mu.Unlock()
		select {
		case <-resume:
		case <-st.e.ctx.Done():
			return false
		case <-st.dead:
			return false
		}
	}
}

// tlsAsked takes the core's ask for TLS, if there is one.
func (st *stream) tlsAsked() (string, bool) {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	if !st.tlsAsk {
		return "", false
	}
	st.tlsAsk = false
	if st.conn != nil {
		_ = st.conn.SetReadDeadline(time.Time{})
	}
	return st.tlsHost, true
}

// handshake starts TLS for host on the stream, once what was written before has gone (a CONNECT,
// say): TLS inside TLS on a stream that is TLS to an https:// proxy already. False if it failed or
// the stream was closed meanwhile.
func (st *stream) handshake(host string) bool {
	if !st.waitDrained() {
		return false
	}
	st.s.mu.Lock()
	conn := st.conn
	st.conn = nil // what the core writes meanwhile waits for TLS
	st.s.mu.Unlock()
	if conn == nil {
		return false
	}
	cfg := st.s.tls.Clone()
	cfg.ServerName = host // crypto/tls sends no SNI for an address, and verifies it as one
	tc, err := st.s.p.startTLS(st.e.ctx, conn, cfg)
	if err != nil {
		st.fail(err, "the TLS handshake failed")
		return false
	}
	st.s.mu.Lock()
	if st.ended || st.e.ctx.Err() != nil {
		// Closed while it shook hands: it goes at once.
		st.s.mu.Unlock()
		st.end(true)
		return false
	}
	st.conn = tc
	st.s.mu.Unlock()
	st.signal()
	return st.report(&corepb.Input{Kind: &corepb.Input_StreamTlsReady{StreamTlsReady: &corepb.Id{Id: st.id}}})
}

// waitDrained waits until everything written has gone: false if the stream is closed meanwhile.
func (st *stream) waitDrained() bool {
	st.s.mu.Lock()
	drained := st.drained
	st.s.mu.Unlock()
	if drained == nil {
		return true
	}
	select {
	case <-drained:
		return true
	case <-st.e.ctx.Done():
		return false
	case <-st.dead:
		return false
	}
}

// linger: the stream is closed (or gone). One the core closed with what was written still going
// reads what the peer still sends, into nothing, until the peer closes too (or the linger
// ends it); then it is closed.
func (st *stream) linger() {
	st.s.mu.Lock()
	raw, closing, ended := st.raw, st.closing, st.ended
	st.s.mu.Unlock()
	if raw == nil || ended || !closing {
		return
	}
	_ = raw.SetReadDeadline(time.Time{})
	buf := make([]byte, 4<<10)
	for {
		if _, err := raw.Read(buf); err != nil {
			break
		}
	}
	// The peer has closed: once everything written has gone (or the linger has ended it), close.
	select {
	case <-st.written:
	case <-st.dead:
	}
	st.end(false)
}

// write sends what the core writes, in order, once the stream is ready; after the core's close,
// once everything has gone, it ends the stream's sending (FIN).
func (st *stream) write() {
	defer close(st.written)
	for {
		st.s.mu.Lock()
		for !st.ended && (len(st.queue) == 0 || st.conn == nil) {
			if len(st.queue) == 0 && st.closing && st.conn != nil {
				conn, raw := st.conn, st.raw
				st.s.mu.Unlock()
				st.finish(conn, raw)
				return
			}
			st.s.mu.Unlock()
			select {
			case <-st.wake:
			case <-st.dead:
				return
			}
			st.s.mu.Lock()
		}
		if st.ended {
			st.s.mu.Unlock()
			return
		}
		chunk, conn := st.queue[0], st.conn
		st.queue[0] = nil
		st.queue = st.queue[1:]
		st.s.mu.Unlock()
		_, err := conn.Write(chunk)
		st.s.mu.Lock()
		st.unsent -= len(chunk)
		if st.unsent == 0 {
			if st.drained != nil {
				close(st.drained)
				st.drained = nil
			}
			if st.blocked {
				st.blocked = false
				st.release()
			}
		}
		st.s.mu.Unlock()
		if err != nil {
			st.fail(err, "writing failed")
			return
		}
	}
}

// finish ends the sending of a stream the core closed, everything written having gone: TLS's
// close_notify, then a FIN; the reader reads on until the peer closes (linger). A connection
// without half-closes (none of TCP's) is closed now.
func (st *stream) finish(conn, raw net.Conn) {
	if tc, ok := conn.(*tls.Conn); ok {
		_ = tc.CloseWrite()
	}
	if cw, ok := raw.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		return
	}
	st.end(false)
}

func (s *streams) startTLS(id uint64, host string) {
	st := s.stream(id)
	if st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.conn == nil || st.ended {
		return // not ready: the core asks only once it is
	}
	st.tlsHost, st.tlsAsk = host, true
	if st.inRead {
		// The reader is in a Read (it isn't, when the core asks in answer to what it read): it
		// stops, and starts TLS.
		_ = st.conn.SetReadDeadline(time.Unix(1, 0))
	}
}

// write queues data for stream id: an outcome (it failed) if too much is unsent already.
func (s *streams) write(id uint64, data []byte) *report {
	st := s.stream(id)
	if st == nil {
		return nil
	}
	s.mu.Lock()
	if st.ended || st.closing {
		s.mu.Unlock()
		return nil
	}
	if st.unsent > maxUnsent {
		unsent := st.unsent
		s.mu.Unlock()
		// The peer has stopped reading: give up on it rather than buffer without end.
		st.end(true)
		return &report{e: st.e, in: streamFailed(id, fmt.Sprintf("the peer has not taken the last %d bytes written to it", unsent)), last: true}
	}
	if len(data) == 0 {
		s.mu.Unlock()
		return nil
	}
	st.queue = append(st.queue, data)
	st.unsent += len(data)
	if st.drained == nil {
		st.drained = make(chan struct{})
	}
	if st.unsent > sendHighWater && !st.blocked && st.conn != nil {
		// Read nothing more from it until it has drained.
		st.blocked = true
		st.hold()
	}
	s.mu.Unlock()
	st.signal()
	return nil
}

// close: stream id is no longer anyone's, and nothing more about it is reported. abort: reset at
// once; otherwise once what was written has gone (FIN), streamLinger at most.
func (s *streams) close(id uint64, abort bool) {
	e := s.reg.cancel(id)
	if e == nil || e.stream == nil {
		return // a resolution, cancelled; or nothing
	}
	st := e.stream
	s.mu.Lock()
	st.splice = nil
	// Given up on, or never ready (connecting, in a handshake: nothing written has anywhere to go).
	graceful := !abort && !st.ended && st.conn != nil && !st.tlsAsk
	if graceful {
		st.closing = true // the reader lingers once it sees it closed
	}
	s.mu.Unlock()
	if !graceful {
		st.end(true)
		return
	}
	st.signal()
	s.lingerAtMost(st)
}

// lingerAtMost resets st if it is still not closed streamLinger from now.
func (s *streams) lingerAtMost(st *stream) {
	s.reg.spawn(func() {
		t := time.NewTimer(streamLinger)
		defer t.Stop()
		select {
		case <-t.C:
			st.end(true)
		case <-st.dead:
		}
	})
}

// splice sends stream sp.Id's next sp.Bytes bytes to download sp.Download's file.
func (s *streams) splice(sp *corepb.StreamSplice) *report {
	st := s.stream(sp.Id)
	if st == nil {
		return nil
	}
	if sp.Bytes == 0 {
		return &report{e: st.e, in: &corepb.Input{Kind: &corepb.Input_StreamSpliced{StreamSpliced: &corepb.Id{Id: sp.Id}}}}
	}
	s.mu.Lock()
	if !st.ended {
		st.splice = &spliceTo{download: sp.Download, left: sp.Bytes}
	}
	s.mu.Unlock()
	return nil
}

// Files.

// dlFile is a download's file.
type dlFile struct {
	id   uint64
	path string
	e    *effect // its entry in s.files: what is reported about it

	// Under s.mu:
	f         writeFile // open, once made
	queue     [][]byte  // bytes waiting for the next write, in order
	unwritten int       // bytes queued or being written
	paused    map[*stream]bool
	busy      bool // a pump is running: only it touches f and hash
	made      bool // made by this client (and not deleted yet): only then is it its to delete
	done      bool // download_done came: sync, close and answer once everything queued is written
	gone      bool // discarded or cancelled (or shut down): delete it, say nothing
	err       string

	hash hash.Hash // of the bytes written (the pump's)
}

// file is download id's file, made up at its first mention. Under s.mu.
func (s *streams) file(id uint64) *dlFile {
	f := s.fileOf[id]
	if f == nil {
		f = &dlFile{id: id, e: s.files.add(id), paused: map[*stream]bool{}, hash: sha256.New()}
		if s.x.stateFile != "" {
			f.path = s.x.downloadPath(id)
		}
		s.fileOf[id] = f
	}
	return f
}

// appendFile queues data for the end of download id's file; src: the stream it came from, held
// while too much waits. Under s.mu.
func (s *streams) appendFile(id uint64, data []byte, src *stream) {
	if s.closed {
		return
	}
	f := s.file(id)
	// After a failure (or a discard), the bytes go nowhere.
	if f.err != "" || f.gone || len(data) == 0 {
		return
	}
	f.queue = append(f.queue, data)
	f.unwritten += len(data)
	if src != nil && f.unwritten > fileHighWater && !f.paused[src] {
		f.paused[src] = true
		src.hold()
	}
	s.pump(f)
}

// unpause lets the streams f held read again. Under s.mu.
func (s *streams) unpause(f *dlFile) {
	for st := range f.paused {
		st.release()
	}
	clear(f.paused)
}

// unwrittenBytes is how many bytes wait for files' writes, all of them (for tests).
func (s *streams) unwrittenBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.fileOf {
		n += f.unwritten
	}
	return n
}

// pump carries f forward, unless that is under way. Under s.mu.
func (s *streams) pump(f *dlFile) {
	if f.busy {
		return
	}
	f.busy = true
	if s.x.work != nil {
		s.x.work.begin()
	}
	s.reg.spawn(func() {
		if s.x.work != nil {
			defer s.x.work.end()
		}
		s.run(f)
	})
}

// run writes what is queued for f; then, if it is done, syncs it and answers; if it is gone or
// failed, deletes it (answering download_done with the failure, once it has come).
func (s *streams) run(f *dlFile) {
	s.mu.Lock()
	for {
		switch {
		case f.gone || f.err != "":
			answer := s.removeFile(f)
			if answer == nil && !f.gone && f.done {
				continue // download_done came while it deleted: answer it
			}
			// Gone: nothing more. Failed before download_done: that answers it, when it comes.
			f.busy = false
			s.mu.Unlock()
			if answer != nil {
				s.x.emit(report{e: f.e, in: answer, last: true})
			}
			return
		case f.f == nil && (len(f.queue) > 0 || f.done):
			if f.path == "" {
				f.err = "no state file"
				continue
			}
			s.mu.Unlock()
			wf, err := s.newFile(f.path)
			s.mu.Lock()
			if err != nil {
				f.err = fmt.Sprintf("writing %s: %v", f.path, err)
				continue
			}
			f.f, f.made = wf, true
		case len(f.queue) > 0:
			n := min(len(f.queue), maxWritev)
			bufs := f.queue[:n:n]
			f.queue = f.queue[n:]
			wf := f.f
			s.mu.Unlock()
			wrote, err := writeAll(wf, bufs, f.hash)
			s.mu.Lock()
			f.unwritten -= wrote
			if f.unwritten <= fileLowWater {
				s.unpause(f)
			}
			if err != nil {
				f.err = fmt.Sprintf("writing %s: %v", f.path, err)
			}
		case f.done:
			// Complete: durable before the core hears of it.
			wf := f.f
			s.mu.Unlock()
			err := wf.Sync()
			if cerr := wf.Close(); err == nil {
				err = cerr
			}
			s.mu.Lock()
			f.f = nil
			if err != nil {
				f.err = fmt.Sprintf("writing %s: %v", f.path, err)
				continue
			}
			if f.gone {
				continue
			}
			delete(s.fileOf, f.id)
			s.answered[f.id] = true
			f.busy = false
			s.mu.Unlock()
			s.x.emit(report{e: f.e, in: &corepb.Input{Kind: &corepb.Input_Downloaded{Downloaded: &corepb.Downloaded{Id: f.id, Sha256: f.hash.Sum(nil)}}}, last: true})
			return
		default:
			f.busy = false
			s.mu.Unlock()
			return
		}
	}
}

// newFile makes a download's file, new (never one already there, a crashed run's say), in its
// directory, made if need be: both this user's only.
func (s *streams) newFile(path string) (writeFile, error) {
	if err := s.x.fs.mkdir(filepath.Dir(path)); err != nil && !os.IsExist(err) {
		return nil, err
	}
	return s.x.fs.createNew(path)
}

// removeFile closes and deletes a file that failed or is gone (only one it made), and drops what
// waits for it: the answer to download_done, if it came and the file isn't gone. Under s.mu
// (released while it deletes).
func (s *streams) removeFile(f *dlFile) *corepb.Input {
	// Nothing more is written: the streams it held read on (into nothing, until they end).
	f.queue = nil
	f.unwritten = 0
	s.unpause(f)
	wf, made := f.f, f.made
	f.f, f.made = nil, false
	if wf != nil || made {
		s.mu.Unlock()
		if wf != nil {
			_ = wf.Close()
		}
		if made {
			_ = s.x.fs.remove(f.path)
		}
		s.mu.Lock()
	}
	if f.gone {
		if s.fileOf[f.id] == f {
			delete(s.fileOf, f.id)
		}
		return nil
	}
	if f.done {
		delete(s.fileOf, f.id)
		return &corepb.Input{Kind: &corepb.Input_DownloadFailed{DownloadFailed: &corepb.Failed{Id: f.id, Detail: lossy(f.err)}}}
	}
	return nil
}

// writeAll writes bufs in full to wf, hashing each byte once it is written: how many were.
func writeAll(wf writeFile, bufs [][]byte, h hash.Hash) (int, error) {
	total := 0
	for _, b := range bufs {
		for len(b) > 0 {
			n, err := wf.Write(b)
			if n > 0 {
				h.Write(b[:n])
				total += n
				b = b[n:]
			}
			if err != nil {
				return total, err
			}
			if n == 0 {
				return total, errors.New("the file took none of the bytes written to it")
			}
		}
	}
	return total, nil
}

// drop: the core is done with download id's file (discard), or gave up on it (cancel). One under
// way goes once the write in flight is over; one complete goes now. Whether it was this one's.
func (s *streams) drop(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.fileOf[id]; f != nil {
		f.gone = true
		s.files.cancel(id)
		s.pump(f)
		return true
	}
	if !s.answered[id] {
		// A failure on its way (removeFile's): not answered once the core has given up on it.
		s.files.cancel(id)
		return false
	}
	delete(s.answered, id)
	s.files.cancel(id) // its answer, if it is still on its way, with it
	if s.closed {
		return true
	}
	path := s.x.downloadPath(id)
	if s.x.work != nil {
		s.x.work.begin()
	}
	s.reg.spawn(func() {
		if s.x.work != nil {
			defer s.x.work.end()
		}
		_ = s.x.fs.remove(path)
	})
	return true
}

// cancelAll ends everything at once: every stream closed with a reset, every resolution and
// connect stopped, nothing reported any more, and every file not complete deleted once its write
// in flight is over. wait then waits for it.
func (s *streams) cancelAll() {
	s.mu.Lock()
	s.closed = true
	for _, f := range s.fileOf {
		f.gone = true
		s.pump(f)
	}
	live := make([]*stream, 0, len(s.live))
	for st := range s.live {
		live = append(live, st)
	}
	s.mu.Unlock()
	s.files.cancelAll()
	s.reg.cancelAll()
	for _, st := range live {
		st.end(true)
	}
}

// wait waits for core mode's goroutines (connects, reads, writes, lingers, resolutions, file
// work) to end, bound at most, cancelling what is under way first: whether they all did.
func (s *streams) wait(bound time.Duration) bool { return s.reg.close(bound) }
