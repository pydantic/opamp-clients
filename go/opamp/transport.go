// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// executor carries out the core's effects (HTTP requests, WebSocket connections, downloads and
// saves) and reports what happened, each report naming the effect it belongs to. It decides
// nothing about OpAMP and keeps no time of its own: the core builds every request and reads every
// response, and an effect ends with its result or the core's cancel. What is under way, and what
// reaches the core, is the register's (inFlight). do is called from the client's goroutine only.
type executor struct {
	// conn carries every request, WebSocket connection and download.
	conn *connection
	out  chan report
	done chan struct{} // closed at shutdown: nobody reads out any more
	fl   *inFlight

	// streams carries out Rust mode's effects (nil in Go mode); what it doesn't take comes here.
	streams *streams

	stateFile string      // where Store saves; "": nowhere
	sealer    StateSealer // what saves are sealed with (Config.StateSealer); nil: written as they are
	fs        fileSystem  // what saves and downloads write with: the disk, journaled if tapped
	work      *fileWork   // the saves and downloads under way on it, in the whole process
	// instance names this client's download files, so no other client on the state file (one
	// stopped before, say) can touch them.
	instance string
	lastSave chan struct{} // closed when the last save asked for has ended
	// used is the URL of the OpAMP request or connection whose outcome came last (Go mode only:
	// in Rust mode the core makes them itself).
	used atomic.Pointer[string]
}

// tried records url as the one the latest request or connection went to, before its outcome
// reaches the core: a notification of that outcome finds it in place.
func (x *executor) tried(url string) { x.used.Store(&url) }

// newExecutor carries out effects over conn. (Compression is off: the core asks for identity, and
// Go only decompresses what it asked for itself.)
func newExecutor(conn *connection) *executor {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &executor{conn: conn, out: make(chan report, 16), done: make(chan struct{}), fl: newInFlight(),
		instance: hex.EncodeToString(b), fs: osFS{}}
}

func (x *executor) reports() <-chan report { return x.out }

func (x *executor) setStateFile(path string) {
	x.stateFile = path
	if path != "" {
		x.work = workOn(path)
	}
}

// emit hands r to the client's goroutine, unless its effect is cancelled or the client stopped.
func (x *executor) emit(r report) {
	if r.e.ctx.Err() != nil {
		return
	}
	select {
	case x.out <- r:
	case <-r.e.ctx.Done():
	case <-x.done:
	}
}

// do carries out an effect (anything but a notification). What it reports comes through
// reports(); an outcome known at once is returned instead, for the caller to queue.
func (x *executor) do(eff *corepb.Effect) *report {
	if x.streams != nil {
		if mine, back := x.streams.do(eff); mine {
			return back
		}
	}
	switch k := eff.Kind.(type) {
	case *corepb.Effect_HttpRequest:
		e := x.fl.add(k.HttpRequest.Id)
		x.fl.spawn(func() {
			in := x.post(e.ctx, k.HttpRequest)
			x.tried(k.HttpRequest.Url)
			x.emit(report{e: e, in: in, last: true})
		})
	case *corepb.Effect_WsOpen:
		e := x.fl.add(k.WsOpen.Id)
		e.cmds = newMailbox[*corepb.Effect]()
		x.fl.spawn(func() { x.connect(e, k.WsOpen) })
	case *corepb.Effect_WsSend:
		x.command(k.WsSend.Id, eff)
	case *corepb.Effect_WsClose:
		x.command(k.WsClose.Id, eff)
	case *corepb.Effect_Download:
		e := x.fl.add(k.Download.Id)
		if x.stateFile == "" {
			return &report{e: e, in: downloadFailed(e.id, errors.New("no state file")), last: true}
		}
		e.file = x.downloadPath(e.id)
		x.work.begin()
		x.fl.spawn(func() {
			defer x.work.end()
			in := x.download(e.ctx, k.Download)
			// The file is the core's from now on, unless the download was cancelled as it ended:
			// then it goes here, by the goroutine that wrote it.
			if in.GetDownloadResponse() != nil && !e.handOver() {
				_ = x.fs.remove(e.file)
				return
			}
			x.emit(report{e: e, in: in, last: true})
		})
	case *corepb.Effect_Discard:
		if x.stateFile != "" {
			_ = x.fs.remove(x.downloadPath(k.Discard.Id))
		}
	case *corepb.Effect_Store:
		e := x.fl.add(k.Store.Id)
		if x.stateFile == "" {
			return &report{e: e, in: storeFailed(e.id, errors.New("no state file")), last: true}
		}
		// Beside the loop, a slow disk holding up nothing, and one after another in the order
		// asked: each save waits for the one before.
		prev, done := x.lastSave, make(chan struct{})
		x.lastSave = done
		x.work.begin()
		x.fl.spawn(func() {
			defer x.work.end()
			defer close(done)
			if prev != nil {
				<-prev
			}
			if in := store(e.ctx, x.fs, x.stateFile, x.sealer, k.Store); in != nil {
				x.emit(report{e: e, in: in, last: true})
			}
		})
	case *corepb.Effect_Cancel:
		x.cancel(k.Cancel.Id)
	}
	return nil
}

// command passes a send or the close to connection id, in order; nothing if it is gone (its
// last report is on its way). After the close, the connection's next end is its last (deliver).
func (x *executor) command(id uint64, eff *corepb.Effect) {
	if e := x.fl.get(id); e != nil && e.cmds != nil {
		if eff.GetWsClose() != nil {
			e.closeAsked = true
		}
		e.cmds.put(eff)
	}
}

// cancel gives up on effect id at once, and nothing more about it reaches the core: a request, a
// connection being opened, or one open or closing, is dropped; a download too, and its file is
// deleted; a save already writing finishes whole or not at all (saveState), unreported.
// covers: RULE-CLIENT-15
func (x *executor) cancel(id uint64) {
	// A download's file is deleted by its goroutine while it is the goroutine's (never under a
	// write), here once handed over (a response the core will not take now).
	if e := x.fl.cancel(id); e != nil && e.file != "" && e.handedOver() {
		_ = x.fs.remove(e.file)
	}
}

// deliver says whether r goes to the core (inFlight.deliver). Called just before the core takes
// it, so a cancel in a step before catches what was already queued.
func (x *executor) deliver(r report) bool {
	if r.e != nil && r.e.owner != nil {
		return r.e.owner.deliver(r)
	}
	return x.fl.deliver(r)
}

// shutdown drops everything still under way (the core has cancelled it all when it stops; in Rust
// mode it has closed every stream), waits for it to end for at most bound in all, and discards
// what is still being reported. Rust mode's files still being written are deleted once their
// write in flight is over.
func (x *executor) shutdown(bound time.Duration) {
	end := time.Now().Add(bound)
	if x.streams != nil {
		x.streams.cancelAll()
	}
	x.fl.close(bound)
	if x.streams != nil {
		x.streams.wait(max(time.Until(end), 0))
	}
	close(x.done)
	x.conn.close()
}

func failed(id uint64, err error) *corepb.Failed {
	return &corepb.Failed{Id: id, Detail: lossy(err.Error())}
}

// lossy is s as UTF-8, each byte that isn't replaced by U+FFFD, as Rust mode's own HTTP reader
// takes header values (String::from_utf8_lossy). Every string the client hands the core from
// outside it (what a server sent, the environment, a path in an error) goes through it: a protobuf
// string must be UTF-8, and one that isn't fails the step, which stops the client.
func lossy(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func downloadFailed(id uint64, err error) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_DownloadFailed{DownloadFailed: failed(id, err)}}
}

func storeFailed(id uint64, err error) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_StoreFailed{StoreFailed: failed(id, err)}}
}

func wsLost(id uint64, err error) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_WsLost{WsLost: failed(id, err)}}
}

func wsClosed(id uint64, code int) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_WsClosed{WsClosed: &corepb.WsClosed{Id: id, Code: uint32(code)}}}
}

// fileWork counts the saves and downloads under way on a state file, in the whole process. A
// client stopped on the file may leave one finishing (a save completes whole or not at all; a
// cancelled download deletes its file), and the next client on the file waits for them before it
// reads the file and clears its downloads. The count lives in works only while something is under
// way, so a file no longer used leaves nothing behind.
type fileWork struct{ path string }

type fileCount struct {
	n    int
	idle sync.Cond
}

var (
	worksMu sync.Mutex
	works   = map[string]*fileCount{} // state file -> what is under way on it; never 0
)

func workOn(path string) *fileWork { return &fileWork{path: path} }

func (w *fileWork) begin() {
	worksMu.Lock()
	c := works[w.path]
	if c == nil {
		c = &fileCount{}
		c.idle.L = &worksMu
		works[w.path] = c
	}
	c.n++
	worksMu.Unlock()
}

func (w *fileWork) end() {
	worksMu.Lock()
	c := works[w.path]
	if c.n--; c.n == 0 {
		delete(works, w.path)
		c.idle.Broadcast()
	}
	worksMu.Unlock()
}

// settle waits until nothing is under way.
func (w *fileWork) settle() {
	worksMu.Lock()
	// Looked up again after every wake: work begun since the last ended has a new count.
	for c := works[w.path]; c != nil; c = works[w.path] {
		c.idle.Wait()
	}
	worksMu.Unlock()
}

// beforeSave is called at the start of every save: a seam for tests (a slow disk).
var beforeSave = func(path string) {}

// store saves s, unless it was cancelled before it began; nil if so.
func store(ctx context.Context, fsys fileSystem, path string, sealer StateSealer, s *corepb.Store) *corepb.Input {
	if ctx.Err() != nil {
		return nil
	}
	data := s.Data
	if sealer != nil {
		sealed, err := sealer.Seal(data)
		if err != nil {
			return storeFailed(s.Id, fmt.Errorf("sealing %s: %w", path, err))
		}
		data = sealed
	}
	if err := saveState(fsys, path, data); err != nil {
		return storeFailed(s.Id, err)
	}
	return &corepb.Input{Kind: &corepb.Input_Stored{Stored: &corepb.Id{Id: s.Id}}}
}

// saveState replaces what is saved at path with data, all or nothing: a new file, synced, renamed
// over the old one. Readable by the agent's user only. The recipe is docs/state-file.md's.
func saveState(fsys fileSystem, path string, data []byte) (err error) {
	if path == "" {
		return errors.New("no state file")
	}
	beforeSave(path)
	tmp := tempFile(path)
	defer func() {
		if err != nil {
			_ = fsys.remove(tmp)
			err = fmt.Errorf("saving %s: %w", path, err)
		}
	}()
	// A new file, never one left behind (which could be anyone's, with any mode).
	_ = fsys.remove(tmp)
	f, err := fsys.createNew(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := fsys.rename(tmp, path); err != nil {
		return err
	}
	// The rename itself must reach the disk before the save is stored: if the directory can't
	// be flushed, the save failed (though the file may hold it: it is whole either way).
	if _, err := fsys.syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// downloadsDir is where package files are downloaded, beside the state file.
func downloadsDir(stateFile string) string { return stateFile + ".downloads" }

// downloadPath is the file of download id, this client's.
func (x *executor) downloadPath(id uint64) string {
	return filepath.Join(downloadsDir(x.stateFile), x.instance+"-"+strconv.FormatUint(id, 10))
}

// download GETs a file into its path, whatever the status, hashing it as it goes, and reports the
// response; on failure, or once cancelled, nothing is left.
func (x *executor) download(ctx context.Context, d *corepb.Download) *corepb.Input {
	path := x.downloadPath(d.Id)
	in, made, err := x.fetch(ctx, d, path)
	if err == nil {
		err = ctx.Err() // cancelled as it finished: the file goes all the same
	}
	if err != nil {
		// Only a file it made is its to delete: never one that was there before.
		if made {
			_ = x.fs.remove(path)
		}
		return downloadFailed(d.Id, err)
	}
	return in
}

// fetch streams the response to d into path, and reports it: the status, the body's digest, and
// for a redirect where it points. A redirect is not followed: the core decides. made: whether it
// made the file.
func (x *executor) fetch(ctx context.Context, d *corepb.Download, path string) (_ *corepb.Input, made bool, err error) {
	ctx, cancel := bounded(ctx, d.TimeoutMs)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Url, nil)
	if err != nil {
		return nil, made, err
	}
	req.Header = toHTTP(d.Headers)
	resp, err := x.conn.http.RoundTrip(req)
	if err != nil {
		return nil, made, err
	}
	defer resp.Body.Close()
	if err := x.fs.mkdir(filepath.Dir(path)); err != nil && !os.IsExist(err) {
		return nil, made, err
	}
	// A new file: never one another download is writing, or one left by a run that crashed.
	f, err := x.fs.createNew(path)
	if err != nil {
		return nil, false, err
	}
	made = true
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), overLimit(resp.Body, d.MaxBytes))
	if err != nil {
		return nil, made, err
	}
	if uint64(n) > d.MaxBytes {
		return nil, made, fmt.Errorf("file over %d bytes", d.MaxBytes)
	}
	if err := f.Sync(); err != nil {
		return nil, made, err
	}
	r := &corepb.DownloadResponse{Id: d.Id, Status: uint32(resp.StatusCode), Sha256: h.Sum(nil)}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		r.Location, r.LocationOrigin = location(resp.Request.URL, resp.Header.Get("Location"))
	}
	return &corepb.Input{Kind: &corepb.Input_DownloadResponse{DownloadResponse: r}}, made, nil
}

// location is where a redirect points: its Location resolved against the URL requested, and that
// URL's origin (port 0 for the scheme's default), as net/url reads them. A Location that doesn't
// resolve goes as given, with no origin: the core refuses it. None: nothing.
func location(requested *url.URL, given string) (string, *corepb.Origin) {
	if given == "" {
		return "", nil
	}
	ref, err := url.Parse(given)
	if err != nil {
		return lossy(given), nil
	}
	u := requested.ResolveReference(ref)
	var port uint64
	if p := u.Port(); p != "" {
		// Port 0 is no port to go to, and an origin's 0 means the scheme's default.
		if port, err = strconv.ParseUint(p, 10, 16); err != nil || port == 0 {
			return lossy(given), nil
		}
	}
	if port == map[string]uint64{"http": 80, "https": 443, "ws": 80, "wss": 443, "ftp": 21}[u.Scheme] {
		port = 0
	}
	// net/url keeps some bytes that aren't UTF-8 as they came (in an opaque part, say): no place
	// the core could be told of, so it goes as unresolved.
	s, host := u.String(), u.Hostname()
	if !utf8.ValidString(s) || !utf8.ValidString(host) {
		return lossy(given), nil
	}
	return s, &corepb.Origin{Scheme: u.Scheme, Host: host, Port: uint32(port)}
}

func toHTTP(hs []*corepb.Header) http.Header {
	h := http.Header{}
	for _, kv := range hs {
		h.Add(kv.Name, kv.Value)
	}
	return h
}

// fromHTTP lists the headers in a fixed order (a map has none).
func fromHTTP(h http.Header) []*corepb.Header {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var hs []*corepb.Header
	for _, name := range names {
		for _, v := range h[name] {
			hs = append(hs, &corepb.Header{Name: lossy(name), Value: lossy(v)})
		}
	}
	return hs
}

func unixMs() uint64 { return uint64(time.Now().UnixMilli()) }

// maxMillis is the most milliseconds a time.Duration holds.
const maxMillis = uint64(math.MaxInt64 / int64(time.Millisecond))

// bounded bounds ctx by the core's timeout of ms: 0 has passed already, and one past what a
// time.Duration holds is no bound.
func bounded(ctx context.Context, ms uint64) (context.Context, context.CancelFunc) {
	if ms > maxMillis {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
}

// overLimit reads r up to one byte past max, so a body over max shows. A max past an int64 is no
// limit.
func overLimit(r io.Reader, max uint64) io.Reader {
	return io.LimitReader(r, int64(min(max, math.MaxInt64-1))+1)
}

func (x *executor) post(ctx context.Context, r *corepb.HttpRequest) *corepb.Input {
	fail := func(err error) *corepb.Input {
		return &corepb.Input{Kind: &corepb.Input_HttpFailed{HttpFailed: failed(r.Id, err)}}
	}
	ctx, cancel := bounded(ctx, r.TimeoutMs)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Url, bytes.NewReader(r.Body))
	if err != nil {
		return fail(err)
	}
	req.Header = toHTTP(r.Headers)
	resp, err := x.conn.http.RoundTrip(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(overLimit(resp.Body, r.MaxResponseBytes))
	if err != nil {
		return fail(err)
	}
	if uint64(len(body)) > r.MaxResponseBytes {
		return fail(fmt.Errorf("response over %d bytes", r.MaxResponseBytes))
	}
	return &corepb.Input{Kind: &corepb.Input_HttpResponse{HttpResponse: &corepb.HttpResponse{
		Id: r.Id, Status: uint32(resp.StatusCode), Headers: fromHTTP(resp.Header), Body: body, UnixMs: unixMs()}}}
}

// connect opens connection e, then carries out its sends and close until it ends. A cancel, at
// any point, drops it: the dial is aborted, or the connection closed without a word.
func (x *executor) connect(e *effect, o *corepb.WsOpen) {
	conn, resp, err := x.conn.dialWebSocket(e.ctx, o.Url, toHTTP(o.Headers))
	x.tried(o.Url)
	if err != nil {
		f := &corepb.WsFailed{Id: o.Id, Detail: lossy(err.Error()), UnixMs: unixMs()}
		if resp != nil {
			s := uint32(resp.StatusCode)
			f.Status = &s
			f.Headers = fromHTTP(resp.Header)
		}
		x.emit(report{e: e, in: &corepb.Input{Kind: &corepb.Input_WsFailed{WsFailed: f}}, last: true})
		return
	}
	// Cancelled while it opened, or from now on: close it.
	stop := context.AfterFunc(e.ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	// Over the limit, gorilla closes with 1009 (message too big) and the read fails. Its limit 0
	// is none, so drive checks every message too; a limit past an int64 is none.
	if o.MaxMessageBytes > 0 && o.MaxMessageBytes <= math.MaxInt64 {
		conn.SetReadLimit(int64(o.MaxMessageBytes))
	}
	// The server's Close is answered when the core says (ws_close), not by gorilla.
	conn.SetCloseHandler(func(int, string) error { return nil })
	x.emit(report{e: e, in: &corepb.Input{Kind: &corepb.Input_WsOpened{WsOpened: &corepb.Id{Id: o.Id}}}, last: false})
	x.drive(e, conn, o.MaxMessageBytes)
}

// wsRead is what the reader got: a binary message, or why reading ended.
type wsRead struct {
	data []byte
	err  error
}

// drive passes on what arrives on conn and carries out the sends and the close in the order
// given. After the close (ours, or the answer to the server's) it reports the connection closed
// when the closing handshake is done, or lost if it fails; if the connection goes first, lost.
// covers: OPAMP-CONN-06, OPAMP-CONN-07
func (x *executor) drive(e *effect, conn *websocket.Conn, maxMessage uint64) {
	id := e.id
	quit := make(chan struct{})
	defer close(quit)
	reads := make(chan wsRead)
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err == nil && kind != websocket.BinaryMessage { // text and pongs mean nothing to OpAMP
				continue
			}
			if err == nil && uint64(len(data)) > maxMessage { // as gorilla does over its limit
				msg := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "")
				_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
				data, err = nil, fmt.Errorf("a message of %d bytes, over %d", len(data), maxMessage)
			}
			select {
			case reads <- wsRead{data, err}:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	// serverClose: the code of the server's Close frame, once it came (the reader has ended).
	serverClose := -1
	for {
		select {
		case <-e.ctx.Done():
			return
		case r := <-reads:
			if r.err == nil {
				x.emit(report{e: e, in: &corepb.Input{Kind: &corepb.Input_WsMessage{WsMessage: &corepb.WsMessage{Id: id, Data: r.data}}}, last: false})
				continue
			}
			code, ok := closeCode(r.err)
			if !ok {
				x.emit(report{e: e, in: wsLost(id, r.err), last: true})
				return
			}
			// Nothing more is read; the core answers it (ws_close).
			serverClose, reads = code, nil
			x.emit(report{e: e, in: wsClosed(id, code), last: false})
		case <-e.cmds.wake:
			for _, c := range e.cmds.take() {
				if m := c.GetWsSend(); m != nil {
					if err := conn.WriteMessage(websocket.BinaryMessage, m.Data); err != nil {
						x.emit(report{e: e, in: wsLost(id, err), last: true})
						return
					}
					continue
				}
				// The close: ours, or the answer to the server's.
				msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
				if err := conn.WriteControl(websocket.CloseMessage, msg, time.Time{}); err != nil {
					x.emit(report{e: e, in: wsLost(id, err), last: true})
					return
				}
				if serverClose >= 0 {
					x.emit(report{e: e, in: wsClosed(id, serverClose), last: true})
					return
				}
				// Wait for the server's Close; what comes before it is dropped.
				for {
					select {
					case <-e.ctx.Done():
						return
					case r := <-reads:
						if r.err == nil {
							continue
						}
						if code, ok := closeCode(r.err); ok {
							x.emit(report{e: e, in: wsClosed(id, code), last: true})
						} else {
							x.emit(report{e: e, in: wsLost(id, r.err), last: true})
						}
						return
					}
				}
			}
		}
	}
}

// closeCode is the code of the server's Close frame, if err is that the reader got one. gorilla
// reports a connection gone without one as a CloseError too, with 1006 (abnormal closure, never
// sent on the wire): that is a loss.
func closeCode(err error) (int, bool) {
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
		return ce.Code, true
	}
	return 0, false
}

// mailbox holds values in order until their taker takes them. Putting never blocks.
type mailbox[T any] struct {
	mu   sync.Mutex
	vals []T
	wake chan struct{} // holds a token while vals may be non-empty
}

func newMailbox[T any]() *mailbox[T] { return &mailbox[T]{wake: make(chan struct{}, 1)} }

func (b *mailbox[T]) put(v T) {
	b.mu.Lock()
	b.vals = append(b.vals, v)
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *mailbox[T]) take() []T {
	b.mu.Lock()
	defer b.mu.Unlock()
	vals := b.vals
	b.vals = nil
	return vals
}

// size is how many values are waiting.
func (b *mailbox[T]) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.vals)
}

// first takes the n oldest values (fewer if fewer are waiting), and leaves the rest.
func (b *mailbox[T]) first(n int) []T {
	b.mu.Lock()
	defer b.mu.Unlock()
	n = min(n, len(b.vals))
	vals := b.vals[:n:n]
	b.vals = b.vals[n:]
	return vals
}
