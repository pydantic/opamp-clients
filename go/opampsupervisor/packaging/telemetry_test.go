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

func TestEnrollTelemetryCredentials(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "runtime-token\nwrite-token\n", "--endpoint", "wss://opamp.example.com", "--telemetry-token-stdin", "--no-start")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "runtime-token")
	assert.NotContains(t, out, "write-token")
	assert.Equal(t, "SEALED:--name=opamp-telemetry-token:write-token", e.read(t, "telemetry-token.cred"))
	cfg := e.read(t, "supervisor.yaml")
	assert.NotContains(t, cfg, "write-token")
	assert.Contains(t, cfg, `LOGFIRE_TOKEN: "${file:/run/credentials/pydantic-opamp-supervisor.service/opamp-telemetry-token}"`)
	dropin, err := os.ReadFile(filepath.Join(e.root, "etc/systemd/system/pydantic-opamp-supervisor.service.d/telemetry.conf"))
	require.NoError(t, err)
	assert.Equal(t, "[Service]\nLoadCredentialEncrypted=opamp-telemetry-token:/etc/pydantic-opamp/telemetry-token.cred\n", string(dropin))
	out, err = e.run(t, "replacement-runtime", "--endpoint", "wss://opamp.example.com", "--no-start")
	require.NoError(t, err, out)
	assert.Contains(t, e.read(t, "supervisor.yaml"), "opamp-telemetry-token}")
	assert.Equal(t, "SEALED:--name=opamp-telemetry-token:write-token", e.read(t, "telemetry-token.cred"))
}

func TestMacEnrollTelemetryCredentials(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "runtime-token\nwrite-token\n", "--endpoint", "wss://opamp.example.com", "--telemetry-token-stdin", "--no-start")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "runtime-token")
	assert.NotContains(t, out, "write-token")
	etc := filepath.Join(root, "opt/pydantic-opamp/etc")
	telemetry, err := os.ReadFile(filepath.Join(etc, "telemetry-token"))
	require.NoError(t, err)
	assert.Equal(t, "write-token", string(telemetry))
	info, err := os.Stat(filepath.Join(etc, "telemetry-token"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	cfg, err := os.ReadFile(filepath.Join(etc, "supervisor.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(cfg), "write-token")
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.NewReplacer(
		"/opt/pydantic-opamp/etc/", filepath.ToSlash(etc)+"/",
		"/opt/pydantic-opamp/bin/otelcol", filepath.ToSlash(collector),
		"/opt/pydantic-opamp/var", filepath.ToSlash(t.TempDir()),
	).Replace(string(cfg))), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "Bearer runtime-token", loaded.Server.Headers.Get("Authorization"))
	assert.Equal(t, "write-token", loaded.Agent.Env["LOGFIRE_TOKEN"])
}

func TestEnrollRefusesMalformedTelemetryCredentials(t *testing.T) {
	for _, stdin := range []string{"runtime-token\n", "runtime-token\n\n", "runtime-token\ninvalid token\n", "runtime-token\nwrite-token\nextra\n"} {
		t.Run(stdin, func(t *testing.T) {
			e := newEnrollment(t)
			out, err := e.run(t, stdin, "--endpoint", "wss://opamp.example.com", "--telemetry-token-stdin")
			require.Error(t, err)
			assert.NotContains(t, out, "runtime-token")
			_, err = os.Stat(filepath.Join(e.root, "etc"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			root := t.TempDir()
			out, err = macEnroll(t, root, t.TempDir(), stdin, "--endpoint", "wss://opamp.example.com", "--telemetry-token-stdin")
			require.Error(t, err)
			assert.NotContains(t, out, "runtime-token")
			_, err = os.Stat(filepath.Join(root, "opt"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
