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

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// winEnroll runs windows/pydantic-opamp-enroll.ps1 writing under root, with stdin as the token.
func winEnroll(t *testing.T, root, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("powershell", append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", filepath.Join("windows", "pydantic-opamp-enroll.ps1")}, args...)...)
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+root)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The token in a file only SYSTEM and Administrators read, and a config the Supervisor loads.
func TestWinEnroll(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pydantic-opamp")
	out, err := winEnroll(t, root, "s3cr3t-token\r\n", "-Endpoint", "wss://opamp.example.com/v1/opamp", "-NoStart")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Start-Service pydantic-opamp-supervisor")

	token, err := os.ReadFile(filepath.Join(root, "enroll-token"))
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t-token", string(token))
	acl, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-Acl -LiteralPath '"+filepath.Join(root, "enroll-token")+"').Access | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value }").CombinedOutput()
	require.NoError(t, err, string(acl))
	assert.ElementsMatch(t, []string{"S-1-5-18", "S-1-5-32-544"}, strings.Fields(string(acl)), "the token is readable beyond SYSTEM and Administrators")

	cfg, err := os.ReadFile(filepath.Join(root, "supervisor.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(cfg), "s3cr3t-token")
	collector := filepath.Join(t.TempDir(), "otelcol.exe")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	programs := filepath.ToSlash(filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "otelcol.exe"))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(cfg), programs, filepath.ToSlash(collector), 1)), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "wss://opamp.example.com/v1/opamp", loaded.Server.Endpoint)
	assert.Equal(t, "Bearer s3cr3t-token", loaded.Server.Headers.Get("Authorization"))
	assert.Equal(t, "dpapi", loaded.Storage.StateKey)
	assert.Equal(t, filepath.ToSlash(filepath.Join(root, "var")), loaded.Storage.Directory)
}

// What it refuses, before it writes anything.
func TestWinEnrollRefuses(t *testing.T) {
	for _, tt := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"a plaintext endpoint", "t", "isn't encrypted", []string{"-Endpoint", "ws://x/v1/opamp"}},
		{"not a URL", "t", "must be a wss://", []string{"-Endpoint", "opamp.example.com"}},
		{"a quote in the endpoint", "t", "can't contain", []string{"-Endpoint", `wss://x/'`}},
		{"an empty token", "\r\n", "token is empty", []string{"-Endpoint", "wss://x"}},
		{"a token that isn't one line of token characters", "a b", "characters a bearer token can't", []string{"-Endpoint", "wss://x"}},
		{"a relative CA file", "t", "absolute path", []string{"-Endpoint", "wss://x", "-CaFile", "enroll_test.go"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "pydantic-opamp")
			out, err := winEnroll(t, root, tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Contains(t, out, tt.want)
			_, err = os.Stat(root)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
