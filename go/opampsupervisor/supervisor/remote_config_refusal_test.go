// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

func policyConfig(hash, body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{ConfigHash: []byte(hash), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(body)}},
	}}
}

// A remote config the policy refuses is reported failed and goes no further: the config the
// collector runs stays the current one, so composing again (own telemetry, agent identification,
// the next start) keeps working. It was kept as current, and a restart failed on it.
func TestSupervisorKeepsItsConfigWhenThePolicyRefusesOne(t *testing.T) {
	cfg := setupSupervisorConfig(t, configTemplate)
	cfg.Agent.RemoteConfigPolicy = config.RemoteConfigPolicy{AllowedComponents: map[string][]string{
		"receivers": {"nop"}, "exporters": {"nop"},
	}}
	var reported []*protobufs.RemoteConfigStatus
	start := func() *Supervisor {
		s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
		require.NoError(t, err)
		// What Start sets up before composing a config.
		s.persistentState, err = loadOrCreatePersistentState(s.persistentStateFilePath(), "", s.telemetrySettings.Logger)
		require.NoError(t, err)
		s.agentDescription.Store(&protobufs.AgentDescription{})
		s.opampClient = &mockOpAMPClient{setRemoteConfigStatusFunc: func(rcs *protobufs.RemoteConfigStatus) error {
			reported = append(reported, rcs)
			return nil
		}}
		return s
	}
	const pipeline = "exporters: {nop: }\nservice: {pipelines: {logs: {receivers: [nop], exporters: [nop]}}}\n"
	allowed := policyConfig("allowed", "receivers: {nop: }\n"+pipeline)
	refused := policyConfig("refused", "receivers: {nop: , filelog: {include: [/etc/shadow]}}\n"+pipeline)

	s := start()
	require.True(t, s.processRemoteConfigMessage(t.Context(), allowed))
	require.False(t, s.processRemoteConfigMessage(t.Context(), refused))
	last := reported[len(reported)-1]
	require.Equal(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, last.Status)
	require.Equal(t, []byte("refused"), last.LastRemoteConfigHash)
	require.Contains(t, last.ErrorMessage, `configures receiver "filelog"`)
	require.Equal(t, []byte("allowed"), s.remoteConfig.Load().GetConfigHash())
	_, err := s.composeMergedConfig(s.remoteConfig.Load())
	require.NoError(t, err)

	restarted := start()
	require.NoError(t, restarted.loadAndWriteInitialMergedConfig())
	require.Equal(t, []byte("allowed"), restarted.remoteConfig.Load().GetConfigHash())
}
