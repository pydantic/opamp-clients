// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// answeredWithin is whether c's Answered is closed within d.
func answeredWithin(c *Client, d time.Duration) bool {
	select {
	case <-c.Answered():
		return true
	case <-time.After(d):
		return false
	}
}

func startAnswered(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := startFull(context.Background(), Config{Mode: ModePlatform, Endpoint: endpoint, HeartbeatInterval: time.Hour}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return c
}

// Answered closes at the server's first answer, and only then: a ServerToAgent over HTTP (with
// the protobuf content type, or none) or WebSocket; not an error response, a page something on
// the way serves, a binary frame that isn't OpAMP, nor a WebSocket the server takes and closes
// unanswered, as one may for credentials it refuses.
func TestAnsweredSaysTheServerAnswered(t *testing.T) {
	answer, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: make([]byte, 16)})
	refusal, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: make([]byte, 16), ErrorResponse: &protobufs.ServerErrorResponse{
		Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest, ErrorMessage: "unknown key"}})
	wsSends := func(frame []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			_, _, _ = conn.ReadMessage()
		}
	}
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		ws       bool
		answered bool
	}{
		{"http protobuf", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(answer)
		}, false, true},
		{"http no content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write(answer)
		}, false, true},
		{"http page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
		}, false, false},
		{"http refused", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, false, false},
		{"http error response", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(refusal)
		}, false, false},
		{"ws answered", wsSends(append([]byte{0}, answer...)), true, true},
		{"ws error response", wsSends(append([]byte{0}, refusal...)), true, false},
		{"ws not opamp", wsSends([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}), true, false},
		{"ws closed unanswered", func(w http.ResponseWriter, r *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""), time.Now().Add(time.Second))
			_ = conn.Close()
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			endpoint := ts.URL
			if tc.ws {
				endpoint = "ws" + strings.TrimPrefix(ts.URL, "http")
			}
			c := startAnswered(t, endpoint)
			wait := 3 * time.Second
			if got := answeredWithin(c, wait); got != tc.answered {
				t.Fatalf("answered within %s: %v, want %v", wait, got, tc.answered)
			}
		})
	}
}

// In core mode the core reads the server itself: Answered is nil.
func TestAnsweredIsNilInCoreMode(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, err := startFull(context.Background(), Config{Endpoint: ts.URL, HeartbeatInterval: time.Hour}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	if c.Answered() != nil {
		t.Fatal("Answered in core mode")
	}
}
