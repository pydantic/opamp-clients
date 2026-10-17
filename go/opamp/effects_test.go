// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// The executor against the core's effects: every kind of cancel, and the closing handshake.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// nextReport is the executor's next report, which must go to the core.
func nextReport(t *testing.T, x *executor) report {
	t.Helper()
	select {
	case r := <-x.reports():
		if !x.deliver(r) {
			t.Fatalf("a report for a cancelled effect: %v", r.in)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no report")
		return report{}
	}
}

// nothingLive checks that for d, nothing reaches the core.
func nothingLive(t *testing.T, x *executor, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case r := <-x.reports():
			if x.deliver(r) {
				t.Fatalf("reported after its cancel: %v", r.in)
			}
		case <-deadline:
			return
		}
	}
}

func within(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: not within 5 s", what)
	}
}

func cancelEffect(id uint64) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_Cancel{Cancel: &corepb.Id{Id: id}}}
}

func httpRequest(id uint64, url string) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_HttpRequest{HttpRequest: &corepb.HttpRequest{
		Id: id, Url: url, TimeoutMs: 60_000, MaxResponseBytes: 1 << 20}}}
}

func wsOpen(id uint64, url string) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_WsOpen{WsOpen: &corepb.WsOpen{Id: id, Url: url, MaxMessageBytes: 1 << 20}}}
}

func wsClose(id uint64) *corepb.Effect {
	return &corepb.Effect{Kind: &corepb.Effect_WsClose{WsClose: &corepb.Id{Id: id}}}
}

// covers: RULE-CLIENT-15
func TestACancelledRequestIsAbortedAndNeverReported(t *testing.T) {
	arrived, aborted := make(chan struct{}), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			return
		}
		close(arrived)
		<-r.Context().Done()
		close(aborted)
	}))
	defer ts.Close()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.do(httpRequest(1, ts.URL+"/slow"))
	within(t, "the request", arrived)
	x.do(cancelEffect(1))
	within(t, "the abort", aborted)
	nothingLive(t, x, 200*time.Millisecond)

	// A result already on its way when the core cancels it doesn't reach the core either.
	x.do(httpRequest(2, ts.URL+"/fast"))
	r := <-x.reports()
	x.do(cancelEffect(2))
	if x.deliver(r) {
		t.Fatalf("a result queued before the cancel reached the core: %v", r.in)
	}
	if len(x.fl.entries) != 0 {
		t.Fatalf("still under way: %v", x.fl.entries)
	}
}

// A connection being opened (a server that never answers the upgrade) is aborted at once.
// covers: RULE-CLIENT-15
func TestACancelledConnectingIsAborted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted, gone := make(chan struct{}), make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		_, _ = io.Copy(io.Discard, c) // the upgrade request, then nothing until the client goes
		close(gone)
	}()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.do(wsOpen(3, "ws://"+ln.Addr().String()+"/v1/opamp"))
	within(t, "the connection", accepted)
	x.do(cancelEffect(3))
	within(t, "the connection closed", gone)
	nothingLive(t, x, 200*time.Millisecond)
}

// wsServer runs handle on every WebSocket connection.
func wsServer(t *testing.T, handle func(*websocket.Conn)) string {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		handle(c)
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/opamp"
}

func openWS(t *testing.T, x *executor, id uint64, url string) {
	t.Helper()
	x.do(wsOpen(id, url))
	if r := nextReport(t, x); r.in.GetWsOpened().GetId() != id || r.last {
		t.Fatalf("got %v", r.in)
	}
}

// An open connection is dropped at once: no Close frame.
// covers: RULE-CLIENT-15
func TestACancelledConnectionIsDropped(t *testing.T) {
	ended := make(chan error, 1)
	url := wsServer(t, func(c *websocket.Conn) {
		_, _, err := c.ReadMessage()
		ended <- err
	})
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	openWS(t, x, 4, url)
	x.do(cancelEffect(4))
	select {
	case err := <-ended:
		if !websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
			t.Fatalf("the server saw %v, not the connection dropped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection is still open")
	}
	nothingLive(t, x, 200*time.Millisecond)
}

// A connection closing (the server never answers our Close) is dropped at once.
// covers: RULE-CLIENT-15
func TestACancelledClosingIsDropped(t *testing.T) {
	gotClose, dropped := make(chan struct{}), make(chan struct{})
	url := wsServer(t, func(c *websocket.Conn) {
		c.SetCloseHandler(func(int, string) error { return nil }) // never answers
		if _, _, err := c.ReadMessage(); websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			close(gotClose)
		}
		_, _ = io.Copy(io.Discard, c.NetConn())
		close(dropped)
	})
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	openWS(t, x, 5, url)
	x.do(wsClose(5))
	within(t, "our Close", gotClose)
	nothingLive(t, x, 200*time.Millisecond) // the handshake isn't done: nothing to report yet
	x.do(cancelEffect(5))
	within(t, "the connection dropped", dropped)
	nothingLive(t, x, 200*time.Millisecond)
}

// After ws_close, the connection is reported closed once the closing handshake is done: ours
// answered, or the server's answered.
// covers: OPAMP-CONN-06, OPAMP-CONN-07
func TestWsClosedAfterTheHandshake(t *testing.T) {
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))

	// Ours, answered by the server (gorilla answers a Close itself).
	url := wsServer(t, func(c *websocket.Conn) {
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(kind, data)
		}
	})
	openWS(t, x, 6, url)
	x.do(&corepb.Effect{Kind: &corepb.Effect_WsSend{WsSend: &corepb.WsMessage{Id: 6, Data: []byte{0, 1, 2}}}})
	if r := nextReport(t, x); !bytes.Equal(r.in.GetWsMessage().GetData(), []byte{0, 1, 2}) || r.last {
		t.Fatalf("got %v", r.in)
	}
	x.do(wsClose(6))
	if r := nextReport(t, x); r.in.GetWsClosed().GetId() != 6 || r.in.GetWsClosed().GetCode() != 1000 || !r.last {
		t.Fatalf("got %v (last %v)", r.in, r.last)
	}

	// The server's, answered when the core says.
	answered := make(chan struct{})
	url = wsServer(t, func(c *websocket.Conn) {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "bye"), time.Time{})
		if _, _, err := c.ReadMessage(); websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			close(answered)
		}
	})
	openWS(t, x, 7, url)
	if r := nextReport(t, x); r.in.GetWsClosed().GetCode() != 1001 || r.last {
		t.Fatalf("got %v (last %v)", r.in, r.last)
	}
	x.do(wsClose(7))
	within(t, "the answer to the server's Close", answered)
	if r := nextReport(t, x); r.in.GetWsClosed().GetId() != 7 || !r.last {
		t.Fatalf("got %v (last %v)", r.in, r.last)
	}
	if len(x.fl.entries) != 0 {
		t.Fatalf("still under way: %v", x.fl.entries)
	}
}

// A connection the server drops without a Close frame is lost (ws_lost), not closed: gorilla's
// 1006 ("abnormal closure") is its own word for no Close frame, never one the server sent. (The
// differential runs found it: tcp-drop-ws, go-core against rust.)
func TestADroppedConnectionIsLostNotClosed(t *testing.T) {
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	url := wsServer(t, func(c *websocket.Conn) {
		_ = c.NetConn().Close()
	})
	openWS(t, x, 8, url)
	r := nextReport(t, x)
	if r.in.GetWsLost().GetId() != 8 || !r.last {
		t.Fatalf("a dropped connection reported as %v (last %v), not ws_lost", r.in, r.last)
	}
}

// A download is aborted and its partial file deleted.
// covers: RULE-CLIENT-15
func TestACancelledDownloadLeavesNoFile(t *testing.T) {
	aborted := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(aborted)
	}))
	defer ts.Close()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(filepath.Join(t.TempDir(), "state"))
	x.do(&corepb.Effect{Kind: &corepb.Effect_Download{Download: &corepb.Download{Id: 8, Url: ts.URL, MaxBytes: 1 << 20, TimeoutMs: 60_000}}})
	path := x.downloadPath(8)
	if !eventually(5*time.Second, func() bool { fi, err := os.Stat(path); return err == nil && fi.Size() > 0 }) {
		t.Fatal("no partial file")
	}
	x.do(cancelEffect(8))
	within(t, "the abort", aborted)
	x.work.settle()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the partial file is left: %v", err)
	}
	nothingLive(t, x, 200*time.Millisecond)
}

// holdSaves holds every save of path at its start until the returned release is called, and
// says on started when one is held.
func holdSaves(t *testing.T, path string) (started <-chan struct{}, release func()) {
	s, gate := make(chan struct{}, 16), make(chan struct{})
	old := beforeSave
	beforeSave = func(p string) {
		if p == path {
			s <- struct{}{}
			<-gate
		}
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); beforeSave = old })
	return s, release
}

// A save already writing when the core cancels it completes whole, and is never reported.
// covers: RULE-CLIENT-15
func TestACancelledSaveFinishesUnreported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	started, release := holdSaves(t, path)
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(path)
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 9, Data: []byte("saved")}}})
	<-started
	x.do(cancelEffect(9))
	release()
	x.work.settle()
	if got, err := os.ReadFile(path); err != nil || string(got) != "saved" {
		t.Fatalf("saved %q, %v", got, err)
	}
	nothingLive(t, x, 200*time.Millisecond)
}

// Stop ends when the core says, within Config.StopTimeout, whatever the server does: here it
// never answers the goodbye (HTTP) or our Close (WebSocket).
func TestStopEndsWhenTheCoreSays(t *testing.T) {
	for _, ws := range []bool{false, true} {
		var mu sync.Mutex
		requests := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if websocket.IsWebSocketUpgrade(r) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				c.SetCloseHandler(func(int, string) error { return nil })
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						_, _ = io.Copy(io.Discard, c.NetConn())
						return
					}
				}
			}
			_, _ = io.ReadAll(r.Body) // the server notices the client going only after the body
			mu.Lock()
			requests++
			n := requests
			mu.Unlock()
			if n > 1 { // the goodbye
				<-r.Context().Done()
				return
			}
			w.Write(nil)
		}))
		cfg := Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour, StopTimeout: 300 * time.Millisecond}
		if ws {
			cfg.Endpoint = "ws" + strings.TrimPrefix(ts.URL, "http")
		}
		connected := make(chan struct{}, 1)
		c, err := Start(context.Background(), cfg, Status{}, func(_ *Client, n Notification) {
			if n.Kind == Connected || n.Kind == HeartbeatInterval {
				select {
				case connected <- struct{}{}:
				default:
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if ws {
			within(t, "connected", connected)
		} else if !eventually(5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return requests >= 1 }) {
			t.Fatal("no first report")
		}
		time.Sleep(50 * time.Millisecond)
		began := time.Now()
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		took := time.Since(began)
		if took < 250*time.Millisecond || took > 3*time.Second {
			t.Fatalf("ws=%v: stop took %v, the core's stop timeout is 300 ms", ws, took)
		}
		ts.Close()
	}
}

// StatusTooLarge names the field as the core does.
func TestStatusTooLargeNamesTheField(t *testing.T) {
	srv := &server{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	big := propDescription(0)
	big.NonIdentifyingAttributes[0].Key = strings.Repeat("k", 4096)
	names := make(chan string, 4)
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour, MaxSendBytes: 1024},
		Status{Description: big}, func(_ *Client, n Notification) {
			if n.Kind == StatusTooLarge {
				names <- n.FieldName
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(context.Background())
	select {
	case name := <-names:
		if name != "agent_description" {
			t.Fatalf("named %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no StatusTooLarge")
	}
}
