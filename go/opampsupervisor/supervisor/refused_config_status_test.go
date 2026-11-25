// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

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
	config := func(hash, body string) *protobufs.AgentRemoteConfig {
		rc := remoteConfig(body)
		rc.ConfigHash = []byte(hash)
		return rc
	}
	old, refused, next := config("old-config", "receivers: {old: {}}"), config("refused-config", "receivers: {refused: {}}"), config("next-config", "receivers: {next: {}}")
	hash := func(rc *protobufs.AgentRemoteConfig) string { return hex.EncodeToString(rc.GetConfigHash()) }
	setup := func(t *testing.T) (*coreClient, string) {
		dir := t.TempDir()
		c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
		c.reach = newReachability(c.logger, "wss://opamp.example.com/v1/opamp")
		c.report.started(c.logger, c.reach.endpoint, [16]byte{1})
		c.onNotification(nil, collector.Notification{Notification: opamp.Notification{Kind: opamp.Connected}})
		c.report.accepted(c.logger, 1)
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
			assert.Contains(t, out.String(), "the config before it (hash "+short(hash(old))+") runs instead: APPLIED since")

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
	// "runs instead".
	c, dir = setup(t)
	send(c, refused)
	report(t, c, refused, failed, why)
	st = read(t, dir)
	assert.Equal(t, "FAILED", st.RemoteConfig.Status)
	assert.Nil(t, st.RunningConfig)
	var out bytes.Buffer
	st.Print(&out, time.Now())
	assert.NotContains(t, out.String(), "runs instead")
}

// A Supervisor started again after a refusal: the core reports the refused config's FAILED itself,
// and the server sends it no more. The status still says it, and what runs instead once the
// Supervisor reports the config it starts on.
func TestARefusedConfigStaysFailedAcrossARestart(t *testing.T) {
	ts := newCoreTestServer(t)
	dir := t.TempDir()
	refused := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: []byte("refused-config"),
		Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, ErrorMessage: "refused by the policy"}
	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(dir, coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL:     ts.url,
		InstanceUid:        types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		Capabilities:       protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig,
		RemoteConfigStatus: refused,
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()
	require.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: []byte("old-config"),
		Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}))

	st, err := ReadStatus(dir)
	require.NoError(t, err)
	require.NotNil(t, st.RemoteConfig)
	assert.Equal(t, hex.EncodeToString([]byte("refused-config")), st.RemoteConfig.Hash)
	assert.Equal(t, "FAILED", st.RemoteConfig.Status)
	assert.Equal(t, "refused by the policy", st.RemoteConfig.Error)
	require.NotNil(t, st.RunningConfig)
	assert.Equal(t, hex.EncodeToString([]byte("old-config")), st.RunningConfig.Hash)
	assert.False(t, st.OK())
}
