// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// macEnroll runs macos/pydantic-opamp-enroll under root, with a launchctl stand-in that logs its
// arguments to tools/launchctl.log. Its `print` fails while the daemon isn't loaded (after
// bootout, before bootstrap); loaded, it says the daemon is running and has run once, unless
// tools/inactive exists (waiting to be started again, as one that fails to start) or
// tools/crashing does (running, but started three times).
func macEnroll(t *testing.T, root, tools, stdin string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the enroll script is for macOS")
	}
	launchctl := filepath.Join(tools, "launchctl")
	require.NoError(t, os.WriteFile(launchctl, []byte(`#!/bin/sh
echo "$@" >>"`+filepath.Join(tools, "launchctl.log")+`"
loaded="`+filepath.Join(tools, "loaded")+`"
case $1 in
bootout) rm -f "$loaded" "`+filepath.Join(tools, "kicks")+`" ;;
bootstrap)
	: >"$loaded"
	`+fakeSetAside(tools)+` ;;
kickstart)
	[ -e "$loaded" ] || exit 113
	[ ! -e "`+filepath.Join(tools, "fail-kick")+`" ] || exit 1
	echo $(($(cat "`+filepath.Join(tools, "kicks")+`" 2>/dev/null || echo 0) + 1)) >"`+filepath.Join(tools, "kicks")+`"
	`+fakeSetAside(tools)+` ;;
print)
	# With LAUNCHCTL_STOPPING naming a file, launchd still holds the label (stopping) for that many prints.
	left=$(cat "${LAUNCHCTL_STOPPING:-/dev/null}" 2>/dev/null)
	if [ "${left:-0}" -gt 0 ]; then echo $((left - 1)) >"$LAUNCHCTL_STOPPING"; exit 0; fi
	# 113 is launchctl's "no such service": the daemon isn't loaded.
	[ -e "$loaded" ] || exit 113
	if [ -e "`+filepath.Join(tools, "inactive")+`" ]; then
		printf '\tstate = spawn scheduled\n\truns = 4\n'
	elif [ -e "`+filepath.Join(tools, "crashing")+`" ]; then
		printf '\tstate = running\n\truns = 3\n'
	else
		printf '\tstate = running\n\truns = %d\n' $(($(cat "`+filepath.Join(tools, "kicks")+`" 2>/dev/null || echo 0) + 1))
	fi
	;;
esac
`), 0o755))
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
		"print system/dev.pydantic.opamp-supervisor\n"+
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
	assert.Equal(t, 30*time.Second, loaded.Agent.BootstrapTimeout)
	// One naming a component the Collector doesn't have is refused before it runs.
	assert.True(t, loaded.Agent.ValidateConfig)
	assert.Equal(t, []string{"--feature-gates=telemetry.newPipelineTelemetry"}, loaded.Agent.Arguments)
	// Without --restricted, any config the server sends runs.
	assert.False(t, loaded.Agent.RemoteConfigPolicy.Enabled(), "a policy without --restricted")
	require.NoError(t, loaded.Agent.RemoteConfigPolicy.Check("", hostLogsConfig(t, "macos")))
}

// Started, enrollment waits for the Supervisor to connect, as on Linux: one that can't fails the
// install with what to check, and a daemon that failed to start says so and where its log is.
func TestMacEnrollWaitsForTheConnection(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	out, err := macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)
	assert.Contains(t, out, "The Supervisor is running and connected (dev.pydantic.opamp-supervisor)")
	calls, err := os.ReadFile(filepath.Join(tools, "supervisor.log"))
	require.NoError(t, err)
	assert.Regexp(t, `^status --storage `+regexp.QuoteMeta(root)+`/opt/pydantic-opamp/var --wait 30s --since \d+ --require-accepted\n$`, string(calls))

	require.NoError(t, os.WriteFile(filepath.Join(tools, "status"), []byte("1"), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "doesn't trust the server's certificate")
	assert.Contains(t, out, "can't connect to wss://opamp.example.com/v1/opamp yet; it keeps retrying")
	assert.NotContains(t, out, "failed to start")
	assert.NotContains(t, out, "running and connected")

	// Enrolled again with the same token after the server revoked the host's own credentials: the
	// Supervisor is asked to set the saved state aside as it starts again, once, and the command that puts
	// it back is said.
	state := filepath.Join(root, "opt/pydantic-opamp/var/opamp-state")
	require.NoError(t, os.MkdirAll(filepath.Dir(state), 0o700))
	require.NoError(t, os.WriteFile(state, []byte("sealed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(revokedJSON), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, "the fake status still fails after the second start")
	assert.Equal(t, 1, strings.Count(out, "the server refused this host's saved credentials"))
	aside, err := filepath.Glob(state + ".refused-*")
	require.NoError(t, err)
	require.Len(t, aside, 1)
	kept, err := os.ReadFile(aside[0])
	require.NoError(t, err)
	assert.Equal(t, "sealed", string(kept))
	assert.NotContains(t, out, "purge")
	assert.NotContains(t, out, "failed to start", "its second start is the one asked for")
	log, err := os.ReadFile(filepath.Join(tools, "launchctl.log"))
	require.NoError(t, err)
	assert.Regexp(t, `(?s)bootout.*bootstrap.*kickstart -k system/dev.pydantic.opamp-supervisor\n`, string(log))
	_, command, ok := strings.Cut(out, "put them back with: sudo ")
	require.True(t, ok, out)
	command, _, _ = strings.Cut(command, "\n")
	// The command, run: the old state is back, and what was there is set aside instead.
	run := exec.Command("/bin/sh", "-c", strings.ReplaceAll(command, "launchctl ", filepath.Join(tools, "launchctl")+" "))
	run.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+root)
	b, err := run.CombinedOutput()
	require.NoError(t, err, string(b))
	kept, err = os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, "sealed", string(kept))
	aside, err = filepath.Glob(state + ".replaced-*")
	require.NoError(t, err)
	require.Len(t, aside, 1)
	kept, err = os.ReadFile(aside[0])
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(kept))
	assert.FileExists(t, filepath.Join(tools, "loaded"))
	// Run again: nothing changes.
	run = exec.Command("/bin/sh", "-c", strings.ReplaceAll(command, "launchctl ", filepath.Join(tools, "launchctl")+" "))
	run.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+root)
	b, _ = run.CombinedOutput()
	assert.Contains(t, string(b), "nothing is put back, the copy is not there")
	kept, err = os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, "sealed", string(kept))
	assert.FileExists(t, filepath.Join(tools, "loaded"))
	// What fails later for another reason is nothing to put credentials back for, nor is a name that isn't
	// the Supervisor's.
	require.NoError(t, os.WriteFile(state+".refused-20261122T000000Z-1", []byte("older"), 0o600))
	require.NoError(t, os.WriteFile(state+".refused-..-1", []byte("x"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(tools, "json")))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.NotContains(t, out, "put them back with")
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(refusedJSON), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "in an earlier run, at 2026-11-22 00:00:00 UTC, and the identity kept with them, are set aside in "+state+".refused-20261122T000000Z-1.")
	require.NoError(t, os.WriteFile(state+".refused-20261121T000000Z-1", []byte("oldest"), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "are set aside in "+state+".refused-20261122T000000Z-1.")
	assert.Contains(t, out, "there are more set aside than these, the last: ")
	require.NoError(t, os.Remove(state+".refused-20261121T000000Z-1"))
	out, err = macEnroll(t, root, tools, "", "--refresh")
	require.Error(t, err)
	assert.NotContains(t, out, "put them back with", "an upgrade")
	assert.NotContains(t, out, "there are more set aside")
	// Taken, and its new state not saved: not enrolled, and the way back said.
	for _, how := range []string{`rm -f "$S/opamp-state"`, "true"} {
		require.NoError(t, os.WriteFile(filepath.Join(tools, "heal"), nil, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(tools, "healed"), []byte(how), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(revokedJSON), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(tools, "status"), []byte("1"), 0o600))
		require.NoError(t, os.WriteFile(state, []byte("sealed"), 0o600))
		if how == "true" {
			require.NoError(t, os.WriteFile(filepath.Join(tools, "healed-json"), []byte("{\n  \"save_error\": \"no space left\"\n}\n"), 0o600))
		}
		out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
		require.Error(t, err, how)
		assert.Contains(t, out, "its new credentials could not be saved")
		assert.Contains(t, out, "put them back with: sudo ")
		assert.NotContains(t, out, "Enrolled with")
		for _, name := range []string{"heal", "healed"} {
			require.NoError(t, os.Remove(filepath.Join(tools, name)))
		}
	}
	require.NoError(t, os.Remove(filepath.Join(tools, "healed-json")))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(revokedJSON), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "status"), []byte("1"), 0o600))
	// Of a status before this start, or --refresh: not asked.
	out, err = macEnroll(t, root, tools, "", "--refresh")
	require.Error(t, err)
	assert.NotContains(t, out, "the server refused this host's saved credentials")
	calls, err = os.ReadFile(filepath.Join(tools, "json-calls.log"))
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^status .* --wait 1s --since \d+ --require-accepted --json$`, string(calls))
	require.NoError(t, os.Remove(filepath.Join(tools, "json")))

	// Connected, and the server never answered: the token was refused.
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(refusedJSON), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "took the connection and not the enrollment token: it may be used up, expired or revoked")
	assert.NotContains(t, out, "keeps retrying")
	require.NoError(t, os.Remove(filepath.Join(tools, "json")))

	// It said so as it closed the connection.
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(spentJSON), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "took the connection and not the enrollment token: it may be used up, expired or revoked")
	assert.NotContains(t, out, "not enrolled: the server")

	// The server offered the host credentials and doesn't take it with one.
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(notTakenJSON), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "not enrolled: the server at wss://opamp.example.com/v1/opamp holds this host's identity for another installation")
	assert.Contains(t, out, "sudo /opt/pydantic-opamp/bin/pydantic-opamp-uninstall --purge")
	assert.NotContains(t, out, "keeps retrying")
	assert.NotContains(t, out, "Enrolled with")
	// For another reason, with no advice to remove anything.
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(strings.Replace(notTakenJSON, "identity_taken", "other", 1)), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "not enrolled: the server at wss://opamp.example.com/v1/opamp answered this host and hasn't taken it")
	assert.NotContains(t, out, "--purge")
	assert.NotContains(t, out, "keeps retrying")
	// It offered one for an endpoint that can't be reached.
	require.NoError(t, os.WriteFile(filepath.Join(tools, "json"), []byte(strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"unreachable\": \"other.example.com:8443\"", 1)), 0o600))
	out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.Contains(t, out, "not enrolled: the server at wss://opamp.example.com/v1/opamp tells this host to connect to other.example.com:8443, which can't be reached from here")
	assert.Contains(t, out, "If the server was wrong to, once it is corrected there: ")
	assert.Contains(t, out, "emove this host's data (sudo ")
	assert.Contains(t, out, "pydantic-opamp-uninstall --purge)")
	assert.NotContains(t, out, "hasn't taken it")
	require.NoError(t, os.Remove(filepath.Join(tools, "json")))

	// Not running, or running but started again since the bootstrap (caught between crashes).
	for _, state := range []string{"inactive", "crashing"} {
		require.NoError(t, os.WriteFile(filepath.Join(tools, state), nil, 0o600))
		out, err = macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
		require.Error(t, err, state)
		assert.Contains(t, out, "the Supervisor service (dev.pydantic.opamp-supervisor) failed to start", state)
		assert.Contains(t, out, "sudo tail -n 50 "+root+"/opt/pydantic-opamp/var/log/supervisor.log", state)
		assert.NotContains(t, out, "keeps retrying", state)
		require.NoError(t, os.Remove(filepath.Join(tools, state)))
	}
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

// Enrolling again (and --refresh, which an upgrade runs) on a Mac whose Supervisor runs waits for
// launchd to finish unloading it before loading it again: bootout returns while the Supervisor
// still stops its Collector, and a bootstrap then failed (5: Input/output error), leaving the
// service unloaded.
func TestMacEnrollWaitsForTheUnload(t *testing.T) {
	root, tools := t.TempDir(), t.TempDir()
	stopping := filepath.Join(tools, "stopping")
	require.NoError(t, os.WriteFile(stopping, []byte("2\n"), 0o600))
	t.Setenv("LAUNCHCTL_STOPPING", stopping)
	out, err := macEnroll(t, root, tools, "t", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)
	log, err := os.ReadFile(filepath.Join(tools, "launchctl.log"))
	require.NoError(t, err)
	const print = "print system/dev.pydantic.opamp-supervisor\n"
	assert.Equal(t, "bootout system/dev.pydantic.opamp-supervisor\n"+print+print+print+
		"bootstrap system /Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist\n", string(log))
}

// With --restricted, --host-logs lets the server read this Mac's unified logs, but not an archive from any
// path, and the files under /var/log, with file_storage keeping its place in the Supervisor's own
// directory, and nowhere else.
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
	p := enroll("--host-logs", "--log-path", "/srv/app/logs")
	assertHostLogs(t, p, enroll(), hostLogsConfig(t, "macos"), `configures receiver "macos_unified_logging"`)
	require.ErrorContains(t, p.Check("", []byte(`receivers: {macos_unified_logging: {archive_path: /tmp/x.logarchive}}`)), "sets archive_path")
	// A transform that makes the unified log's JSON bodies readable (the message as the body, the
	// rest as attributes) runs too.
	transform, err := os.ReadFile(filepath.Join("testdata", "host-logs-macos-transform.yaml"))
	require.NoError(t, err)
	require.NoError(t, p.CheckWithConnectionEnv("", transform, map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}))

	// The log files: /var/log is a link to /private/var/log on macOS, and a config may name either.
	assert.Equal(t, []string{"/srv/app/logs", "/var/log", "/private/var/log"}, p.AllowedLogPaths)
	// The test's stand-in for /opt/pydantic-opamp/var, as enroll makes it.
	require.Len(t, p.AllowedStorageDirectories, 1)
	storage := p.AllowedStorageDirectories[0]
	assert.Equal(t, "collector", filepath.Base(storage))
	fileLogs := []byte(strings.NewReplacer("STORAGE", storage).Replace(`
extensions:
  file_storage/host_logs: {directory: STORAGE, create_directory: true}
receivers:
  file_log/system:
    include: [/var/log/*.log, /private/var/log/install.log]
    start_at: end
    storage: file_storage/host_logs
exporters:
  otlp_http/logfire: {endpoint: https://logfire-us.pydantic.dev}
service:
  extensions: [file_storage/host_logs]
  pipelines:
    logs: {receivers: [file_log/system], exporters: [otlp_http/logfire]}
`))
	require.NoError(t, p.Check("", fileLogs))
	require.ErrorContains(t, enroll().Check("", fileLogs), `configures extension "file_storage/host_logs"`)
	require.ErrorContains(t, p.Check("", []byte(`extensions: {file_storage: {directory: /etc/cron.d}}`)), "allowed_storage_directories doesn't allow")
	require.ErrorContains(t, p.Check("", []byte(`receivers: {filelog: {include: [/etc/master.passwd]}}`)), "allowed_log_paths doesn't allow")
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
			assert.Contains(t, out, "Refreshed with wss://opamp.example.com/v1/opamp")
			assert.Equal(t, enrolled, read("supervisor.yaml"))
			assert.Equal(t, "s3cr3t-token", read("enroll-token"))
			log, err := os.ReadFile(filepath.Join(tools, "launchctl.log"))
			require.NoError(t, err)
			assert.Equal(t, "bootout system/dev.pydantic.opamp-supervisor\n"+
				"print system/dev.pydantic.opamp-supervisor\n"+
				"bootstrap system /Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist\n", string(log))
		})
	}
	assert.Equal(t, older, read("supervisor.yaml.old"))

	// An older package's config with a setting the new one wouldn't keep stays as it is.
	handEdited := strings.Replace(older, "  headers:\n", "  headers:\n    X-Team: infra\n", 1)
	require.NotEqual(t, older, handEdited)
	require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml"), []byte(handEdited), 0o600))
	require.NoError(t, os.Remove(filepath.Join(etc, "enroll.options")))
	require.NoError(t, os.Remove(filepath.Join(etc, "supervisor.yaml.sha256")))
	out, err = macEnroll(t, root, tools, "", "--refresh")
	require.NoError(t, err, out)
	assert.Contains(t, out, "with --restricted, or edited, and is kept")
	assert.Equal(t, handEdited, read("supervisor.yaml"))
	_, err = os.Stat(filepath.Join(etc, "enroll.options"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml"), []byte(enrolled), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml.sha256"), []byte(sha256Hex(enrolled)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(etc, "enroll.options"),
		[]byte("--endpoint\nwss://opamp.example.com/v1/opamp\n--ca-file\n"+ca+"\n--accept-packages\n"), 0o600))

	edited := enrolled + "# by hand\n"
	require.NoError(t, os.WriteFile(filepath.Join(etc, "supervisor.yaml"), []byte(edited), 0o600))
	out, err = macEnroll(t, root, tools, "", "--refresh")
	require.NoError(t, err, out)
	assert.Contains(t, out, "edited since it was written and is kept")
	assert.Equal(t, edited, read("supervisor.yaml"))
}
