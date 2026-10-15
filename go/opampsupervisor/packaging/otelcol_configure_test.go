// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unmanagedConfig is a collector config of the kind the logfire CLI writes: the token read from
// the credential systemd decrypts for pydantic-otelcol.service.
const unmanagedConfig = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:4317
exporters:
  otlphttp:
    endpoint: https://logfire-us.pydantic.dev
    headers:
      Authorization: ${file:/run/credentials/pydantic-otelcol.service/otelcol-token}
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlphttp]
`

func (e enrollment) configure(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("./pydantic-otelcol-configure", args...)
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+e.root, "SYSTEMD_CREDS="+e.creds, "SYSTEMCTL="+e.systemctl)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "otelcol.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// One step: the token sealed (never in the clear), the config installed as given, and the
// unmanaged collector started in place of the Supervisor, which is turned off for good.
func TestOtelcolConfigure(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.configure(t, "lf-token_123\n", "--config", writeConfig(t, unmanagedConfig))
	require.NoError(t, err, out)

	assert.Equal(t, "SEALED:--name=otelcol-token:lf-token_123", e.read(t, "otelcol-token.cred"))
	assert.Equal(t, unmanagedConfig, e.read(t, "otelcol.yaml"))
	for _, name := range []string{"otelcol-token.cred", "otelcol.yaml"} {
		info, err := os.Stat(filepath.Join(e.root, "etc/pydantic-opamp", name))
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o027, "%s is readable beyond root and the service", name)
	}
	log, err := os.ReadFile(filepath.Join(e.tools, "systemctl.log"))
	require.NoError(t, err)
	assert.Equal(t, "disable --now pydantic-opamp-supervisor.service\nenable pydantic-otelcol.service\n"+
		"restart pydantic-otelcol.service\n", string(log))
}

// The unit decrypts the credential the script seals, under the name the config reads it by,
// and stands in for the Supervisor.
func TestOtelcolUnitReadsWhatConfigureWrites(t *testing.T) {
	unit, err := os.ReadFile("pydantic-otelcol.service")
	require.NoError(t, err)
	assert.Contains(t, string(unit), "\nLoadCredentialEncrypted=otelcol-token:/etc/pydantic-opamp/otelcol-token.cred\n")
	assert.Contains(t, string(unit), "\nExecStart=/usr/lib/pydantic-opamp/otelcol --config /etc/pydantic-opamp/otelcol.yaml\n")
	assert.Contains(t, string(unit), "\nConflicts=pydantic-opamp-supervisor.service\n")
	supervisor, err := os.ReadFile("pydantic-opamp-supervisor.service")
	require.NoError(t, err)
	assert.Contains(t, string(supervisor), "\nConflicts=pydantic-otelcol.service\n")
}

func TestOtelcolConfigureNoStart(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.configure(t, "lf-token", "--config", writeConfig(t, unmanagedConfig), "--no-start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "systemctl enable --now pydantic-otelcol.service")
	_, err = os.Stat(filepath.Join(e.tools, "systemctl.log"))
	assert.ErrorIs(t, err, os.ErrNotExist, "it touched the services")
}

// Nothing is written, and nothing started, without a token and a config.
func TestOtelcolConfigureRefuses(t *testing.T) {
	config := writeConfig(t, unmanagedConfig)
	for name, c := range map[string]struct {
		stdin string
		args  []string
		says  string
	}{
		"no config":      {"lf-token", nil, "usage:"},
		"missing config": {"lf-token", []string{"--config", "/nonexistent/otelcol.yaml"}, "can't read"},
		"empty config":   {"lf-token", []string{"--config", writeConfig(t, "")}, "empty"},
		"empty token":    {"\n", []string{"--config", config}, "the token is empty"},
		"bad token":      {"two words", []string{"--config", config}, "characters"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnrollment(t)
			out, err := e.configure(t, c.stdin, c.args...)
			assert.Error(t, err, out)
			assert.Contains(t, out, c.says)
			_, err = os.Stat(filepath.Join(e.root, "etc/pydantic-opamp/otelcol.yaml"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(filepath.Join(e.tools, "systemctl.log"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
