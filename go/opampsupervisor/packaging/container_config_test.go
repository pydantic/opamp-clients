// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// loadContainerConfig loads one of the image's configs, with the collector at a real path.
func loadContainerConfig(t *testing.T, name string) config.Supervisor {
	t.Helper()
	t.Setenv("OPAMP_ENDPOINT", "wss://logfire-us.pydantic.dev/v1/opamp")
	t.Setenv("OPAMP_TOKEN", "t")
	body, err := os.ReadFile(filepath.Join("container", name))
	require.NoError(t, err)
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	cfg := strings.NewReplacer(
		"/usr/lib/pydantic-opamp/otelcol", collector,
		"/var/lib/pydantic-opamp", t.TempDir(),
	).Replace(string(body))
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	return loaded
}

// The image runs any config the server sends; supervisor-restricted.yaml is the enroll scripts'
// --restricted policy, sending to Logfire's regions unless OPAMP_EXPORTER_ENDPOINTS says otherwise.
func TestContainerConfigs(t *testing.T) {
	open := loadContainerConfig(t, "supervisor.yaml")
	assert.True(t, open.Capabilities.ReportsOwnMetrics)
	assert.True(t, open.Capabilities.ReportsOwnLogs)
	assert.False(t, open.Agent.RemoteConfigPolicy.Enabled(), "a policy in the default image config")
	// As the host packages: a config the Collector can't load is refused before it runs, and one
	// it fails on is replaced by the last that ran healthy, the log's tail in the FAILED status.
	for _, cfg := range []config.Supervisor{open, loadContainerConfig(t, "supervisor-restricted.yaml")} {
		assert.True(t, cfg.Agent.ValidateConfig)
		assert.True(t, cfg.Agent.AutomaticConfigRollback)
		assert.Equal(t, 4, cfg.Agent.CollectorCrashLogSnippetKiB)
	}
	require.NoError(t, open.Agent.RemoteConfigPolicy.Check("", []byte(`exporters: {datadog: {api: {key: "${env:DD_API_KEY}"}}}`)))

	restricted := loadContainerConfig(t, "supervisor-restricted.yaml")
	assert.True(t, restricted.Capabilities.ReportsOwnMetrics)
	assert.True(t, restricted.Capabilities.ReportsOwnLogs)
	p := restricted.Agent.RemoteConfigPolicy
	// The own telemetry Logfire offers stays inside the restricted policy.
	require.NoError(t, p.Check(string(config.SpecialConfigFileOwnTelemetry), []byte(`service: {telemetry: {logs: {processors: [{batch: {exporter: {otlp: {protocol: http/protobuf, endpoint: "https://logfire-eu.pydantic.dev/v1/logs"}}}}]}}}`)))
	assert.Equal(t, packagedComponents, p.AllowedComponents)
	assert.Equal(t, []string{"https://logfire-us.pydantic.dev", "https://logfire-eu.pydantic.dev"}, p.AllowedExporterEndpoints)
	assert.Empty(t, p.AllowedLogPaths)
}
