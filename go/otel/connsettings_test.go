// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
// enrollment key speaks for it no more (401). Once the agent has said APPLIED of its own key, it
// can offer the next settings (next): another key, here or at another server's endpoint.
type keyServer struct {
	*httptest.Server
	mu       sync.Mutex
	caps     []uint64 // each message's capabilities
	auth     []string // each request's Authorization
	statuses []*protobufs.ConnectionSettingsStatus
	ownUsed  bool
	// keys are the keys it takes besides the enrollment key, which it takes only if enroll is set.
	keys   []string
	enroll bool
	// next is offered in answer to each message with the key "own" once the agent has said
	// APPLIED of it (nil: nothing).
	next *protobufs.ConnectionSettingsOffers
	// retires: once a request comes with this key (the next one, at the same endpoint), "own"
	// speaks for the agent no more, as the enrollment key doesn't once "own" is used.
	retires string
	ownGone bool
}

const offerHash = "own-key-offer"

func newKeyServer(t *testing.T) *keyServer {
	t.Helper()
	k := &keyServer{keys: []string{"own"}, enroll: true}
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
		key, _ := strings.CutPrefix(auth, "Bearer ")
		taken := auth == "Bearer enroll" && k.enroll && !k.ownUsed ||
			slices.Contains(k.keys, key) && !(key == "own" && k.ownGone)
		if !taken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		k.ownUsed = k.ownUsed || key == "own"
		k.ownGone = k.ownGone || k.retires != "" && key == k.retires
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
		} else if key == "own" && k.next != nil && k.saidLocked(settingsApplied, offerHash) {
			reply.ConnectionSettings = k.next
		}
		out, _ := proto.Marshal(reply)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(k.Close)
	return k
}

// newOtherServer is a server that takes key alone: one an agent is moved to.
func newOtherServer(t *testing.T, key string) *keyServer {
	t.Helper()
	k := newKeyServer(t)
	k.keys, k.enroll = []string{key}, false
	return k
}

// offer is an offer, by hash, of key at endpoint.
func offer(hash, endpoint, key string) *protobufs.ConnectionSettingsOffers {
	return &protobufs.ConnectionSettingsOffers{Hash: []byte(hash), Opamp: &protobufs.OpAMPConnectionSettings{
		DestinationEndpoint: endpoint,
		Headers:             &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer " + key}}},
	}}
}

const (
	settingsApplied = protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED
	settingsFailed  = protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED
)

// said says whether the agent has reported status of the offer by hash here.
func (k *keyServer) said(status protobufs.ConnectionSettingsStatuses, hash string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.saidLocked(status, hash)
}

func (k *keyServer) saidLocked(status protobufs.ConnectionSettingsStatuses, hash string) bool {
	for _, s := range k.statuses {
		if s.GetStatus() == status && string(s.GetLastConnectionSettingsHash()) == hash {
			return true
		}
	}
	return false
}

// requests is how many requests have come.
func (k *keyServer) requests() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.auth)
}

// authSince is the Authorization of each request from the n-th on.
func (k *keyServer) authSince(n int) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.auth[n:])
}

// applied says whether the agent has said APPLIED of the key's offer, and on its own key.
func (k *keyServer) applied() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.saidLocked(settingsApplied, offerHash) && k.ownUsed
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

// The server moves the agent: once it has its own key, A offers it another key at server B's
// endpoint. The agent goes to B with that key, says APPLIED of the move there, and keeps it:
// the next start goes to B with it first, not to A.
func TestTheAgentFollowsAMoveToAnotherServerAndKeepsIt(t *testing.T) {
	a, b := newKeyServer(t), newOtherServer(t, "moved")
	a.mu.Lock()
	a.next = offer("move-to-b", b.URL+"/v1/opamp", "moved")
	a.mu.Unlock()
	dir := t.TempDir()
	agent := a.startAgent(t, dir)
	eventually(t, "the move was never APPLIED at B", func() bool { return b.said(settingsApplied, "move-to-b") })
	// What the agent says of reaching its server names B from now on.
	if got := agent.client.Endpoint(); got != b.URL+"/v1/opamp" {
		t.Errorf("the client says it uses %q, not B", got)
	}
	stopAgent(t, agent)
	for _, auth := range b.authSince(0) {
		if auth != "Bearer moved" {
			t.Fatalf("B got a request with %q, not the key it was moved with", auth)
		}
	}

	atA, atB := a.requests(), b.requests()
	next := a.startAgent(t, dir)
	eventually(t, "the next start never reached B", func() bool { return b.requests() > atB })
	if got := a.requests(); got != atA {
		t.Errorf("the next start went to A first (%d requests), not to B", got-atA)
	}
	stopAgent(t, next)
	if got := b.authSince(atB)[0]; got != "Bearer moved" {
		t.Fatalf("the next start came to B with %q, not the key it was moved with", got)
	}
}

// The server rotates the key a second time, at the same endpoint: the agent takes "own2", says
// APPLIED of it, keeps it across a restart, and never sends "own" again.
func TestTheAgentTakesASecondKeyAndKeepsIt(t *testing.T) {
	k := newKeyServer(t)
	k.mu.Lock()
	k.keys, k.retires = []string{"own", "own2"}, "own2"
	k.next = offer("second-key", k.URL+"/v1/opamp", "own2")
	k.mu.Unlock()
	dir := t.TempDir()
	a := k.startAgent(t, dir)
	eventually(t, "the second key was never APPLIED", func() bool { return k.said(settingsApplied, "second-key") })
	from := k.requests()
	eventually(t, "the agent stopped asking", func() bool { return k.requests() > from+2 })
	stopAgent(t, a)

	next := k.startAgent(t, dir)
	at := k.requests()
	eventually(t, "the next start never reached the server", func() bool { return k.requests() > at+2 })
	stopAgent(t, next)
	for _, auth := range k.authSince(from) {
		if auth != "Bearer own2" {
			t.Fatalf("after the second key was APPLIED a request came with %q", auth)
		}
	}
}

// A move to an endpoint nobody listens on fails: the agent goes back to A with the key it has,
// says FAILED of the move there, and carries on with A.
func TestAMoveNobodyAnswersFailsAndTheAgentStaysWhereItWas(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String() + "/v1/opamp"
	_ = l.Close()
	k := newKeyServer(t)
	k.mu.Lock()
	k.next = offer("move-nowhere", closed, "moved")
	k.mu.Unlock()
	a := k.startAgent(t, t.TempDir())
	eventually(t, "the move was never FAILED", func() bool { return k.said(settingsFailed, "move-nowhere") })
	from := k.requests()
	eventually(t, "the agent did not carry on with A", func() bool { return k.requests() > from+2 })
	stopAgent(t, a)
	for _, auth := range k.authSince(from) {
		if auth != "Bearer own" {
			t.Fatalf("after the move failed a request came with %q, not the key the agent had", auth)
		}
	}
	if k.said(settingsApplied, "move-nowhere") {
		t.Fatal("the move was said APPLIED too")
	}
}
