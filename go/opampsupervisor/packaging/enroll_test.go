// Package packaging holds the Linux package's files (pydantic-opamp-enroll, the systemd unit, the
// bundled collector's manifest) and the tests that run them.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// fakeSystemd puts systemd-creds and systemctl stand-ins in dir: encrypt writes "SEALED:" plus
// its input to the output file; systemctl logs its arguments to dir/systemctl.log.
func fakeSystemd(t *testing.T, dir string) (creds, systemctl string) {
	t.Helper()
	creds = filepath.Join(dir, "systemd-creds")
	require.NoError(t, os.WriteFile(creds, []byte(`#!/bin/sh
[ "$1" = encrypt ] || exit 3
{ printf 'SEALED:%s:' "$2"; cat "$3"; } >"$4"
`), 0o755))
	systemctl = filepath.Join(dir, "systemctl")
	require.NoError(t, os.WriteFile(systemctl, []byte(`#!/bin/sh
echo "$@" >>"`+filepath.Join(dir, "systemctl.log")+`"
`), 0o755))
	return creds, systemctl
}

type enrollment struct {
	root, tools string
	creds       string
	systemctl   string
}

func newEnrollment(t *testing.T) enrollment {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the enroll script is for Linux")
	}
	e := enrollment{root: t.TempDir(), tools: t.TempDir()}
	e.creds, e.systemctl = fakeSystemd(t, e.tools)
	return e
}

func (e enrollment) run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("./pydantic-opamp-enroll", args...)
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+e.root, "SYSTEMD_CREDS="+e.creds, "SYSTEMCTL="+e.systemctl)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e enrollment) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, "etc/pydantic-opamp", name))
	require.NoError(t, err)
	return string(b)
}

// One step: the token is sealed (never written in the clear), the state key made, the config
// written for the unit, and the service started; the config is one the Supervisor loads.
func TestEnroll(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "s3cr3t-token\n", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)

	assert.Equal(t, "SEALED:--name=opamp-enroll-token:s3cr3t-token", e.read(t, "enroll-token.cred"))
	key := e.read(t, "state-key.cred")
	assert.True(t, strings.HasPrefix(key, "SEALED:--name=opamp-state-key:"))
	assert.Len(t, strings.TrimPrefix(key, "SEALED:--name=opamp-state-key:"), 32)

	cfg := e.read(t, "supervisor.yaml")
	assert.NotContains(t, cfg, "s3cr3t-token")
	assert.Contains(t, cfg, "${file:/run/credentials/pydantic-opamp-supervisor.service/opamp-enroll-token}")
	for _, name := range []string{"enroll-token.cred", "state-key.cred", "supervisor.yaml"} {
		info, err := os.Stat(filepath.Join(e.root, "etc/pydantic-opamp", name))
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o027, "%s is readable beyond root and the service", name)
	}

	log, err := os.ReadFile(filepath.Join(e.tools, "systemctl.log"))
	require.NoError(t, err)
	// The unmanaged collector, which conflicts with the Supervisor, is turned off for good first.
	assert.Equal(t, "disable --now pydantic-otelcol.service\nenable pydantic-opamp-supervisor.service\n"+
		"restart pydantic-opamp-supervisor.service\n", string(log))

	// What systemd gives the service: the decrypted token, and the collector at its path.
	token := filepath.Join(t.TempDir(), "opamp-enroll-token")
	require.NoError(t, os.WriteFile(token, []byte("s3cr3t-token"), 0o600))
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	cfg = strings.NewReplacer(
		"/run/credentials/pydantic-opamp-supervisor.service/opamp-enroll-token", token,
		"/usr/lib/pydantic-opamp/otelcol", collector,
		"/var/lib/pydantic-opamp", t.TempDir(),
	).Replace(cfg)
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "wss://opamp.example.com/v1/opamp", loaded.Server.Endpoint)
	assert.Equal(t, "Bearer s3cr3t-token", loaded.Server.Headers.Get("Authorization"))
	assert.Equal(t, "systemd-creds", loaded.Storage.StateKey)
	assert.True(t, loaded.Capabilities.AcceptsOpAMPConnectionSettings)
}

// Enrolling again replaces the token but keeps the state key: a new key couldn't open the
// state the old one sealed.
func TestEnrollAgainKeepsTheStateKey(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "first", "--endpoint", "https://a.example.com/v1/opamp", "--no-start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "systemctl enable --now")
	key := e.read(t, "state-key.cred")

	out, err = e.run(t, "second", "--endpoint", "https://b.example.com/v1/opamp", "--no-start")
	require.NoError(t, err, out)
	assert.Equal(t, key, e.read(t, "state-key.cred"))
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:second", e.read(t, "enroll-token.cred"))
	assert.Contains(t, e.read(t, "supervisor.yaml"), `endpoint: "https://b.example.com/v1/opamp"`)
	_, err = os.Stat(filepath.Join(e.tools, "systemctl.log"))
	assert.ErrorIs(t, err, os.ErrNotExist, "--no-start started the service")
}

func TestEnrollWithACAFile(t *testing.T) {
	e := newEnrollment(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, []byte("not checked here"), 0o644))
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("from-a-file"), 0o600))
	out, err := e.run(t, "", "--endpoint", "wss://opamp.example.com/v1/opamp", "--ca-file", ca, "--token-file", tokenFile)
	require.NoError(t, err, out)
	assert.Contains(t, e.read(t, "supervisor.yaml"), "ca_file: \""+ca+"\"")
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:from-a-file", e.read(t, "enroll-token.cred"))
}

// What it refuses, before it writes anything.
func TestEnrollRefuses(t *testing.T) {
	tests := []struct {
		name, stdin, want string
		args              []string
	}{
		{"no endpoint", "t", "usage:", nil},
		{"an unknown flag", "t", "usage:", []string{"--endpoint", "wss://x", "--bogus"}},
		{"a plaintext endpoint", "t", "isn't encrypted", []string{"--endpoint", "ws://x/v1/opamp"}},
		{"not a URL", "t", "must be a wss://", []string{"--endpoint", "opamp.example.com"}},
		{"a quote in the endpoint", "t", "can't contain", []string{"--endpoint", `wss://x/"`}},
		{"an empty token", "\n", "token is empty", []string{"--endpoint", "wss://x"}},
		{"a token that isn't one line of token characters", "a b", "characters a bearer token can't", []string{"--endpoint", "wss://x"}},
		{"a relative CA file", "t", "absolute path", []string{"--endpoint", "wss://x", "--ca-file", "enroll_test.go"}},
		{"a missing CA file", "t", "can't read", []string{"--endpoint", "wss://x", "--ca-file", "/nonexistent/ca.pem"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnrollment(t)
			out, err := e.run(t, tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Contains(t, out, tt.want)
			_, err = os.Stat(filepath.Join(e.root, "etc/pydantic-opamp"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// --plaintext lets a ws:// or http:// endpoint through, for a server on the same host.
func TestEnrollPlaintext(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "t", "--endpoint", "http://127.0.0.1:4320/v1/opamp", "--plaintext", "--no-start")
	require.NoError(t, err, out)
	assert.Contains(t, e.read(t, "supervisor.yaml"), `endpoint: "http://127.0.0.1:4320/v1/opamp"`)
}
