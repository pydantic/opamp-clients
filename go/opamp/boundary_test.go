// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

// Regressions for what the fuzz tests (fuzz_test.go) found at the boundary between the client and
// the core: what a server, the environment or the disk says reaches the core as UTF-8 (a protobuf
// string that isn't fails the step, and stopped the client for what a server sent); a notification
// of a kind this client doesn't know is not Stopped; values past what a Go type holds saturate;
// and a negative duration in Config is no duration (as 0), as in the Node.js client.

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// mustCross fails unless in can be handed to the core (proto.Marshal checks every string is UTF-8).
func mustCross(t *testing.T, what string, in proto.Message) {
	t.Helper()
	if _, err := proto.Marshal(in); err != nil {
		t.Errorf("%s can't reach the core: %v (%v)", what, err, in)
	}
}

// A response header that isn't UTF-8 (Latin-1, say: obs-text, which HTTP allows) used to stop a
// Go-mode client: the response could not be encoded for the core.
func TestAResponseHeaderThatIsntUTF8DoesntStopTheClient(t *testing.T) {
	srv := &server{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Note", "caf\xe9")
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()
	stoppedWith := make(chan error, 1)
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour},
		Status{}, func(_ *Client, n Notification) {
			if n.Kind == Stopped {
				stoppedWith <- n.Err
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if !eventually(5*time.Second, func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.msgs) > 0 }) {
		t.Fatal("no first report")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("the client stopped on its own: %v", err)
	}
	if err := <-stoppedWith; err != nil {
		t.Fatalf("Stopped with %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if n := len(srv.msgs); n != 2 || srv.msgs[1].AgentDisconnect == nil {
		t.Fatalf("want the first report and the goodbye, got %d messages", n)
	}
}

// Every string the client puts in an input from outside the core (headers, a Location, an error's
// text naming a path, the environment's proxy) is UTF-8, bytes that aren't replaced by U+FFFD as
// Rust mode's own HTTP reader does.
func TestWhatTheClientHandsTheCoreIsUTF8(t *testing.T) {
	h := fromHTTP(http.Header{"X-Note": {"caf\xe9"}, "\xff": {"v"}})
	mustCross(t, "a header", &corepb.Input{Kind: &corepb.Input_HttpResponse{HttpResponse: &corepb.HttpResponse{Headers: h}}})
	if h[0].Value != "caf�" && h[1].Value != "caf�" {
		t.Errorf("headers %v", h)
	}
	base, _ := url.Parse("https://files.example.com/a")
	for _, given := range []string{"http://[\xe9", "\xe9:/x", "a000:\x80", "http://h/\xe9"} {
		loc, origin := location(base, given)
		mustCross(t, "a Location", &corepb.Input{Kind: &corepb.Input_DownloadResponse{DownloadResponse: &corepb.DownloadResponse{
			Location: loc, LocationOrigin: origin}}})
	}
	bad := errors.New("saving /var/lib/caf\xe9/state: no space left on device")
	mustCross(t, "a failed save", storeFailed(1, bad))
	mustCross(t, "a failed download", downloadFailed(1, bad))
	mustCross(t, "a lost connection", wsLost(1, bad))
	mustCross(t, "a failed stream", streamFailed(1, bad.Error()))
	env := func(name string) (string, bool) {
		if name == "HTTPS_PROXY" {
			return "http://caf\xe9:3128", true
		}
		return "", false
	}
	mustCross(t, "the environment's proxy", newRustProxies(Proxy{}, env).choice)
	configured := newRustProxies(Proxy{URL: "http://u:p\xe9@proxy:3128", ConnectHeader: http.Header{"X-\xe9": {"\xe9"}}}, nil)
	mustCross(t, "the configured proxy", configured.choice)
	mustCross(t, "the configured proxy, asked", configured.answer(1))

	// A download's file that can't be written, under a directory whose name isn't UTF-8 (Rust mode).
	r := newStreamRig(t, nil, &slowFS{})
	r.x.setStateFile(r.state + "-caf\xe9/missing/state")
	r.do(nil, dData(3, []byte("x")), dDone(3))
	in := r.take()
	if in.GetDownloadFailed() == nil {
		t.Fatalf("got %v", in)
	}
	mustCross(t, "a failed download's file", in)
}

// Only the core's Stopped is Stopped: a notification of a kind this client doesn't know (a newer
// core's) used to be told as Stopped, though the client went on (RULE-SHELL-01).
func TestANotificationOfAnUnknownKindIsntStopped(t *testing.T) {
	if n := fromCore(&corepb.Notification{}); n.Kind == Stopped {
		t.Fatalf("an unknown notification is %v", n)
	}
	if n := fromCore(&corepb.Notification{Kind: &corepb.Notification_Stopped{Stopped: &corepb.Empty{}}}); n.Kind != Stopped {
		t.Fatalf("stopped is %v", n)
	}
}

// A heartbeat interval or a size past what a Go type holds saturates: never negative.
func TestNotificationValuesSaturate(t *testing.T) {
	n := fromCore(&corepb.Notification{Kind: &corepb.Notification_HeartbeatIntervalMs{HeartbeatIntervalMs: math.MaxUint64}})
	if want := time.Duration(maxMillis) * time.Millisecond; n.HeartbeatInterval != want {
		t.Errorf("heartbeat interval %v, want %v", n.HeartbeatInterval, want)
	}
	n = fromCore(&corepb.Notification{Kind: &corepb.Notification_StatusTooLarge{StatusTooLarge: &corepb.StatusTooLarge{Bytes: math.MaxUint64}}})
	if n.Bytes != math.MaxInt {
		t.Errorf("bytes %d, want %d", n.Bytes, math.MaxInt)
	}
}

// A negative duration in Config is no duration: the core's default, as 0 is (the Node.js client
// clamps the same way). It used to reach the core as some 584 million years.
func TestANegativeDurationIsTheDefault(t *testing.T) {
	cc, err := coreConfig(Config{HeartbeatInterval: -time.Second, RequestTimeout: -1, ConnectTimeout: -time.Hour,
		StopTimeout: -time.Minute, VerifyTimeout: -time.Second, DownloadTimeout: math.MinInt64})
	if err != nil {
		t.Fatal(err)
	}
	for name, ms := range map[string]uint64{"heartbeat": cc.HeartbeatIntervalMs, "request": cc.RequestTimeoutMs,
		"connect": cc.ConnectTimeoutMs, "stop": cc.StopTimeoutMs, "verify": cc.VerifyTimeoutMs, "download": cc.DownloadTimeoutMs} {
		if ms != 0 {
			t.Errorf("%s: %d ms", name, ms)
		}
	}
}
