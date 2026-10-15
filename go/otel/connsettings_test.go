// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/protobuf/proto"
)

const takesKeys = uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings |
	protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus)

// keyServer is an OpAMP server over HTTP that, as the reference server does, takes an enrollment
// key and hands an agent that says it takes connection settings a key of its own, in its answer
// to the agent's first message with the enrollment key; once the agent uses its own key, the
// enrollment key speaks for it no more (401).
type keyServer struct {
	*httptest.Server
	mu       sync.Mutex
	caps     []uint64 // each message's capabilities
	auth     []string // each request's Authorization
	statuses []*protobufs.ConnectionSettingsStatus
	ownUsed  bool
}

const offerHash = "own-key-offer"

func newKeyServer(t *testing.T) *keyServer {
	t.Helper()
	k := &keyServer{}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var m protobufs.AgentToServer
		if err != nil || proto.Unmarshal(body, &m) != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		auth := r.Header.Get("Authorization")
		k.mu.Lock()
		defer k.mu.Unlock()
		k.auth = append(k.auth, auth)
		if auth != "Bearer enroll" && auth != "Bearer own" || auth == "Bearer enroll" && k.ownUsed {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		k.ownUsed = k.ownUsed || auth == "Bearer own"
		k.caps = append(k.caps, m.GetCapabilities())
		if s := m.GetConnectionSettingsStatus(); s != nil {
			k.statuses = append(k.statuses, s)
		}
		reply := &protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid()}
		if auth == "Bearer enroll" && m.GetCapabilities()&takesKeys == takesKeys {
			reply.ConnectionSettings = &protobufs.ConnectionSettingsOffers{Hash: []byte(offerHash),
				Opamp: &protobufs.OpAMPConnectionSettings{
					DestinationEndpoint: k.URL + "/v1/opamp",
					Headers: &protobufs.Headers{Headers: []*protobufs.Header{
						{Key: "Authorization", Value: "Bearer own"}}},
					HeartbeatIntervalSeconds: 1,
				}}
		}
		out, _ := proto.Marshal(reply)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(k.Close)
	return k
}

// applied says whether the agent has said APPLIED of the key's offer, and on its own key.
func (k *keyServer) applied() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, s := range k.statuses {
		if s.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED &&
			string(s.GetLastConnectionSettingsHash()) == offerHash {
			return k.ownUsed
		}
	}
	return false
}

func (k *keyServer) startAgent(t *testing.T, stateDir string) *agent {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer enroll")
	a, err := newAgent(context.Background(), &server{endpoint: k.URL + "/v1/opamp", header: header,
		stateDir: stateDir, heartbeat: 100 * time.Millisecond}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.layer.Close(context.Background()) })
	a.start(context.Background(), resource.Empty())
	if a.client == nil {
		t.Fatal("the client did not start")
	}
	return a
}

func stopAgent(t *testing.T, a *agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// With a state directory the agent says it takes connection settings, takes the key the server
// offers, says APPLIED on it, and keeps it: the next start comes with it, not the enrollment key
// configured (which the server no longer takes for it).
func TestTheAgentTakesAKeyOfItsOwnAndKeepsIt(t *testing.T) {
	k := newKeyServer(t)
	dir := t.TempDir()
	a := k.startAgent(t, dir)
	eventually(t, "the key offered was never APPLIED on it", k.applied)
	stopAgent(t, a)
	k.mu.Lock()
	if k.caps[0]&takesKeys != takesKeys {
		t.Errorf("the first message's capabilities %#x leave out connection settings (%#x)", k.caps[0], takesKeys)
	}
	before := len(k.auth)
	k.mu.Unlock()

	next := k.startAgent(t, dir)
	eventually(t, "the next start never reached the server", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return len(k.auth) > before
	})
	stopAgent(t, next)
	k.mu.Lock()
	defer k.mu.Unlock()
	if got := k.auth[before]; got != "Bearer own" {
		t.Fatalf("the next start came with %q, not the key it was given", got)
	}
}

// Without a state directory a restart would lose the key: the agent doesn't say it takes one.
func TestWithoutAStateDirectoryNoKeyIsTaken(t *testing.T) {
	k := newKeyServer(t)
	a := k.startAgent(t, "")
	eventually(t, "no message reached the server", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return len(k.caps) >= 2
	})
	stopAgent(t, a)
	k.mu.Lock()
	defer k.mu.Unlock()
	for i, c := range k.caps {
		if c&takesKeys != 0 {
			t.Fatalf("message %d says it takes connection settings (%#x) with nowhere to keep them", i, c)
		}
	}
	for _, auth := range k.auth {
		if auth != "Bearer enroll" {
			t.Fatalf("a request came with %q: no key to take", auth)
		}
	}
}
