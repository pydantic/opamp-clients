// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

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

// macEnroll runs macos/pydantic-opamp-enroll under root, with a launchctl stand-in that logs its
// arguments to tools/launchctl.log.
func macEnroll(t *testing.T, root, tools, stdin string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the enroll script is for macOS")
	}
	launchctl := filepath.Join(tools, "launchctl")
	require.NoError(t, os.WriteFile(launchctl, []byte("#!/bin/sh\necho \"$@\" >>\""+filepath.Join(tools, "launchctl.log")+"\"\n"), 0o755))
	cmd := exec.Command("./macos/pydantic-opamp-enroll", args...)
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+root, "LAUNCHCTL="+launchctl, "OPAMP_SUPERVISOR="+fakeSupervisor(t, tools))
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// One step on macOS: the token in a file only root reads, the config written for the
// LaunchDaemon, and the daemon (re)loaded; the config is one the Supervisor loads.
func TestMacEnroll(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "s3cr3t-token\n", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)

	etc := filepath.Join(root, "opt/pydantic-opamp/etc")
	token, err := os.ReadFile(filepath.Join(etc, "enroll-token"))
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t-token", string(token))
	for _, name := range []string{"", "enroll-token", "supervisor.yaml"} {
		info, err := os.Stat(filepath.Join(etc, name))
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o077, "%q is readable beyond root", name)
	}
	cfg, err := os.ReadFile(filepath.Join(etc, "supervisor.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(cfg), "s3cr3t-token")

	log, err := os.ReadFile(filepath.Join(tools, "launchctl.log"))
	require.NoError(t, err)
	assert.Equal(t, "bootout system/dev.pydantic.opamp-supervisor\n"+
		"bootstrap system /Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist\n", string(log))

	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.NewReplacer(
		"/opt/pydantic-opamp/etc/enroll-token", filepath.Join(etc, "enroll-token"),
		"/opt/pydantic-opamp/bin/otelcol", collector,
		"/opt/pydantic-opamp/var", t.TempDir(),
	).Replace(string(cfg))), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "wss://opamp.example.com/v1/opamp", loaded.Server.Endpoint)
	assert.Equal(t, "Bearer s3cr3t-token", loaded.Server.Headers.Get("Authorization"))
	assert.Equal(t, "keychain", loaded.Storage.StateKey)
	assert.True(t, loaded.Capabilities.AcceptsOpAMPConnectionSettings)
	assert.True(t, loaded.Capabilities.ReportsOwnMetrics)
	assert.True(t, loaded.Capabilities.ReportsOwnLogs)
	assert.True(t, loaded.Agent.AutomaticConfigRollback)
	// Without --restricted, any config the server sends runs.
	assert.False(t, loaded.Agent.RemoteConfigPolicy.Enabled(), "a policy without --restricted")
	require.NoError(t, loaded.Agent.RemoteConfigPolicy.Check("", hostLogsConfig(t, "macos")))
}

// A --restricted policy widened locally, as on Linux.
func TestMacEnrollWidensThePolicyLocally(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp", "--restricted",
		"--exporter-endpoint", "https://logfire-us.pydantic.dev", "--log-path", "/Library/Logs/app", "--no-start")
	require.NoError(t, err, out)
	etc := filepath.Join(root, "opt/pydantic-opamp/etc")
	cfg, err := os.ReadFile(filepath.Join(etc, "supervisor.yaml"))
	require.NoError(t, err)
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.NewReplacer(
		"/opt/pydantic-opamp/etc/enroll-token", filepath.Join(etc, "enroll-token"),
		"/opt/pydantic-opamp/bin/otelcol", collector,
		"/opt/pydantic-opamp/var", t.TempDir(),
	).Replace(string(cfg))), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assertPackagedPolicy(t, loaded.Agent.RemoteConfigPolicy, []string{"https://logfire-us.pydantic.dev"}, []string{"/Library/Logs/app"})
}

// --accept-packages, as on Linux.
func TestMacEnrollAcceptsPackages(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp", "--accept-packages", "--restricted", "--no-start")
	require.NoError(t, err, out)
	etc := filepath.Join(root, "opt/pydantic-opamp/etc")
	cfg, err := os.ReadFile(filepath.Join(etc, "supervisor.yaml"))
	require.NoError(t, err)
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.NewReplacer(
		"/opt/pydantic-opamp/etc/enroll-token", filepath.Join(etc, "enroll-token"),
		"/opt/pydantic-opamp/bin/otelcol", collector,
		"/opt/pydantic-opamp/var", t.TempDir(),
	).Replace(string(cfg))), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	assertPackages(t, loaded, true, "otelcol")
	assert.Empty(t, loaded.Agent.InstanceID, "the Supervisor makes its own instance UID")
	assert.Equal(t, map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}, loaded.Agent.ConnectionEnv)
	assertPackagedPolicy(t, loaded.Agent.RemoteConfigPolicy, []string{"https://opamp.example.com"}, nil)
}

func TestMacEnrollNoStart(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "t", "--endpoint", "http://127.0.0.1:4320/v1/opamp", "--plaintext", "--no-start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "sudo launchctl bootstrap system /Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist")
	_, err = os.Stat(filepath.Join(tools, "launchctl.log"))
	assert.ErrorIs(t, err, os.ErrNotExist, "--no-start loaded the daemon")
}

// What it refuses, before it writes anything.
func TestMacEnrollRefuses(t *testing.T) {
	for _, tt := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"no endpoint", "t", "usage:", nil},
		{"a plaintext endpoint", "t", "isn't encrypted", []string{"--endpoint", "ws://x/v1/opamp"}},
		{"a quote in the endpoint", "t", "can't contain", []string{"--endpoint", `wss://x/"`}},
		{"an empty token", "\n", "token is empty", []string{"--endpoint", "wss://x"}},
		{"a token that isn't one line of token characters", "a b", "characters a bearer token can't", []string{"--endpoint", "wss://x"}},
		{"a relative CA file", "t", "absolute path", []string{"--endpoint", "wss://x", "--ca-file", "enroll_test.go"}},
		{"a plaintext exporter endpoint", "t", "isn't encrypted", []string{"--endpoint", "wss://x", "--restricted", "--exporter-endpoint", "http://x"}},
		{"an exporter endpoint with a path", "t", "with nothing else", []string{"--endpoint", "wss://x", "--restricted", "--exporter-endpoint", "https://x/v1/traces"}},
		{"user info in the endpoint", "t", "with nothing else", []string{"--endpoint", "wss://u@x/v1/opamp"}},
		{"host logs without --restricted", "t", "widen a --restricted policy", []string{"--endpoint", "wss://x", "--host-logs"}},
		{"a relative log path", "t", "absolute path", []string{"--endpoint", "wss://x", "--restricted", "--log-path", "Logs/app"}},
		{"a log path with a glob", "t", "glob characters", []string{"--endpoint", "wss://x", "--restricted", "--log-path", "/Library/Logs/*"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			out, err := macEnroll(t, root, t.TempDir(), tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Contains(t, out, tt.want)
			_, err = os.Stat(filepath.Join(root, "opt"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// With --restricted, --host-logs lets the server read this Mac's unified logs, but not an archive from any path.
func TestMacEnrollHostLogs(t *testing.T) {
	enroll := func(args ...string) config.RemoteConfigPolicy {
		root, tools := t.TempDir(), t.TempDir()
		out, err := macEnroll(t, root, tools, "t", append([]string{"--endpoint", "wss://logfire-us.pydantic.dev/v1/opamp", "--restricted", "--no-start"}, args...)...)
		require.NoError(t, err, out)
		etc := filepath.Join(root, "opt/pydantic-opamp/etc")
		cfg, err := os.ReadFile(filepath.Join(etc, "supervisor.yaml"))
		require.NoError(t, err)
		collector := filepath.Join(t.TempDir(), "otelcol")
		require.NoError(t, os.WriteFile(collector, nil, 0o755))
		path := filepath.Join(t.TempDir(), "supervisor.yaml")
		require.NoError(t, os.WriteFile(path, []byte(strings.NewReplacer(
			"/opt/pydantic-opamp/etc/enroll-token", filepath.Join(etc, "enroll-token"),
			"/opt/pydantic-opamp/bin/otelcol", collector,
			"/opt/pydantic-opamp/var", t.TempDir(),
		).Replace(string(cfg))), 0o600))
		loaded, err := config.Load(path)
		require.NoError(t, err)
		return loaded.Agent.RemoteConfigPolicy
	}
	p := enroll("--host-logs")
	assertHostLogs(t, p, enroll(), hostLogsConfig(t, "macos"), `configures receiver "macos_unified_logging"`)
	require.ErrorContains(t, p.Check("", []byte(`receivers: {macos_unified_logging: {archive_path: /tmp/x.logarchive}}`)), "sets archive_path")
}

// --refresh, which the .pkg's postinstall runs on an upgrade, writes the config again with the
// last enrollment's options, keeps the token and loads the daemon again; one an older package
// wrote without --restricted is rewritten too, the old one kept; an edited one is kept.
func TestMacEnrollRefresh(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	etc := filepath.Join(root, "opt/pydantic-opamp/etc")
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, []byte("a bundle"), 0o644))
	out, err := macEnroll(t, root, tools, "s3cr3t-token\n", "--endpoint", "wss://opamp.example.com/v1/opamp", "--ca-file", ca, "--accept-packages")
	require.NoError(t, err, out)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(etc, name))
		require.NoError(t, err)
		return string(b)
	}
	enrolled := read("supervisor.yaml")
	assert.Equal(t, "--endpoint\nwss://opamp.example.com/v1/opamp\n--ca-file\n"+ca+"\n--accept-packages\n", read("enroll.options"))
	older := strings.ReplaceAll(enrolled, "  reports_own_logs: true\n", "")

	for _, tt := range []struct {
		name  string
		setup func()
	}{
		{"options kept", func() {
			require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml.sha256"), []byte(sha256Hex(older)+"\n"), 0o600))
		}},
		{"an older package's", func() {
			require.NoError(t, os.Remove(filepath.Join(etc, "enroll.options")))
			require.NoError(t, os.Remove(filepath.Join(etc, "supervisor.yaml.sha256")))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml"), []byte(older), 0o600))
			tt.setup()
			require.NoError(t, os.RemoveAll(filepath.Join(tools, "launchctl.log")))
			out, err := macEnroll(t, root, tools, "", "--refresh")
			require.NoError(t, err, out)
			assert.Contains(t, out, "refreshed with wss://opamp.example.com/v1/opamp")
			assert.Equal(t, enrolled, read("supervisor.yaml"))
			assert.Equal(t, "s3cr3t-token", read("enroll-token"))
			log, err := os.ReadFile(filepath.Join(tools, "launchctl.log"))
			require.NoError(t, err)
			assert.Equal(t, "bootout system/dev.pydantic.opamp-supervisor\n"+
				"bootstrap system /Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist\n", string(log))
		})
	}
	assert.Equal(t, older, read("supervisor.yaml.old"))

	edited := enrolled + "# by hand\n"
	require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml"), []byte(edited), 0o600))
	out, err = macEnroll(t, root, tools, "", "--refresh")
	require.NoError(t, err, out)
	assert.Contains(t, out, "edited since it was written and is kept")
	assert.Equal(t, edited, read("supervisor.yaml"))
}
