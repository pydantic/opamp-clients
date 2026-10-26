// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// metricsLevel is the merged config's service::telemetry::metrics::level, or "" when unset.
func metricsLevel(t *testing.T, s *Supervisor) string {
	t.Helper()
	conf, err := config.NewConfFromYAML([]byte(s.cfgState.Load().(*configState).mergedConfig))
	require.NoError(t, err)
	level, _ := conf.Get("service::telemetry::metrics::level").(string)
	return level
}

func TestPlaceholderCollectorMetricsOff(t *testing.T) {
	emptyMap := &protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{}},
	}

	t.Run("no remote config and no local files", func(t *testing.T) {
		for _, remote := range []*protobufs.AgentRemoteConfig{nil, emptyMap} {
			s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
			s.config.Agent.ConfigFiles = nil
			_, err := s.composeMergedConfig(remote)
			require.NoError(t, err)
			require.Equal(t, "none", metricsLevel(t, s))
		}
	})

	t.Run("a remote config keeps its own telemetry", func(t *testing.T) {
		s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
		s.config.Agent.ConfigFiles = nil
		_, err := s.composeMergedConfig(&protobufs.AgentRemoteConfig{
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
				"": {Body: []byte("receivers:\n  nop: {}\nexporters:\n  nop: {}\nservice:\n  pipelines:\n    traces:\n      receivers: [nop]\n      exporters: [nop]\n")},
			}},
		})
		require.NoError(t, err)
		require.Empty(t, metricsLevel(t, s))
	})

	t.Run("offered own metrics are kept", func(t *testing.T) {
		s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
		s.config.Agent.ConfigFiles = nil
		s.agentConfigOwnTelemetrySection.Store("service:\n  telemetry:\n    metrics:\n      readers:\n        - periodic:\n            exporter:\n              otlp:\n                protocol: http/protobuf\n                endpoint: https://example.com\n")
		_, err := s.composeMergedConfig(nil)
		require.NoError(t, err)
		require.Empty(t, metricsLevel(t, s))
	})

	t.Run("local config files keep the default", func(t *testing.T) {
		s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
		_, err := s.composeMergedConfig(nil)
		require.NoError(t, err)
		require.Empty(t, metricsLevel(t, s))
	})
}
