// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

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
		"receivers": {"nop", "filelog"}, "exporters": {"nop", "otlphttp"},
	}, AllowedExporterEndpoints: []string{"https://logfire-us.pydantic.dev"}, AllowedLogPaths: []string{"/var/log/app"}}
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
	refused := policyConfig("refused", "receivers: {nop: , filelog: {include: [/var/log/app/../../../etc/shadow]}}\n"+pipeline)
	elsewhere := policyConfig("elsewhere", "receivers: {nop: }\n"+
		"exporters: {nop: , otlphttp: {endpoint: \"https://collector.example.com\"}}\n"+
		"service: {pipelines: {logs: {receivers: [nop], exporters: [otlphttp]}}}\n")

	s := start()
	require.True(t, s.processRemoteConfigMessage(t.Context(), allowed))
	require.False(t, s.processRemoteConfigMessage(t.Context(), refused))
	last := reported[len(reported)-1]
	require.Equal(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, last.Status)
	require.Equal(t, []byte("refused"), last.LastRemoteConfigHash)
	require.Contains(t, last.ErrorMessage, `receiver "filelog" reads "/var/log/app/../../../etc/shadow", which could climb out with ..`)
	require.False(t, s.processRemoteConfigMessage(t.Context(), elsewhere))
	last = reported[len(reported)-1]
	require.Equal(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, last.Status)
	require.Equal(t, []byte("elsewhere"), last.LastRemoteConfigHash)
	require.Contains(t, last.ErrorMessage, `exporter "otlphttp" sends to "https://collector.example.com", which agent::remote_config_policy::allowed_exporter_endpoints doesn't allow`)
	require.Equal(t, []byte("allowed"), s.remoteConfig.Load().GetConfigHash())
	_, err := s.composeMergedConfig(s.remoteConfig.Load())
	require.NoError(t, err)

	restarted := start()
	require.NoError(t, restarted.loadAndWriteInitialMergedConfig())
	require.Equal(t, []byte("allowed"), restarted.remoteConfig.Load().GetConfigHash())
}

// Accepting remote configs with no policy is allowed (upstream's behaviour), and said at start.
func TestSupervisorWarnsWhenTheRemoteConfigIsUnrestricted(t *testing.T) {
	cfg := setupSupervisorConfig(t, configTemplate)
	cfg.Capabilities.AcceptsRemoteConfig = true
	core, logs := observer.New(zap.WarnLevel)
	_, err := NewSupervisor(t.Context(), zap.New(core), cfg)
	require.NoError(t, err)
	require.Equal(t, 1, logs.FilterMessageSnippet("remote config is unrestricted").Len())

	cfg.Agent.RemoteConfigPolicy = config.RemoteConfigPolicy{AllowedComponents: map[string][]string{"receivers": {"nop"}}}
	core, logs = observer.New(zap.WarnLevel)
	_, err = NewSupervisor(t.Context(), zap.New(core), cfg)
	require.NoError(t, err)
	require.Zero(t, logs.FilterMessageSnippet("remote config is unrestricted").Len())
}
