// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// enrollHost starts a client that enrolls with the token at s, keeping its state in dir.
func enrollHost(t *testing.T, s *enrollServer, dir string) *coreClient {
	t.Helper()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL: s.url(), InstanceUid: types.InstanceUid(uuid.New()), Header: http.Header{"Authorization": {"Bearer token"}},
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings | protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth,
	}))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

// hostStatus is the status in dir, and its enrollment as the file holds it.
func hostStatus(t *testing.T, dir string) (*Status, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	require.NoError(t, err)
	st := &Status{}
	require.NoError(t, json.Unmarshal(b, st))
	var raw struct {
		Enrollment map[string]any `json:"enrollment"`
	}
	require.NoError(t, json.Unmarshal(b, &raw))
	return st, raw.Enrollment
}

// A host is accepted once the server has answered it on the credential it issued it.
func TestAHostIsAcceptedOnTheCredentialItWasIssued(t *testing.T) {
	s := newEnrollServer(t)
	dir := t.TempDir()
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
	st, enrollment := hostStatus(t, dir)
	require.Nil(t, enrollment)
	require.True(t, st.Server.Connected)
	offers, _, keys := s.seen()
	require.Equal(t, []int{1, 1}, []int{offers, keys}, "offers, connections with a credential")
}

// A server that issues no credentials takes the host with its first answer.
func TestAHostOfAServerThatIssuesNoCredentialsIsAccepted(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.serveToken = true })
	dir := t.TempDir()
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
}

// An answer that only offers a credential isn't taking the host: it is not accepted while the
// credential is unclaimed.
func TestAnOfferIsNotAcceptance(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.refuseClaim = "no" })
	dir := t.TempDir()
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { offers, _, _ := s.seen(); return offers > 0 }, 10*time.Second, time.Millisecond)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		st, _ := hostStatus(t, dir)
		require.False(t, st.IsAccepted(), "accepted on an offer")
	}
}

// A host whose credential the server refuses stops taking new ones: without it, it came back
// with the token at once and every time, 18 connections in 14 seconds with the server's limit at
// 16, and read as accepted. The status says how many were offered and why none is in use.
func TestAHostWhoseCredentialIsRefusedStopsTrying(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) {
		s.refuseClaim = "The OpAMP instance UID is already bound to another Fleet installation."
	})
	dir := t.TempDir()
	enrollHost(t, s, dir)
	// As long as the loop took to the server's limit.
	time.Sleep(20 * time.Second)
	offers, tokens, keys := s.seen()
	t.Logf("offers=%d connections with the token=%d with a credential=%d", offers, tokens, keys)
	require.LessOrEqual(t, offers, 2)
	require.LessOrEqual(t, tokens+keys, 4)
	st, enrollment := hostStatus(t, dir)
	require.False(t, st.IsAccepted())
	require.False(t, st.OK())
	require.NotNil(t, enrollment, "the status says nothing of the enrollment")
	require.EqualValues(t, 2, enrollment["offers"])
	require.Equal(t, true, enrollment["identity_taken"])
	require.Equal(t, "the server says this host's identity is already bound to another installation: the server closed the connection (1008: The OpAMP instance UID is already bound to another Fleet installation.)", enrollment["error"])
	at, err := time.Parse(time.RFC3339, enrollment["retry_at"].(string))
	require.NoError(t, err)
	require.InDelta(t, time.Hour.Seconds(), time.Until(at).Seconds(), 60)
	require.False(t, st.Stopped, "the Supervisor runs on")
	require.False(t, st.Server.Connected)
	require.Contains(t, st.Server.Error, "not connecting for now")
	var out bytes.Buffer
	st.Print(&out, time.Now())
	t.Log("\n" + out.String())
	require.Contains(t, out.String(), "no: the server offered this host 2 credentials of its own, none of them in use")
	require.Contains(t, out.String(), "is already bound to another installation")
	require.Contains(t, out.String(), "Not trying again before")
	require.NotContains(t, out.String(), "can't reach")
}

// A host whose enrollment key the server doesn't take (used up, say: it takes the connection and
// closes it unanswered) isn't well, whatever the connection and the Collector say.
func TestAHostWhoseKeyIsSpentIsNotWell(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.refuseToken = "The enrollment key is used up." })
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
	require.Eventually(t, func() bool { _, tokens, _ := s.seen(); return tokens > 1 }, 15*time.Second, 10*time.Millisecond)
	st, enrollment := hostStatus(t, dir)
	require.False(t, st.IsAccepted())
	require.True(t, st.Collector.Healthy)
	require.True(t, st.Server.Connected)
	require.False(t, st.OK(), "it exits 0")
	require.NotNil(t, enrollment)
	require.Equal(t, true, enrollment["token_refused"])
	require.Equal(t, "the server doesn't take the credentials this host is configured with: the server closed the connection (1008: The enrollment key is used up.)", enrollment["error"])
	var out bytes.Buffer
	st.Print(&out, time.Now())
	t.Log("\n" + out.String())
	require.Contains(t, out.String(), "Accepted       no: the server doesn't take")
	require.Contains(t, out.String(), "The enrollment key may be used up")
	require.False(t, c.holding(), "the core retries it, as any refused connection")
	// A key that works after all: nothing of it is left.
	s.set(func(s *enrollServer) { s.refuseToken = "" })
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 60*time.Second, 50*time.Millisecond)
	st, enrollment = hostStatus(t, dir)
	require.Nil(t, enrollment)
	require.True(t, st.OK())
}

// The server's "too many unclaimed credentials" and its wait reach the status; the host isn't
// accepted, and stays away.
func TestTooManyUnclaimedCredentialsIsSaid(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.maxOffers = 0 })
	dir := t.TempDir()
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { _, tokens, _ := s.seen(); return tokens > 0 }, 10*time.Second, time.Millisecond)
	time.Sleep(5 * time.Second)
	_, tokens, _ := s.seen()
	require.Equal(t, 1, tokens, "connections, with a wait of an hour")
	st, enrollment := hostStatus(t, dir)
	require.False(t, st.IsAccepted())
	require.False(t, st.OK())
	require.NotNil(t, enrollment, "the status says nothing of the server's answer")
	require.Equal(t, "the server said: "+tooManyUnclaimed, enrollment["error"])
	at, err := time.Parse(time.RFC3339, enrollment["retry_at"].(string))
	require.NoError(t, err)
	require.InDelta(t, time.Hour.Seconds(), time.Until(at).Seconds(), 60)
	var out bytes.Buffer
	st.Print(&out, time.Now())
	t.Log("\n" + out.String())
	require.Contains(t, out.String(), tooManyUnclaimed)
}

// A Close for policy that doesn't say the identity is taken may pass (servers send 1008 for that
// too): the host isn't held for the hour, but for a minute, twice as long each time.
func TestAHostClosedForAnotherReasonBacksOff(t *testing.T) {
	setHolds(t, time.Hour, time.Second, time.Hour)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.refuseClaim = "credentials revoked" })
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	var waits []time.Duration
	for range 3 {
		require.Eventually(t, c.holding, 15*time.Second, time.Millisecond)
		_, enrollment := hostStatus(t, dir)
		require.Eventually(t, func() bool { _, enrollment = hostStatus(t, dir); return enrollment["retry_at"] != nil }, 2*time.Second, time.Millisecond)
		require.Nil(t, enrollment["identity_taken"])
		require.Equal(t, "the server didn't take the host with the offered credential: the server closed the connection (1008: credentials revoked)", enrollment["error"])
		at, err := time.Parse(time.RFC3339, enrollment["retry_at"].(string))
		require.NoError(t, err)
		waits = append(waits, time.Until(at).Round(time.Second))
		require.Eventually(t, func() bool { return !c.holding() }, 15*time.Second, time.Millisecond)
	}
	require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, waits)
	offers, _, _ := s.seen()
	require.LessOrEqual(t, offers, 5, "one more credential a try")
	_, held := hostStatus(t, dir)
	require.GreaterOrEqual(t, int(held["offers"].(float64)), 4, "those of the tries before too")
	// It passed: the host is taken, and nothing of it is left.
	s.set(func(s *enrollServer) { s.refuseClaim = "" })
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 30*time.Second, 20*time.Millisecond)
	_, enrollment := hostStatus(t, dir)
	require.Nil(t, enrollment)
	c.mu.Lock()
	require.Zero(t, c.enroll.holds, "the next one starts again from the first wait")
	c.mu.Unlock()
}

// The waits as they ship.
func TestHoldTimes(t *testing.T) {
	require.Equal(t, []time.Duration{time.Hour, time.Minute, 5 * time.Second}, []time.Duration{holdTaken, holdClosed, holdLost})
	require.Equal(t, time.Minute, backedOff(holdClosed, 0))
	require.Equal(t, 32*time.Minute, backedOff(holdClosed, 5))
	require.Equal(t, time.Hour, backedOff(holdClosed, 6))
	require.Equal(t, time.Hour, backedOff(holdClosed, 1000))
}

// A host whose identity was taken is tried again after the hold, with the credential it kept: it
// is taken once the server takes it, and its status set while it was held reaches the server.
func TestAHostIsTriedAgainAfterTheHold(t *testing.T) {
	setHolds(t, 3*time.Second, time.Hour, time.Hour)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) {
		s.refuseClaim = "The OpAMP instance UID is already bound to another Fleet installation."
	})
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, c.holding, 15*time.Second, 10*time.Millisecond)
	require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "set while held"}))
	s.set(func(s *enrollServer) { s.refuseClaim = "" })
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	st, enrollment := hostStatus(t, dir)
	require.Nil(t, enrollment)
	require.False(t, st.Stopped)
	require.True(t, st.Server.Connected)
	require.Eventually(t, func() bool { return s.reported("set while held") }, 5*time.Second, 10*time.Millisecond)
	// The credential it kept, first. (The core takes the last one offered since, which waited, once
	// that one is taken.)
	keys := s.claims()
	require.GreaterOrEqual(t, len(keys), 2)
	require.Equal(t, []string{"Bearer key-1", "Bearer key-1"}, keys[:2])
	require.NoError(t, c.Stop(context.Background()))
	st, _ = hostStatus(t, dir)
	require.True(t, st.Stopped)
}

// A connection with the credential that is lost isn't the server's word: the host tries the
// credential it kept again within seconds, and is taken.
func TestAHostWhoseClaimsAreLostIsTriedAgain(t *testing.T) {
	setHolds(t, time.Hour, time.Hour, time.Second)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.dropClaims = 1 })
	dir := t.TempDir()
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 30*time.Second, 20*time.Millisecond)
	offers, _, _ := s.seen()
	require.LessOrEqual(t, offers, 3)
	keys := s.claims()
	require.GreaterOrEqual(t, len(keys), 2)
	require.Equal(t, []string{"Bearer key-1", "Bearer key-1"}, keys[:2])
}

// Stop of a held client returns once its goroutines are done, and says it stopped.
func TestAHeldClientStops(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) {
		s.refuseClaim = "The OpAMP instance UID is already bound to another Fleet installation."
	})
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, c.holding, 15*time.Second, time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	require.False(t, c.holding())
	st, _ := hostStatus(t, dir)
	require.True(t, st.Stopped)
	offers, tokens, keys := s.seen()
	time.Sleep(2 * time.Second)
	o2, t2, k2 := s.seen()
	require.Equal(t, []int{offers, tokens, keys}, []int{o2, t2, k2}, "it connects no more")
}

// A host that claimed its credential before a restart is accepted on it after.
func TestAHostIsAcceptedOnItsSavedCredential(t *testing.T) {
	s := newEnrollServer(t)
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // the promotion is saved
	require.NoError(t, c.Stop(context.Background()))
	enrollHost(t, s, dir)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() && !st.Stopped }, 10*time.Second, 20*time.Millisecond)
	offers, tokens, _ := s.seen()
	require.Equal(t, []int{1, 1}, []int{offers, tokens})
}

func setHolds(t *testing.T, taken, closed, lost time.Duration) {
	a, b, c := holdTaken, holdClosed, holdLost
	holdTaken, holdClosed, holdLost = taken, closed, lost
	t.Cleanup(func() { holdTaken, holdClosed, holdLost = a, b, c })
}

// When the client is held, and how long.
func TestHoldFor(t *testing.T) {
	now := time.Now()
	closed := opamp.Standing{Offers: 1, Rounds: 1, ClaimError: "closed", ClaimClosed: true, FailedClaims: 1}
	with := func(st opamp.Standing, change func(*opamp.Standing)) opamp.Standing { change(&st); return st }
	again := with(closed, func(st *opamp.Standing) { st.OfferedAgain = true })
	for name, tc := range map[string]struct {
		st    opamp.Standing
		holds int
		want  time.Duration
	}{
		"nothing":                  {opamp.Standing{}, 0, 0},
		"offered":                  {opamp.Standing{Offers: 1, Rounds: 1}, 0, 0},
		"closed":                   {closed, 0, 0},
		"closed, offered again":    {again, 0, time.Minute},
		"after a hold":             {again, 1, 2 * time.Minute},
		"after many":               {again, 60, time.Hour},
		"identity taken":           {with(again, func(st *opamp.Standing) { st.IdentityTaken = true }), 0, time.Hour},
		"lost, offered again":      {with(again, func(st *opamp.Standing) { st.ClaimClosed = false }), 0, 5 * time.Second},
		"lost, after a hold":       {with(again, func(st *opamp.Standing) { st.ClaimClosed = false }), 2, 20 * time.Second},
		"two rounds":               {opamp.Standing{Offers: 2, Rounds: 2}, 0, 0},
		"three rounds":             {opamp.Standing{Offers: 1, Rounds: 3}, 0, time.Minute},
		"two offers failed":        {with(closed, func(st *opamp.Standing) { st.Offers, st.FailedClaims = 2, 2 }), 0, time.Minute},
		"one offer failed twice":   {with(closed, func(st *opamp.Standing) { st.FailedClaims = 2 }), 0, 0},
		"two offers, one failed":   {with(closed, func(st *opamp.Standing) { st.Offers = 2 }), 0, 0},
		"the server said to wait":  {with(again, func(st *opamp.Standing) { st.Error, st.RetryAt = "too many", now.Add(time.Minute) }), 0, 0},
		"its wait is over":         {with(again, func(st *opamp.Standing) { st.Error, st.RetryAt = "too many", now.Add(-time.Minute) }), 0, time.Minute},
		"accepted":                 {with(again, func(st *opamp.Standing) { st.Accepted = true }), 0, 0},
		"a saved credential fails": {opamp.Standing{ClaimError: "HTTP 401", ClaimClosed: true, FailedClaims: 9}, 0, 0},
	} {
		require.Equal(t, tc.want, holdFor(tc.st, tc.holds, now), name)
	}
}

// What the status says of it.
func TestEnrollmentOf(t *testing.T) {
	at := time.Now().Add(time.Minute)
	for name, tc := range map[string]struct {
		st     opamp.Standing
		before int
		want   EnrollmentState
	}{
		"nothing":       {opamp.Standing{}, 0, EnrollmentState{}},
		"held before":   {opamp.Standing{Offers: 1}, 2, EnrollmentState{Offers: 3}},
		"saved, failed": {opamp.Standing{ClaimError: "EOF"}, 0, EnrollmentState{}},
		"saved, taken":  {opamp.Standing{ClaimError: "x", ClaimClosed: true, IdentityTaken: true}, 0, EnrollmentState{IdentityTaken: true, Error: "the server says this host's identity is already bound to another installation: x"}},
		"lost":          {opamp.Standing{Offers: 1, ClaimError: "EOF"}, 0, EnrollmentState{Offers: 1, Error: "connecting with the offered credential failed: EOF"}},
		"closed":        {opamp.Standing{Offers: 1, ClaimError: "x", ClaimClosed: true}, 0, EnrollmentState{Offers: 1, Error: "the server didn't take the host with the offered credential: x"}},
		"too many": {opamp.Standing{Offers: 1, ClaimError: "x", ClaimClosed: true, Error: "too many", RetryAt: at}, 15,
			EnrollmentState{Offers: 16, Error: "the server said: too many; the server didn't take the host with the offered credential: x", RetryAt: at}},
		"dropped":           {opamp.Standing{Dropped: "x"}, 0, EnrollmentState{Error: "the server took this host, and then dropped it: x"}},
		"its own words":     {opamp.Standing{Error: "the server took this host"}, 0, EnrollmentState{Error: "the server said: the server took this host"}},
		"the token refused": {opamp.Standing{Refused: "x"}, 0, EnrollmentState{TokenRefused: true, Error: "the server doesn't take the credentials this host is configured with: x"}},
	} {
		require.Equal(t, tc.want, enrollmentOf(tc.st, tc.before), name)
	}
}
