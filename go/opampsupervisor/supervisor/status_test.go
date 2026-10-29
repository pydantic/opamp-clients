// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// The client keeps the status file up to date as it goes: the connection (a run of failures
// from its first, then connected), the collector's health and the remote config's status, with
// no credentials in it.
func TestTheStatusFileFollowsTheClient(t *testing.T) {
	dir := t.TempDir()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	clock := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	c.report.now = func() time.Time { return clock }
	c.reach = newReachability(c.logger, "wss://user:secret@opamp.example.com/v1/opamp")
	c.report.started(c.logger, c.reach.endpoint, [16]byte{1})

	c.onNotification(nil, opamp.Notification{Kind: opamp.ConnectFailed, Status: 401})
	clock = clock.Add(time.Minute)
	c.onNotification(nil, opamp.Notification{Kind: opamp.ConnectFailed, Status: 401})
	st, err := ReadStatus(dir)
	require.NoError(t, err)
	require.NotNil(t, st.Server)
	assert.False(t, st.Server.Connected)
	assert.Equal(t, 401, st.Server.HTTPStatus)
	assert.Equal(t, clock.Add(-time.Minute), st.Server.Since, "since the first failure of the run")
	assert.False(t, st.OK())

	var out bytes.Buffer
	st.Print(&out, clock)
	assert.Contains(t, out.String(), "can't reach it since")
	assert.Contains(t, out.String(), "server.headers", "what to check")

	c.onNotification(nil, opamp.Notification{Kind: opamp.Connected})
	require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
	require.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte{0xab, 0xcd}, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
	}))
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.True(t, st.Server.Connected)
	assert.Equal(t, "abcd", st.RemoteConfig.Hash)
	assert.Equal(t, "APPLIED", st.RemoteConfig.Status)
	assert.True(t, st.OK(), "this process wrote it, and runs")

	require.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte{0xef}, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
		ErrorMessage: "receiver \"filelog\" reads \"/etc/shadow\"",
	}))
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.False(t, st.OK(), "a FAILED remote config isn't well")
	out.Reset()
	st.Print(&out, clock)
	assert.Contains(t, out.String(), `FAILED since`)
	assert.Contains(t, out.String(), `/etc/shadow`)
	assert.Contains(t, out.String(), "opamp.example.com/v1/opamp: connected since")

	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "secret", "no credentials in the status file")
	if fi, err := os.Stat(filepath.Join(dir, StatusFile)); assert.NoError(t, err) && runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "the Supervisor's user's alone")
	}

	c.report.close(c.logger)
	c.onNotification(nil, opamp.Notification{Kind: opamp.ConnectFailed, Detail: "late"})
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.False(t, st.Running())
	assert.True(t, st.Server.Connected, "as it was: nothing changes it after close")
	out.Reset()
	st.Print(&out, clock)
	assert.Contains(t, out.String(), "stopped")
}

// A status left by a process that is gone says so, and isn't well.
func TestAStatusFromAGoneProcessIsNotRunning(t *testing.T) {
	st := &Status{PID: 1 << 30, Server: &ServerStatus{Connected: true}, Collector: &CollectorStatus{Healthy: true}}
	assert.False(t, st.Running())
	assert.False(t, st.OK())
	var out bytes.Buffer
	st.Print(&out, time.Now())
	assert.Contains(t, out.String(), "not running")
	assert.Contains(t, out.String(), "none received")
}

// An unhealthy Collector with no reason reads as waiting for its config, or unhealthy, never
// with a dangling ": " (Fleet e2e 2026-10-28).
func TestAnUnhealthyCollectorWithoutAReason(t *testing.T) {
	now := time.Now()
	st := &Status{PID: os.Getpid(), Collector: &CollectorStatus{Since: now}}
	var out bytes.Buffer
	st.Print(&out, now)
	assert.Contains(t, out.String(), "waiting for its first remote config since")
	st.RemoteConfig = &RemoteConfigState{Hash: "abc", Status: "APPLYING", Since: now}
	out.Reset()
	st.Print(&out, now)
	assert.Contains(t, out.String(), "unhealthy since")
	assert.NotContains(t, out.String(), "ago): ")
}

// A nil report (a client made without newCoreClient) does nothing.
func TestANilStatusReportDoesNothing(t *testing.T) {
	var r *statusReport
	r.connected(nil)
	r.stopped(nil)
	r.close(nil)
}
