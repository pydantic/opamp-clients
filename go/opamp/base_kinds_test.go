// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/plumb"
)

// collectorOnlyServer answers requests in turn with a restart command, and with a custom message
// for propCapability and its own custom capabilities (a command's message carries nothing else):
// what only package collector hands on.
type collectorOnlyServer struct{ n atomic.Int64 }

func (s *collectorOnlyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
	if s.n.Add(1)%2 == 0 {
		reply.Command = &protobufs.ServerToAgentCommand{Type: protobufs.CommandType_CommandType_Restart}
	} else {
		reply.CustomMessage = &protobufs.CustomMessage{Capability: propCapability, Type: "tick"}
		reply.CustomCapabilities = &protobufs.CustomCapabilities{Capabilities: []string{propCapability}}
	}
	out, _ := proto.Marshal(reply)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// The base client never hands the application a command, a custom message or the server's custom
// capabilities, though the core tells them; started as package collector's, the same client does.
func TestBaseStartTellsNoCollectorKinds(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := map[bool]string{false: "base", true: "collector"}[full]
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(&collectorOnlyServer{})
			defer ts.Close()
			cfg := clientConfig(ts, false)
			cfg.Capabilities |= uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand)
			var mu sync.Mutex
			var kinds []NotificationKind
			connected := make(chan struct{}, 1)
			notify := func(_ *Client, n Notification) {
				mu.Lock()
				kinds = append(kinds, n.Kind)
				mu.Unlock()
				if n.Kind == Connected {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			}
			var c *Client
			var err error
			if full {
				c, err = startFull(context.Background(), cfg, Status{}, notify, &protobufs.CustomCapabilities{Capabilities: []string{propCapability}})
			} else {
				c, err = Start(context.Background(), cfg, Status{}, notify)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Stop(context.Background()) }()
			<-connected
			// A few heartbeats, answered in turn.
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			seen := map[NotificationKind]bool{}
			for _, k := range kinds {
				seen[k] = true
			}
			for _, k := range []NotificationKind{Command, CustomMessage, ServerCustomCapabilities} {
				if seen[k] != full {
					t.Errorf("kind %d told: %v, want %v (kinds %v)", k, seen[k], full, kinds)
				}
			}
			if !full {
				for _, k := range kinds {
					if k >= plumb.KindCommand {
						t.Errorf("the base client told collector kind %d", k)
					}
				}
			}
		})
	}
}
