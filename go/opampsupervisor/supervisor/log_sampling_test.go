// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// Upstream's tests compare whole merged configs, on macOS too: they run without the default.
// TestTheCollectorLogIsSampledByDefaultOnMacOS names the system it tests.
func init() { logSamplingGOOS = "" }

func logSampling(t *testing.T, s *Supervisor) map[string]any {
	t.Helper()
	conf, err := config.NewConfFromYAML([]byte(s.cfgState.Load().(*configState).mergedConfig))
	require.NoError(t, err)
	m, _ := conf.Get("service::telemetry::logs::sampling").(map[string]any)
	return m
}

// On macOS the collector's own log is sampled by default (upstream's unified log receiver logs a
// line at each poll, about one a second); a remote config's own sampling wins; other systems get
// no default.
func TestTheCollectorLogIsSampledByDefaultOnMacOS(t *testing.T) {
	withRemoteSampling := &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: []byte("receivers:\n  nop: {}\nexporters:\n  nop: {}\nservice:\n  telemetry:\n    logs:\n      sampling:\n        enabled: false\n  pipelines:\n    logs:\n      receivers: [nop]\n      exporters: [nop]\n")},
	}}}
	plain := &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: []byte("receivers:\n  nop: {}\nexporters:\n  nop: {}\nservice:\n  pipelines:\n    logs:\n      receivers: [nop]\n      exporters: [nop]\n")},
	}}}
	defer func(goos string) { logSamplingGOOS = goos }(logSamplingGOOS)

	logSamplingGOOS = "darwin"
	s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
	s.config.Agent.ConfigFiles = nil
	_, err := s.composeMergedConfig(plain)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"enabled": true, "tick": "60s", "initial": 5, "thereafter": 500}, logSampling(t, s))

	_, err = s.composeMergedConfig(withRemoteSampling)
	require.NoError(t, err)
	assert.Equal(t, false, logSampling(t, s)["enabled"], "the remote config's own")

	for _, goos := range []string{"linux", "windows"} {
		logSamplingGOOS = goos
		s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
		s.config.Agent.ConfigFiles = nil
		_, err := s.composeMergedConfig(plain)
		require.NoError(t, err)
		assert.Nil(t, logSampling(t, s), goos)
	}
}
