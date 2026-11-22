// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Do sends a request to the server the client uses, with the headers its OpAMP requests send (the
// application's own Authorization is replaced), and only to that server's origin.
func TestDoSendsToTheServerWithTheClientsHeaders(t *testing.T) {
	srv := &server{}
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/fleet/installations/self/uninstall/" {
			mu.Lock()
			seen = append(seen, r.Method+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-App"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()
	c, err := startFull(context.Background(), Config{Mode: ModePlatform, Endpoint: ts.URL + "/v1/opamp", HeartbeatInterval: time.Hour,
		Header: http.Header{"Authorization": {"Bearer issued-or-configured"}}}, Status{}, func(*Client, Notification) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	if !eventually(5*time.Second, func() bool { return c.Endpoint() != "" }) {
		t.Fatal("no request reached the server")
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/fleet/installations/self/uninstall/", nil)
	req.Header.Set("Authorization", "Bearer the application's own")
	req.Header.Set("X-App", "kept")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "POST Bearer issued-or-configured kept" {
		t.Fatalf("the server saw %q", got)
	}
	if req.Header.Get("Authorization") != "Bearer the application's own" {
		t.Fatal("Do changed the caller's request")
	}

	// Anywhere else is refused, and nothing is sent.
	for _, other := range []string{"http://elsewhere.example" + "/x", strings.Replace(ts.URL, "http://", "https://", 1) + "/x"} {
		req, _ := http.NewRequest(http.MethodPost, other, nil)
		if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "is not the server's origin") {
			t.Fatalf("%s: %v", other, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("a refused request reached the server: %q", seen)
	}
}

// A WebSocket endpoint's origin is its http(s) one; before any request there is no server to send to.
func TestDoTakesAWebSocketEndpointsOrigin(t *testing.T) {
	if _, err := (&Client{}).Do(&http.Request{}); !errors.Is(err, ErrNoServer) {
		t.Fatalf("zero client: %v", err)
	}
	var used atomic.Pointer[usedRequest]
	c := &Client{used: &used, http: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, "https://fleet.example:8443/v1/fleet/installations/self/uninstall/", nil)
	if _, err := c.Do(req); !errors.Is(err, ErrNoServer) {
		t.Fatalf("before any request: %v", err)
	}
	used.Store(&usedRequest{url: "wss://Fleet.Example:8443/v1/opamp", header: http.Header{"Authorization": {"Bearer k"}}})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Request.Header.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("sent %q", got)
	}
	for _, other := range []string{"http://fleet.example:8443/", "https://fleet.example/", "https://fleet.example:8444/"} {
		req, _ := http.NewRequest(http.MethodPost, other, nil)
		if _, err := c.Do(req); err == nil {
			t.Fatalf("%s was sent", other)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
