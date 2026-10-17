// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// The adversary's harness: the client runs the
// adversary, a hostile core, instead of the real one, against a local server that does what each
// URL's query says. From a seed the adversary hands the shell effect sequences the real core never
// makes but spec/shell-contract.md allows, and reports through the shell itself: notes, calls and
// stops for the application to make, a violation of a rule as soon as it sees one, and a verdict
// before it says stopped. The harness acts on them, and checks what only it can see: callbacks one
// at a time and in order, stop bounded, nothing left open or running after stop, and the state
// file holding what was last answered stored.
//
// Seeds OPAMP_ADVERSARY_BASE (1) to OPAMP_ADVERSARY_BASE + OPAMP_ADVERSARY_SEEDS (24) - 1, or
// OPAMP_ADVERSARY_SEED alone (a replay); the module from OPAMP_ADVERSARY_WASM (default
// testdata/core/opamp_core_adversary.wasm at the repository's root).
//
// RULE-SHELL-06 runs in every seed, and each cancel production in focus (TestAdversaryCancel).
// "stop inside" calls Stop from the callback, where it returns at once, and waits for the client
// on a new goroutine. "call inside" calls the client the callback is given (the harness may not
// have stored what Start returned yet), with a health whose status is "inside": the adversary
// checks that call reaches it before anything the client takes in next (RULE-SHELL-10).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// knownBug is a production that triggers a bug this shell is known to have.
type knownBug struct {
	bug    string // the break's S-number (or the issue)
	reason string
}

// knownAdversaryBugs: the main run passes these in off; TestAdversaryKnownBugs runs each in focus
// and expects a failure. When one stops failing, the bug is fixed: take it off.
//
// None is left. Fixed: stop while opening (S1) and while downloading (S3),
// now stop_cancel's and the cancel productions' (the adversary no longer has the productions of a
// shell without cancels: stop_while_opening, stop_while_downloading, ws_hold_past_timeout), and
// wake_max, timeout_huge, limit_huge and limit_zero, each in the main run (TestAdversaryRegressions).
var knownAdversaryBugs = map[string]knownBug{}

// advTolerated: violations of a known bug that only a race triggers, so that no production can be
// put in focus to fail on it: tolerated, counted and logged (as S5 is in the callback model test).
// None is left: a ws_send whose write failed was answered ws_lost by send and again by the
// connection's reader; now one goroutine owns each connection's reports and ends with the first.
var advTolerated = []struct{ prefix, bug string }{}

const (
	advParallel  = 6                // seeds at once
	advStopBound = 15 * time.Second // stop returns within this of being asked
	advRunBound  = 90 * time.Second // the adversary asks for stop within this
	advSettle    = 3 * time.Second  // after stop: connections closed, nothing new
	advPrefix    = "opamp-adversary "

	// advArrivalGrace: how long after the harness's own marker a request or upgrade the client had
	// already sent may arrive, as in the Rust, Node, Java and .NET harnesses. The marker is a
	// request the harness sends the moment stop returns, on a connection of its own (advMark): it
	// waits for the server's accept and read as whatever the client sent before then does, so a
	// server late to both is late to the marker too. Measured from the moment stop returned, as
	// it was, the 50 ms called that lateness the client's: seed 20729380 on a loaded host had an
	// upgrade's head read 9 ms after stop returned, though the client's shutdown had waited for
	// the goroutine that wrote it, and on CI Node's server has taken 171 ms and 348 ms.
	advArrivalGrace = 50 * time.Millisecond
	// advMarkBound: the marker is answered within this of being sent (a server that never
	// answers it fails the seed, and doesn't hang it).
	advMarkBound = 30 * time.Second
)

// advCancelProds: the cancel productions, in every run's grammar.
var advCancelProds = []string{
	"cancel_download", "cancel_http", "cancel_http_done", "cancel_store", "cancel_twice",
	"cancel_ws_closing", "cancel_ws_opening", "stop_cancel",
}

// advServer is the README's local server: POST /h, GET /d and the GET /w upgrade, each doing what
// its query says, with per-run counts (run=SEED).
type advServer struct {
	quit chan struct{} // closed at teardown: held requests end
	// lag, a test's (ns): a loaded host's pause before the handler takes a request.
	lag atomic.Int64
	// late, a test's (ns): a loaded host's pause before the server accepts a connection that has
	// come.
	late atomic.Int64

	mu   sync.Mutex
	runs map[string]*advRunStats
}

type advRunStats struct {
	open     map[*websocket.Conn]struct{} // WebSocket connections open
	last     time.Time                    // when the last request or upgrade arrived
	lastWhat string                       // and what it was
	count    int                          // requests and upgrades
	marker   time.Time                    // when the harness's marker (GET /marker, sent as stop returned) arrived
}

func newAdvServer() *advServer {
	return &advServer{quit: make(chan struct{}), runs: map[string]*advRunStats{}}
}

func (s *advServer) stats(run string) *advRunStats {
	st := s.runs[run]
	if st == nil {
		st = &advRunStats{open: map[*websocket.Conn]struct{}{}}
		s.runs[run] = st
	}
	return st
}

// arrived counts a request or upgrade whose head reached the server at at.
func (s *advServer) arrived(run string, at time.Time, what string) {
	s.mu.Lock()
	st := s.stats(run)
	if at.After(st.last) {
		st.last, st.lastWhat = at, what
	}
	st.count++
	s.mu.Unlock()
}

// marked notes the harness's marker for run, whose head reached the server at at.
func (s *advServer) marked(run string, at time.Time) {
	s.mu.Lock()
	s.stats(run).marker = at
	s.mu.Unlock()
}

// afterStop: what breaks RULE-SHELL-07 in when run's requests and upgrades arrived, stop having
// returned at stopped ("": nothing does): one that arrived more than advArrivalGrace after the
// marker the harness sent then.
func (s *advServer) afterStop(run string, stopped time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats(run)
	if !st.last.After(st.marker.Add(advArrivalGrace)) {
		return ""
	}
	return fmt.Sprintf("RULE-SHELL-07: a request or upgrade (%s) arrived %v after stop returned, %v after a request the harness sent then",
		st.lastWhat, st.last.Sub(stopped).Round(time.Millisecond), st.last.Sub(st.marker).Round(time.Millisecond))
}

// advMark sends the harness's marker for run to the server at host, on a connection of its own:
// the request is written before advMark returns. The channel says how it went (nil: answered 204).
func advMark(host, run string) <-chan error {
	done := make(chan error, 1)
	c, err := net.DialTimeout("tcp", host, advMarkBound)
	if err != nil {
		done <- err
		return done
	}
	if _, err := fmt.Fprintf(c, "GET /marker?run=%s HTTP/1.1\r\nHost: %s\r\n\r\n", run, host); err != nil {
		_ = c.Close()
		done <- err
		return done
	}
	go func() {
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(advMarkBound))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("answered %s", resp.Status)
			}
		}
		done <- err
	}()
	return done
}

// counts: the run's open connections, and when its last request or upgrade arrived and what it
// was.
func (s *advServer) counts(run string) (open int, last time.Time, what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats(run)
	return len(st.open), st.last, st.lastWhat
}

// headConn is a connection the server accepted, noting when each request's head has reached the
// server: the read that brought its blank line. A request arrives then, not when the server has
// got round to parsing it and calling the handler: on a loaded host that comes later, and a
// request sent before stop returned was counted as arriving after it (as Java's harness found).
type headConn struct {
	net.Conn
	mu   sync.Mutex
	tail []byte    // the last bytes read (up to 3): a blank line may span two reads
	at   time.Time // when the head being read came whole (zero: not yet)
}

var advBlankLine = []byte("\r\n\r\n")

func (c *headConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		now := time.Now()
		c.mu.Lock()
		edge := append(slices.Clone(c.tail), b[:min(n, 3)]...)
		if c.at.IsZero() && (bytes.Contains(edge, advBlankLine) || bytes.Contains(b[:n], advBlankLine)) {
			c.at = now
		}
		t := append(c.tail, b[max(n-3, 0):n]...)
		c.tail = slices.Clone(t[max(len(t)-3, 0):])
		c.mu.Unlock()
	}
	return n, err
}

// head is when the head of the request being handled came whole; what is read from now on is
// the next one's.
func (c *headConn) head() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.at
	c.at, c.tail = time.Time{}, nil
	return at
}

type headListener struct {
	net.Listener
	srv *advServer
}

func (l headListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	time.Sleep(time.Duration(l.srv.late.Load()))
	return &headConn{Conn: c}, nil
}

type headConnKey struct{}

// advListen serves srv, each request arriving when its head came (headConn).
func advListen(srv *advServer) *httptest.Server {
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = headListener{ts.Listener, srv}
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, headConnKey{}, c)
	}
	ts.Start()
	return ts
}

// close ends every held request and open connection.
func (s *advServer) close() {
	close(s.quit)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.runs {
		for c := range st.open {
			_ = c.NetConn().Close()
		}
	}
}

// wait holds a request delay ms (-1: until the client goes, or 30 s); false if the client went.
func (s *advServer) wait(r *http.Request, delay int64) bool {
	d := time.Duration(delay) * time.Millisecond
	if delay < 0 {
		d = 30 * time.Second
	}
	if d == 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return delay >= 0
	case <-r.Context().Done():
	case <-s.quit:
	}
	return false
}

func advBody(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func (s *advServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(name string, def int64) int64 {
		if v := q.Get(name); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
		return def
	}
	time.Sleep(time.Duration(s.lag.Load()))
	run := q.Get("run")
	at := time.Now()
	if c, ok := r.Context().Value(headConnKey{}).(*headConn); ok {
		if head := c.head(); !head.IsZero() {
			at = head
		}
		// The rest of the body is this request's, not the next one's head.
		defer func() {
			_, _ = io.Copy(io.Discard, r.Body)
			c.head()
		}()
	}
	if r.URL.Path == "/marker" {
		// The harness's own, not the client's: when what was sent as stop returned got here.
		s.marked(run, at)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.arrived(run, at, r.Method+" "+r.URL.RequestURI())
	switch {
	case r.URL.Path == "/w" && websocket.IsWebSocketUpgrade(r):
		if !s.wait(r, num("delay", 0)) {
			return
		}
		if status := num("status", 0); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		s.websocket(w, r, run, num("echo", 0) == 1, num("push", 0), num("close", 0), num("drop", 0))
	case (r.URL.Path == "/h" && r.Method == http.MethodPost) || (r.URL.Path == "/d" && r.Method == http.MethodGet):
		_, _ = io.Copy(io.Discard, r.Body)
		if !s.wait(r, num("delay", 0)) {
			return
		}
		if num("drop", 0) == 1 {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		size := max(num("size", 0), 0)
		status := int(num("status", 200))
		if size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(status)
		if size > 0 {
			_, _ = w.Write(advBody(size))
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *advServer) websocket(w http.ResponseWriter, r *http.Request, run string, echo bool, push, closeMs, dropMs int64) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.stats(run).open[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.stats(run).open, conn)
		s.mu.Unlock()
	}()
	var wmu sync.Mutex
	for range push {
		wmu.Lock()
		err := conn.WriteMessage(websocket.BinaryMessage, advBody(16))
		wmu.Unlock()
		if err != nil {
			return
		}
	}
	if closeMs > 0 {
		t := time.AfterFunc(time.Duration(closeMs)*time.Millisecond, func() {
			wmu.Lock()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			wmu.Unlock()
		})
		defer t.Stop()
	}
	if dropMs > 0 {
		t := time.AfterFunc(time.Duration(dropMs)*time.Millisecond, func() { _ = conn.NetConn().Close() })
		defer t.Stop()
	}
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if echo && kind == websocket.BinaryMessage {
			wmu.Lock()
			err := conn.WriteMessage(websocket.BinaryMessage, data)
			wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// advHarness is one seed's client and what its callback saw.
type advHarness struct {
	host, run  string  // the server, and the run (run=SEED) the marker sent as stop returns is for
	raw        *advRaw // the run's raw server, for its streams (Rust mode)
	client     atomic.Pointer[Client]
	started    atomic.Bool
	inProgress atomic.Int32
	ask        chan string // "inside" or "outside": the adversary asked for stop
	stopDone   chan advStop

	mu        sync.Mutex
	fails     []string
	early     int      // callbacks before Start returned (S5, tolerated)
	tolerated []string // violations of advTolerated
	note      uint64
	verdicts  []string
	stopped   int
	stopErr   error
}

type advStop struct {
	err       error
	at        time.Time
	marked    <-chan error // how the marker sent then went (advMark)
	rawMarked <-chan error // and the raw server's (advRawMark; nil without one)
}

func (h *advHarness) fail(format string, args ...any) {
	h.fails = append(h.fails, fmt.Sprintf(format, args...))
}

// stop stops the client, as asked, and reports when it returned. The marker is sent then, before
// anything else: what the client sent before Stop returned reaches the server by the time it does
// (advArrivalGrace).
func (h *advHarness) stop(c *Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*advStopBound)
	defer cancel()
	err := c.Stop(ctx)
	at := time.Now()
	stopped := advStop{err: err, at: at, marked: advMark(h.host, h.run)}
	if h.raw != nil {
		stopped.rawMarked = advRawMark(h.raw.ln.Addr().String())
		// From here a connection the server was told not to read is read: only that tells it the
		// connection has gone.
		h.raw.release()
	}
	h.stopDone <- stopped
}

// handle: the client, once Start has returned (a call made before is held until then).
func (h *advHarness) handle() *Client {
	for {
		if c := h.client.Load(); c != nil {
			return c
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *advHarness) notify(c *Client, n Notification) {
	if h.inProgress.Add(1) > 1 {
		h.mu.Lock()
		h.fail("RULE-SHELL-01: two callbacks at once (%v)", n.Kind)
		h.mu.Unlock()
	}
	defer h.inProgress.Add(-1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started.Load() {
		h.early++ // S5: a race, tolerated
	}
	if h.stopped > 0 {
		h.fail("RULE-SHELL-01: a callback (%v) after stopped", n.Kind)
	}
	if n.Kind == Stopped {
		h.stopped++
		h.stopErr = n.Err
		return
	}
	if n.Kind != BadMessage || !strings.HasPrefix(n.Detail, advPrefix) {
		return
	}
	report := strings.TrimPrefix(n.Detail, advPrefix)
	switch {
	case strings.HasPrefix(report, "note "):
		k, err := strconv.ParseUint(strings.TrimPrefix(report, "note "), 10, 64)
		if err != nil || k != h.note+1 {
			h.fail("RULE-SHELL-01: %q after note %d", report, h.note)
		}
		if err == nil {
			h.note = k
		}
	case report == "call inside":
		// Through the client the callback is given: always from inside it.
		c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "inside"})
	case report == "call outside":
		go func() { h.handle().SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "outside"}) }()
	case report == "stop inside":
		// Stop from the callback asks for the stop and returns at once (with no handle yet, S5,
		// it is asked for once there is one); a goroutine waits for the client.
		select {
		case h.ask <- "inside":
			if c := h.client.Load(); c != nil {
				if err := c.Stop(context.Background()); err != nil {
					h.fail("RULE-SHELL-03: Stop from the callback returned %v", err)
				}
			}
			go func() { h.stop(h.handle()) }()
		default:
			h.fail("asked to stop twice")
		}
	case report == "stop outside":
		select {
		case h.ask <- "outside":
		default:
			h.fail("asked to stop twice")
		}
	case strings.HasPrefix(report, "violation "):
		for _, tol := range advTolerated {
			if strings.HasPrefix(n.Detail, tol.prefix) {
				h.tolerated = append(h.tolerated, n.Detail+" ["+tol.bug+"]")
				return
			}
		}
		h.fail("%s", n.Detail)
	case strings.HasPrefix(report, "verdict "):
		h.verdicts = append(h.verdicts, report)
	default:
		h.fail("an adversary report the harness doesn't know: %q", n.Detail)
	}
}

// advResult is one seed's outcome.
type advResult struct {
	seed      uint64
	fails     []string
	verdict   map[string]string
	early     int
	tolerated []string
}

func (r advResult) used() []string    { return advList(r.verdict["used"]) }
func (r advResult) enabled() []string { return advList(r.verdict["enabled"]) }

func advList(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// listing: each file of dir and its size and modification time.
func advListing(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			out[path] = fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano())
		}
		return nil
	})
	return out
}

// advCerts is the batch's CA, and the raw server's certificate under it (for 127.0.0.1 and
// localhost).
type advCerts struct {
	ca     []byte
	server *tls.Config
}

// advTLSName is the name the raw server's certificate is for, which the adversary starts TLS under
// (tls=NAME).
const advTLSName = "localhost"

// runAdversary runs one seed: query is the rest of the endpoint's query (off, focus, steps...).
// In Rust mode the run has a raw server of its own, which the adversary opens streams to (raw=PORT,
// tls=NAME): it hands the shell the stream effects, and the platform's as well (it plays the core,
// not the shell inside it), which reach Go mode's executor, as a core's would, through Rust mode's
// wiring, stop and shutdown.
func runAdversary(srv *advServer, host string, wasm []byte, dir string, seed uint64, query string, mode Mode, certs advCerts) advResult {
	res := advResult{seed: seed}
	run := strconv.FormatUint(seed, 10)
	h := &advHarness{host: host, run: run, ask: make(chan string, 1), stopDone: make(chan advStop, 1)}
	stateFile := filepath.Join(dir, "state.bin")
	cfg := Config{
		StateFile:  stateFile,
		Proxy:      Proxy{Direct: true},
		coreModule: wasm,
		Mode:       mode,
	}
	if mode != ModeGo {
		raw, err := newAdvRaw(certs.server)
		if err != nil {
			res.fails = append(res.fails, fmt.Sprintf("the raw server: %v", err))
			return res
		}
		defer raw.close()
		h.raw = raw
		query += fmt.Sprintf("&raw=%d&tls=%s", raw.port(), advTLSName)
		cfg.TLS = TLS{CAPEM: certs.ca}
	}
	cfg.Endpoint = fmt.Sprintf("http://%s/opamp?seed=%d%s", host, seed, query)
	c, err := Start(context.Background(), cfg, Status{}, h.notify)
	if err != nil {
		res.fails = append(res.fails, fmt.Sprintf("start: %v", err))
		return res
	}
	h.client.Store(c)
	h.started.Store(true)

	// Until the adversary asks for stop; then stop, bounded.
	var asked time.Time
	select {
	case side := <-h.ask:
		asked = time.Now()
		if side == "outside" {
			go h.stop(c)
		}
	case <-time.After(advRunBound):
		h.mu.Lock()
		h.fail("the adversary never asked for stop within %v", advRunBound)
		h.mu.Unlock()
		asked = time.Now()
		go h.stop(c)
	}
	var stopped advStop
	select {
	case stopped = <-h.stopDone:
		if took := stopped.at.Sub(asked); took > advStopBound {
			h.mu.Lock()
			h.fail("RULE-SHELL-07: stop took %v, over %v", took.Round(time.Millisecond), advStopBound)
			h.mu.Unlock()
		}
	case <-time.After(advStopBound):
		h.mu.Lock()
		h.fail("RULE-SHELL-07: stop didn't return within %v", advStopBound)
		h.mu.Unlock()
		stopped = <-h.stopDone // Stop's own context ends it
	}
	if stopped.err != nil {
		h.mu.Lock()
		h.fail("the client stopped with an error: %v", stopped.err)
		h.mu.Unlock()
	}

	// After stop: connections closed within advSettle, nothing new, no download changing.
	downloads := downloadsDir(stateFile)
	before := advListing(downloads)
	deadline := stopped.at.Add(advSettle)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	markErr := <-stopped.marked
	open, _, _ := srv.counts(run)
	after := advListing(downloads)
	if h.raw != nil {
		// The streams: every connection gone, and none made after stop returned.
		if err := <-stopped.rawMarked; err != nil {
			res.fails = append(res.fails, fmt.Sprintf("the harness's marker on the raw server failed: %v", err))
		} else if late := h.raw.afterMark(); late != "" {
			res.fails = append(res.fails, late)
		}
		if all, anonymous := h.raw.open(); all > 0 {
			res.fails = append(res.fails, fmt.Sprintf(
				"RULE-SHELL-07: %d stream connection(s) still open %v after stop returned (%d never said who they were)", all, advSettle, anonymous))
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	res.fails = append(res.fails, h.fails...)
	res.early = h.early
	res.tolerated = h.tolerated
	fail := func(format string, args ...any) { res.fails = append(res.fails, fmt.Sprintf(format, args...)) }
	if open > 0 {
		fail("RULE-SHELL-07: %d WebSocket connection(s) still open %v after stop returned", open, advSettle)
	}
	// A request the client had sent before stop returned may reach the server after it, but not
	// after the marker sent then has.
	if markErr != nil {
		fail("the harness's marker request failed: %v", markErr)
	} else if late := srv.afterStop(run, stopped.at); late != "" {
		fail("%s", late)
	}
	if !maps.Equal(before, after) {
		fail("RULE-SHELL-07: the downloads changed after stop returned: %v, then %v", before, after)
	}
	if h.stopped != 1 {
		fail("RULE-SHELL-02: stopped %d times", h.stopped)
	}
	if h.stopErr != nil {
		fail("stopped with %v", h.stopErr)
	}
	if len(h.verdicts) != 1 {
		fail("%d verdicts: %q", len(h.verdicts), h.verdicts)
		return res
	}
	res.verdict = map[string]string{}
	for _, kv := range strings.Fields(strings.TrimPrefix(h.verdicts[0], "verdict ")) {
		k, v, _ := strings.Cut(kv, "=")
		res.verdict[k] = v
	}
	if v := res.verdict["violations"]; v != strconv.Itoa(len(h.tolerated)) {
		fail("the verdict counts %s violation(s), %d of them tolerated: %s", v, len(h.tolerated), h.verdicts[0])
	}
	// RULE-SHELL-09: the state file holds what was last answered stored, or the data of a later
	// store (one cancelled, or cut off by stop, while it was writing: it lands whole, unreported).
	got, err := os.ReadFile(stateFile)
	want := res.verdict["stored"]
	wantData, _ := hex.DecodeString(want)
	later := func() bool {
		n, ok := advStoreID(got, seed)
		if want == "-" {
			return ok
		}
		m, okm := advStoreID(wantData, seed)
		return ok && okm && n > m
	}
	switch {
	case want == "-" && err == nil && !later():
		fail("RULE-SHELL-09: nothing was answered stored, and the state file holds %d bytes", len(got))
	case want == "-":
	case err != nil:
		fail("RULE-SHELL-09: stored=%s, and the state file: %v", want, err)
	case hex.EncodeToString(got) != want && !later():
		fail("RULE-SHELL-09: the state file holds %x, the last answered stored is %s", got, want)
	}
	return res
}

// advStoreID: the id of the adversary's store whose data this is ("opamp-adversary seed S store ID
// ..."), if it is one of this seed's.
func advStoreID(data []byte, seed uint64) (uint64, bool) {
	prefix := fmt.Sprintf("%sseed %d store ", advPrefix, seed)
	rest, ok := strings.CutPrefix(string(data), prefix)
	if !ok {
		return 0, false
	}
	id, _, _ := strings.Cut(rest, " ")
	n, err := strconv.ParseUint(id, 10, 64)
	return n, err == nil
}

// advSeeds: the seeds of a batch, and whether it is a replay of one.
func advSeeds(t *testing.T) (seeds []uint64, replay bool) {
	num := func(name string, def uint64) uint64 {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return n
	}
	if os.Getenv("OPAMP_ADVERSARY_SEED") != "" {
		return []uint64{num("OPAMP_ADVERSARY_SEED", 0)}, true
	}
	base, count := num("OPAMP_ADVERSARY_BASE", 1), num("OPAMP_ADVERSARY_SEEDS", 24)
	for i := range count {
		seeds = append(seeds, base+i)
	}
	return seeds, false
}

// advModule reads the adversary: OPAMP_ADVERSARY_WASM, or the one checked in at the repository's root.
func advModule(t *testing.T) []byte {
	path := os.Getenv("OPAMP_ADVERSARY_WASM")
	if path == "" {
		dir, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "testdata", "core")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				t.Fatal("no testdata/core above the test's directory: set OPAMP_ADVERSARY_WASM")
			}
			dir = parent
		}
		path = filepath.Join(dir, "testdata", "core", "opamp_core_adversary.wasm")
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the adversary module: %v", err)
	}
	return wasm
}

// advBatch runs seeds, advParallel at once, each with query, the client in mode.
func advBatch(t *testing.T, wasm []byte, seeds []uint64, query func(seed uint64) string, mode Mode) []advResult {
	ca := newTestCA(t, "adversary CA")
	_, _, pair := ca.issue(t, "raw server", true)
	certs := advCerts{ca: ca.pem, server: &tls.Config{Certificates: []tls.Certificate{pair}}}
	srv := newAdvServer()
	ts := advListen(srv)
	defer ts.Close()
	defer srv.close()
	host := strings.TrimPrefix(ts.URL, "http://")
	results := make([]advResult, len(seeds))
	sem := make(chan struct{}, advParallel)
	var wg sync.WaitGroup
	for i, seed := range seeds {
		dir := t.TempDir()
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runAdversary(srv, host, wasm, dir, seed, query(seed), mode, certs)
		})
	}
	wg.Wait()
	return results
}

func advKnownOff(except string) string {
	var off []string
	for _, p := range slices.Sorted(maps.Keys(knownAdversaryBugs)) {
		if p != except {
			off = append(off, p)
		}
	}
	if len(off) == 0 {
		return ""
	}
	return "&off=" + strings.Join(off, ",")
}

func advReplay(seed uint64, query, test string) string {
	return fmt.Sprintf("replay: OPAMP_ADVERSARY_SEED=%d go test -race -run '^%s$' ./opamp/ (endpoint query seed=%d%s)",
		seed, test, seed, query)
}

// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09, RULE-SHELL-10
func TestAdversary(t *testing.T) {
	t.Parallel() // (beside the other long ones: each spends its time on the adversary's clocks)
	for _, mode := range []Mode{ModeGo, ModeRust} {
		t.Run(modeName(mode), func(t *testing.T) { testAdversary(t, mode) })
	}
}

func testAdversary(t *testing.T, mode Mode) {
	wasm := advModule(t)
	seeds, replay := advSeeds(t)
	off := advKnownOff("")
	results := advBatch(t, wasm, seeds, func(uint64) string { return off }, mode)
	used, enabled := map[string]bool{}, map[string]bool{}
	early := 0
	for _, r := range results {
		early += r.early
		for _, v := range r.tolerated {
			t.Logf("tolerated (a race), seed %d: %s", r.seed, v)
		}
		if len(r.fails) > 0 {
			t.Errorf("seed %d failed (%s):\n  %s", r.seed, advReplay(r.seed, off, "TestAdversary/"+modeName(mode)), strings.Join(r.fails, "\n  "))
		}
		for _, p := range r.used() {
			used[p] = true
		}
		for _, p := range r.enabled() {
			enabled[p] = true
		}
	}
	if early > 0 {
		t.Logf("known break S5, tolerated: %d callbacks came before Start returned", early)
	}
	if t.Failed() {
		return
	}
	// A production that never runs tests nothing (a replay of one seed can't cover them all).
	var never []string
	for _, p := range slices.Sorted(maps.Keys(enabled)) {
		if !used[p] {
			never = append(never, p)
		}
	}
	if len(never) == 0 || replay {
		return
	}
	// A production is drawn at random, and runs only if the run's timing leaves it room, so one can
	// miss a batch by chance (with Rust mode's streams the grammar is half as large again): each
	// missed one runs in focus, on two seeds past the batch's, as in Java's and Node's harnesses.
	last := seeds[len(seeds)-1]
	var still []string
	for _, p := range never {
		query := off + "&focus=" + p
		ran := false
		for _, r := range advBatch(t, wasm, []uint64{last + 1, last + 2}, func(uint64) string { return query }, mode) {
			if len(r.fails) > 0 {
				t.Errorf("seed %d, %s in focus (%s):\n  %s", r.seed, p, advReplay(r.seed, query, "TestAdversary/"+modeName(mode)), strings.Join(r.fails, "\n  "))
			}
			ran = ran || slices.Contains(r.used(), p)
		}
		if !ran {
			still = append(still, p)
		}
	}
	if len(still) > 0 {
		t.Errorf("enabled, never used in seeds %d to %d, nor in focus in %d and %d: %v", seeds[0], last, last+1, last+2, still)
	}
}

// Each known bug's production, in focus, must still fail; when it doesn't, the bug looks fixed.
//
// covers: RULE-SHELL-07
func TestAdversaryKnownBugs(t *testing.T) {
	wasm := advModule(t)
	seeds, _ := advSeeds(t)
	const tries = 6 // seeds per bug, until one fails
	for _, prod := range slices.Sorted(maps.Keys(knownAdversaryBugs)) {
		bug := knownAdversaryBugs[prod]
		t.Run(prod, func(t *testing.T) {
			t.Parallel()
			query := advKnownOff(prod) + "&focus=" + prod
			for i := range uint64(tries) {
				seed := seeds[0] + i
				r := advBatch(t, wasm, []uint64{seed}, func(uint64) string { return query }, ModeGo)[0]
				if len(r.fails) > 0 {
					t.Logf("expected failure, %s: %s (seed %d, %s):\n  %s", bug.bug, bug.reason, seed,
						advReplay(seed, query, "TestAdversaryKnownBugs/"+prod), strings.Join(r.fails, "\n  "))
					return
				}
			}
			t.Errorf("%s looks fixed: %s in focus passed %d seeds from %d. Take %q off knownAdversaryBugs.",
				bug.bug, prod, tries, seeds[0], prod)
		})
	}
}

// RULE-SHELL-06: nothing after a cancel, a late resource released. Every cancel production is
// enabled, and each runs in focus.
//
// covers: RULE-SHELL-06
func TestAdversaryCancel(t *testing.T) {
	t.Parallel() // (beside the other long ones: each spends its time on the adversary's clocks)
	for _, mode := range []Mode{ModeGo, ModeRust} {
		t.Run(modeName(mode), func(t *testing.T) { testAdversaryCancel(t, mode) })
	}
}

func testAdversaryCancel(t *testing.T, mode Mode) {
	wasm := advModule(t)
	seeds, _ := advSeeds(t)
	probe := advBatch(t, wasm, seeds[:1], func(uint64) string { return advKnownOff("") + "&steps=3" }, mode)[0]
	if probe.verdict == nil {
		t.Fatalf("no verdict from seed %d: %v", probe.seed, probe.fails)
	}
	var cancels []string
	for _, p := range probe.enabled() {
		if strings.HasPrefix(p, "cancel_") || p == "stop_cancel" {
			cancels = append(cancels, p)
		}
	}
	for _, p := range advCancelProds {
		if !slices.Contains(cancels, p) {
			t.Errorf("%s isn't enabled (enabled: %v)", p, probe.enabled())
		}
	}
	// Each in focus beside the others (as many at once as -parallel lets, each batch with its own
	// server): one after another they were most of the package's time, each waiting on its clock.
	for _, prod := range cancels {
		t.Run(prod, func(t *testing.T) {
			t.Parallel()
			query := advKnownOff("") + "&focus=" + prod
			for _, r := range advBatch(t, wasm, seeds[:min(len(seeds), 2)], func(uint64) string { return query }, mode) {
				if len(r.fails) > 0 {
					t.Errorf("seed %d, %s in focus (%s):\n  %s", r.seed, prod, advReplay(r.seed, query, "TestAdversaryCancel"),
						strings.Join(r.fails, "\n  "))
				}
			}
		})
	}
}

// The adversary's findings in Go's shell, each fixed, each replayed in focus with the seed that
// found it (RULE-SHELL-05 and -08).
//
// covers: RULE-SHELL-05, RULE-SHELL-08
func TestAdversaryRegressions(t *testing.T) {
	wasm := advModule(t)
	for _, c := range []struct {
		prod string
		seed uint64
		bug  string
	}{
		{"wake_max", 1, "a wake_at_ms past what a time.Duration holds armed the timer negative: it fired at once"},
		{"timeout_huge", 1, "a timeout_ms past what a time.Duration holds made a deadline already passed"},
		{"limit_huge", 2, "a limit of 2^63 or more overflowed int64: nothing was read, an empty body reported"},
		{"limit_zero", 1, "a max_message_bytes of 0 took every message (gorilla's SetReadLimit(0) is no limit)"},
		{"limit_zero", 2, "the same"},
	} {
		for _, mode := range []Mode{ModeGo, ModeRust} {
			t.Run(fmt.Sprintf("%s/%s/%d", modeName(mode), c.prod, c.seed), func(t *testing.T) {
				t.Parallel()
				query := advKnownOff(c.prod) + "&focus=" + c.prod
				r := advBatch(t, wasm, []uint64{c.seed}, func(uint64) string { return query }, mode)[0]
				if len(r.fails) > 0 {
					t.Errorf("%s (%s):\n  %s", c.bug, advReplay(c.seed, query, "TestAdversaryRegressions"), strings.Join(r.fails, "\n  "))
				}
			})
		}
	}
}

// The server's own measure (seed 20729380 failed on it, 0 ms late): a request arrives when its
// head has reached the server, however long the server then takes to handle it; a head that
// comes in two parts arrives with the second, and so does the next request on a kept connection.
//
// covers: RULE-SHELL-07
func TestAdversaryArrivalIsWhenItsHeadCame(t *testing.T) {
	srv := newAdvServer()
	ts := advListen(srv)
	defer ts.Close()
	defer srv.close()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// send writes parts 300 ms apart, then reads the answer: when the last part went.
	send := func(c net.Conn, br *bufio.Reader, parts ...string) time.Time {
		var sent time.Time
		for i, p := range parts {
			if i > 0 {
				time.Sleep(300 * time.Millisecond)
			}
			sent = time.Now()
			if _, err := io.WriteString(c, p); err != nil {
				t.Fatal(err)
			}
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return sent
	}
	arrived := func(run string) time.Time {
		_, last, _ := srv.counts(run)
		return last
	}

	srv.lag.Store(int64(2 * time.Second)) // a loaded host, between the bytes and the handler
	c := dial()
	sent := send(c, bufio.NewReader(c), "GET /d?run=whole HTTP/1.1\r\nHost: x\r\n\r\n")
	if late := arrived("whole").Sub(sent); late >= time.Second {
		t.Errorf("a head sent whole counted as arriving %v after it was sent: when the handler took it", late)
	}
	srv.lag.Store(0)

	c = dial()
	sent = send(c, bufio.NewReader(c), "GET /d?run=split HTTP/1.1\r\n", "Host: x\r\n\r\n")
	if arrived("split").Before(sent) {
		t.Error("a head sent in two parts counted as arriving before its second part was sent")
	}

	// A body with a blank line in it, then, on the same connection, the next request.
	c = dial()
	br := bufio.NewReader(c)
	send(c, br, "POST /h?run=first HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\n", "\r\n\r\n")
	time.Sleep(300 * time.Millisecond)
	sent = send(c, br, "GET /d?run=next HTTP/1.1\r\nHost: x\r\n\r\n")
	if arrived("next").Before(sent) {
		t.Error("the next request on a kept connection counted as arriving before it was sent")
	}
}

// The harness's own measure of what arrived after stop (seed 20729380: an upgrade's head read
// 9 ms after stop returned, written by a goroutine the shutdown had waited for; Node's seed 5 on
// loaded CI runners: 171 ms and 348 ms): a request sent before stop returned isn't the client's to
// answer for however late the server takes it, and one sent after stop returned still is.
//
// covers: RULE-SHELL-07
func TestAdversaryArrivalIsJudgedAgainstTheMarker(t *testing.T) {
	srv := newAdvServer()
	ts := advListen(srv)
	defer ts.Close()
	defer srv.close()
	host := ts.Listener.Addr().String()
	// get sends GET /d for run on a connection of its own, before it returns; the channel closes
	// when it is answered.
	get := func(run string) <-chan struct{} {
		c, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := fmt.Fprintf(c, "GET /d?run=%s HTTP/1.1\r\nHost: x\r\n\r\n", run); err != nil {
			t.Fatal(err)
		}
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
				t.Error(err)
			} else {
				_ = resp.Body.Close()
			}
		}()
		return answered
	}

	// Sent before stop returned, to a server 400 ms late to accept anything: by the moment stop
	// returned it arrived late, by the marker it didn't.
	srv.late.Store(int64(400 * time.Millisecond))
	answered := get("late")
	stopped := time.Now()
	if err := <-advMark(host, "late"); err != nil {
		t.Fatalf("the marker: %v", err)
	}
	<-answered
	_, last, _ := srv.counts("late")
	late := last.Sub(stopped)
	if late <= advArrivalGrace {
		t.Fatalf("the server was to be late: it took the request %v after stop returned", late)
	}
	said := srv.afterStop("late", stopped)
	t.Logf("a request sent before stop returned, taken %v after it: %q", late.Round(time.Millisecond), said)
	if said != "" {
		t.Errorf("a request sent before stop returned counted as arriving after it: %s", said)
	}

	// Sent after stop returned: 100 ms after the marker sent then was answered.
	srv.late.Store(0)
	stopped = time.Now()
	if err := <-advMark(host, "after"); err != nil {
		t.Fatalf("the marker: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	<-get("after")
	said = srv.afterStop("after", stopped)
	t.Logf("a request sent 100 ms after stop returned: %q", said)
	if !strings.Contains(said, "RULE-SHELL-07") {
		t.Error("a request sent 100 ms after stop returned passed")
	}
}
