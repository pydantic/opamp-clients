// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unreachable are endpoints a host can't reach: a name that doesn't resolve, and a port nothing listens on.
func unreachable(t *testing.T) map[string]string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return map[string]string{"doesn't resolve": "no-such-host.invalid", "refuses": l.Addr().String()}
}

func (c *coreClient) holdsInARow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enroll.holds
}

// A host whose offered endpoint it can't reach keeps the credential it was offered, and takes no other: it
// went back to its token at once, for another each time, to the server's limit. A minute is 100 ms here: the
// ten minutes of the first four tries, and what follows at the bound.
func TestAnOfferedEndpointThatCannotBeReachedTakesOneCredential(t *testing.T) {
	setHolds(t, 6*time.Second, 100*time.Millisecond, 8*time.Millisecond)
	for name, host := range unreachable(t) {
		t.Run(name, func(t *testing.T) {
			s := newEnrollServer(t)
			s.set(func(s *enrollServer) { s.dest = "ws://" + host + "/v1/opamp" })
			dir := t.TempDir()
			c := enrollHost(t, s, dir)
			require.Eventually(t, func() bool { return c.holdsInARow() >= 5 }, 60*time.Second, 20*time.Millisecond)
			time.Sleep(8 * time.Second) // past the bound, once
			offers, tokens, keys := s.seen()
			require.Equal(t, 1, offers, "another credential was taken")
			require.Equal(t, 1, tokens, "back to the token")
			require.Zero(t, keys)

			st, enrollment := hostStatus(t, dir)
			require.False(t, st.IsAccepted())
			require.False(t, st.OK())
			require.Equal(t, host, enrollment["unreachable"])
			require.EqualValues(t, 1, enrollment["offers"])
			require.NotContains(t, enrollment, "identity_taken")
			require.Contains(t, enrollment["error"], "the server offers the credential for "+host+", which can't be reached: ")
			require.NotContains(t, enrollment["error"], "key-1")
			var out bytes.Buffer
			st.Print(&out, time.Now())
			require.Contains(t, out.String(), "This host must reach "+host)
			require.Contains(t, st.Server.Error, "the endpoint the server offers can't be reached")
		})
	}
}

// The first try again is after a minute, as after a close.
func TestAnOfferedEndpointIsTriedAgainInAMinute(t *testing.T) {
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.dest = "ws://no-such-host.invalid/v1/opamp" })
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, func() bool { return c.holdsInARow() == 1 }, 15*time.Second, 20*time.Millisecond)
	_, enrollment := hostStatus(t, dir)
	at, err := time.Parse(time.RFC3339, enrollment["retry_at"].(string))
	require.NoError(t, err)
	require.InDelta(t, time.Minute.Seconds(), time.Until(at).Seconds(), 5)
}

// Once the endpoint can be reached, the host connects with the credential it kept.
func TestAnOfferedEndpointThatComesBackTakesTheKeptCredential(t *testing.T) {
	setHolds(t, time.Second, 100*time.Millisecond, 8*time.Millisecond)
	s := newEnrollServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	s.set(func(s *enrollServer) { s.dest = "ws://" + addr + "/v1/opamp" })
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, func() bool { return c.holdsInARow() >= 3 }, 30*time.Second, 20*time.Millisecond)
	l, err = net.Listen("tcp", addr)
	require.NoError(t, err)
	defer l.Close()
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", s.Listener.Addr().String())
			if err != nil {
				in.Close()
				continue
			}
			go func() { _, _ = io.Copy(out, in); out.Close() }()
			go func() { _, _ = io.Copy(in, out); in.Close() }()
		}
	}()
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 30*time.Second, 50*time.Millisecond)
	offers, tokens, _ := s.seen()
	require.Equal(t, []int{1, 1}, []int{offers, tokens})
	require.Equal(t, "Bearer key-1", s.claims()[0])
	_, enrollment := hostStatus(t, dir)
	require.Nil(t, enrollment)
}

// A credential that is tried, and refused, sends the host back to its token, and to the holds of one refused.
func TestAnOfferedEndpointThatComesBackAndRefuses(t *testing.T) {
	setHolds(t, time.Minute, 100*time.Millisecond, 8*time.Millisecond)
	s := newEnrollServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	s.set(func(s *enrollServer) { s.dest, s.refuseClaim = "ws://"+addr+"/v1/opamp", identityTakenText })
	dir := t.TempDir()
	c := enrollHost(t, s, dir)
	require.Eventually(t, func() bool { return c.holdsInARow() >= 2 }, 30*time.Second, 20*time.Millisecond)
	l, err = net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := &http.Server{Handler: s.Config.Handler}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	require.Eventually(t, func() bool { _, e := hostStatus(t, dir); return e["identity_taken"] == true }, 30*time.Second, 50*time.Millisecond)
	_, e := hostStatus(t, dir)
	require.NotContains(t, e, "unreachable")
	offers, _, keys := s.seen()
	require.LessOrEqual(t, offers, 3)
	require.Positive(t, keys)
}

// A restart of the Supervisor tries the kept credential again, and takes at most one more.
func TestAnOfferedEndpointAfterARestart(t *testing.T) {
	setHolds(t, 6*time.Second, 100*time.Millisecond, 8*time.Millisecond)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.dest = "ws://no-such-host.invalid/v1/opamp" })
	dir := t.TempDir()
	for n := 1; n <= 3; n++ {
		c := enrollHost(t, s, dir)
		require.Eventually(t, func() bool { return c.holdsInARow() >= 3 }, 30*time.Second, 20*time.Millisecond)
		require.NoError(t, c.Stop(context.Background()))
		offers, tokens, _ := s.seen()
		require.LessOrEqual(t, offers, n)
		require.LessOrEqual(t, tokens, n)
	}
}

const identityTakenText = "The OpAMP instance UID is already bound to another Fleet installation."
