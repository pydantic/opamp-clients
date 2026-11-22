// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opamp/collector"
)

// A config the server sent that FAILED (the policy refused it) stays the status, though the
// Supervisor then reports the config the collector still runs (at a restart, or a retarget): the
// status says which config runs instead. A later config the server sends is the status again.
func TestARefusedConfigStaysFailedWhileTheLastOneRuns(t *testing.T) {
	const (
		applied  = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
		applying = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING
		failed   = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
	)
	old, refused, next := remoteConfig("receivers: {old: {}}"), remoteConfig("receivers: {refused: {}}"), remoteConfig("receivers: {next: {}}")
	hash := func(rc *protobufs.AgentRemoteConfig) string { return hex.EncodeToString(rc.GetConfigHash()) }
	setup := func(t *testing.T) (*coreClient, string) {
		dir := t.TempDir()
		c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
		c.reach = newReachability(c.logger, "wss://opamp.example.com/v1/opamp")
		c.report.started(c.logger, c.reach.endpoint, [16]byte{1})
		c.onNotification(nil, collector.Notification{Notification: opamp.Notification{Kind: opamp.Connected}})
		c.report.accepted(c.logger)
		require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
		return c, dir
	}
	send := func(c *coreClient, rc *protobufs.AgentRemoteConfig) {
		c.onNotification(nil, collector.Notification{Notification: opamp.Notification{Kind: opamp.RemoteConfig, RemoteConfig: rc}})
	}
	report := func(t *testing.T, c *coreClient, rc *protobufs.AgentRemoteConfig, s protobufs.RemoteConfigStatuses, msg string) {
		require.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: rc.GetConfigHash(), Status: s, ErrorMessage: msg}))
	}
	read := func(t *testing.T, dir string) *Status {
		st, err := ReadStatus(dir)
		require.NoError(t, err)
		return st
	}
	const why = `the remote config: receiver "filelog" has what the policy refuses`

	for name, steps := range map[string]func(*testing.T, *coreClient){
		// The Supervisor reports the config it starts on after the server's refused one: as on
		// a host enrolled again with a new key, whose server sends its config first.
		"refused, then the running one reported": func(t *testing.T, c *coreClient) {
			send(c, refused)
			report(t, c, refused, failed, why)
			report(t, c, old, applied, "")
		},
		// The running one was reported first.
		"the running one, then refused": func(t *testing.T, c *coreClient) {
			report(t, c, old, applied, "")
			send(c, refused)
			report(t, c, refused, failed, why)
			report(t, c, old, applied, "") // a retarget says it again
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, dir := setup(t)
			steps(t, c)
			st := read(t, dir)
			require.NotNil(t, st.RemoteConfig)
			assert.Equal(t, hash(refused), st.RemoteConfig.Hash)
			assert.Equal(t, "FAILED", st.RemoteConfig.Status)
			assert.Equal(t, why, st.RemoteConfig.Error)
			require.NotNil(t, st.RunningConfig)
			assert.Equal(t, hash(old), st.RunningConfig.Hash)
			assert.Equal(t, "APPLIED", st.RunningConfig.Status)
			assert.False(t, st.OK())
			var out bytes.Buffer
			st.Print(&out, time.Now())
			assert.Contains(t, out.String(), "Remote config  FAILED since")
			assert.Contains(t, out.String(), why)
			assert.Contains(t, out.String(), "running instead: the config with hash "+short(hash(old))+" (APPLIED since")

			// The server's next config is the status again.
			send(c, next)
			report(t, c, next, applying, "")
			report(t, c, next, applied, "")
			st = read(t, dir)
			assert.Equal(t, hash(next), st.RemoteConfig.Hash)
			assert.Equal(t, "APPLIED", st.RemoteConfig.Status)
			assert.Nil(t, st.RunningConfig)
			assert.True(t, st.OK())
		})
	}

	// Before the server sent a config, the one the Supervisor starts on is the status, as before.
	c, dir := setup(t)
	report(t, c, old, applied, "")
	st := read(t, dir)
	assert.Equal(t, hash(old), st.RemoteConfig.Hash)
	assert.Nil(t, st.RunningConfig)
	assert.True(t, st.OK(), "all is well here: what the refused config's status turns false")

	// A config that FAILED of itself, the server's own, with nothing running before it: no
	// "running instead".
	c, dir = setup(t)
	send(c, refused)
	report(t, c, refused, failed, why)
	st = read(t, dir)
	assert.Equal(t, "FAILED", st.RemoteConfig.Status)
	assert.Nil(t, st.RunningConfig)
	var out bytes.Buffer
	st.Print(&out, time.Now())
	assert.NotContains(t, out.String(), "running instead")
}
