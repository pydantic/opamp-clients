// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// Start starts the OpAMP client, then composes the config the collector starts on; the server can
// send a remote config in between. That config is the one the collector runs, and its apply is
// the one reported. Start's compose overwrote it (with no remote config, or the one the client
// resumed), so the collector ran without it and its apply never reported: APPLYING for good.
func TestARemoteConfigSentAsTheClientStartsIsKept(t *testing.T) {
	cfg := setupSupervisorConfig(t, configTemplate)
	start := func(resumed opamp.Resumed) *Supervisor {
		s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
		require.NoError(t, err)
		// What Start sets up before composing a config.
		s.persistentState, err = s.clientPersistentState()
		require.NoError(t, err)
		s.resumed = resumed
		s.agentDescription.Store(&protobufs.AgentDescription{})
		s.opampClient = &mockOpAMPClient{setRemoteConfigStatusFunc: func(*protobufs.RemoteConfigStatus) error { return nil }}
		return s
	}
	const pipeline = "exporters: {nop: }\nservice: {pipelines: {logs: {receivers: [nop], exporters: [nop]}}}\n"
	sent := policyConfig("sent", "receivers: {nop: }\n"+pipeline)
	older := policyConfig("older", "receivers: {nop: , nop/2: }\n"+pipeline)
	current := func(s *Supervisor) []byte {
		return s.cfgState.Load().(*configState).remote.GetConfigHash()
	}

	// The server's config composes only once Start's compose is done, not in the middle of it.
	s := start(opamp.Resumed{})
	s.configWriteMu.Lock()
	done := make(chan bool)
	go func() { done <- s.processRemoteConfigMessage(t.Context(), sent) }()
	select {
	case <-done:
		t.Fatal("the remote config was composed while Start composed the collector's")
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, s.loadAndWriteInitialMergedConfig())
	s.configWriteMu.Unlock()
	require.True(t, <-done)
	require.Equal(t, []byte("sent"), current(s))
	require.Equal(t, []byte("sent"), s.remoteConfig.Load().GetConfigHash())

	// Composed before Start's, it is the one Start composes: not the one the client resumed.
	for _, resumed := range []opamp.Resumed{{}, {RemoteConfig: older}, {RemoteConfig: older, AppliedRemoteConfig: older}} {
		s = start(resumed)
		require.True(t, s.processRemoteConfigMessage(t.Context(), sent))
		require.NoError(t, s.loadAndWriteInitialMergedConfig())
		require.Equal(t, []byte("sent"), current(s))
		require.Equal(t, []byte("sent"), s.remoteConfig.Load().GetConfigHash())
	}
}
