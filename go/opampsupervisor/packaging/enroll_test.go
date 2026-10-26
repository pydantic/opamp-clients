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
	assertPackagedPolicy(t, loaded.Agent.RemoteConfigPolicy, []string{"https://opamp.example.com"}, nil)
}

// packagedComponents are the component types every enroll script lets a remote config use.
var packagedComponents = map[string][]string{
	"receivers":  {"otlp", "host_metrics", "hostmetrics", "file_log", "filelog", "nop"},
	"processors": {"batch", "memory_limiter", "resource", "resource_detection", "resourcedetection"},
	"exporters":  {"otlp_grpc", "otlp", "otlp_http", "otlphttp", "debug", "nop"},
	"extensions": {"health_check"},
}

// assertPackagedPolicy checks the policy an enroll script wrote: the packaged components, no
// providers, and the destinations and log paths given.
func assertPackagedPolicy(t *testing.T, p config.RemoteConfigPolicy, endpoints, logPaths []string) {
	t.Helper()
	require.True(t, p.Enabled(), "no remote config policy")
	assert.Equal(t, packagedComponents, p.AllowedComponents)
	assert.Empty(t, p.AllowedProviders)
	assert.Equal(t, endpoints, p.AllowedExporterEndpoints)
	if len(logPaths) == 0 {
		assert.Empty(t, p.AllowedLogPaths)
	} else {
		assert.Equal(t, logPaths, p.AllowedLogPaths)
	}
}

// loadEnrolled loads the config pydantic-opamp-enroll wrote, with the paths systemd provides
// made real.
func (e enrollment) loadEnrolled(t *testing.T) config.Supervisor {
	t.Helper()
	token := filepath.Join(t.TempDir(), "opamp-enroll-token")
	require.NoError(t, os.WriteFile(token, []byte("t"), 0o600))
	collector := filepath.Join(t.TempDir(), "otelcol")
	require.NoError(t, os.WriteFile(collector, nil, 0o755))
	cfg := strings.NewReplacer(
		"/run/credentials/pydantic-opamp-supervisor.service/opamp-enroll-token", token,
		"/usr/lib/pydantic-opamp/otelcol", collector,
		"/var/lib/pydantic-opamp", t.TempDir(),
	).Replace(e.read(t, "supervisor.yaml"))
	path := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	loaded, err := config.Load(path)
	require.NoError(t, err)
	return loaded
}

// The policy widened locally: the destinations and log paths given replace the defaults.
func TestEnrollWidensThePolicyLocally(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "t", "--endpoint", "wss://opamp.example.com:8443/v1/opamp",
		"--exporter-endpoint", "https://logfire-us.pydantic.dev", "--exporter-endpoint", "https://logfire-eu.pydantic.dev:443/",
		"--log-path", "/var/log/app", "--log-path", "/var/log/syslog", "--no-start")
	require.NoError(t, err, out)
	assertPackagedPolicy(t, e.loadEnrolled(t).Agent.RemoteConfigPolicy,
		[]string{"https://logfire-us.pydantic.dev", "https://logfire-eu.pydantic.dev:443/"}, []string{"/var/log/app", "/var/log/syslog"})

	// By default, the OpAMP server's host and port, over https or (with --plaintext) http.
	e = newEnrollment(t)
	out, err = e.run(t, "t", "--endpoint", "wss://opamp.example.com:8443/v1/opamp?x=1", "--no-start")
	require.NoError(t, err, out)
	assertPackagedPolicy(t, e.loadEnrolled(t).Agent.RemoteConfigPolicy, []string{"https://opamp.example.com:8443"}, nil)
	e = newEnrollment(t)
	out, err = e.run(t, "t", "--endpoint", "ws://127.0.0.1:4320/v1/opamp", "--plaintext", "--no-start")
	require.NoError(t, err, out)
	assertPackagedPolicy(t, e.loadEnrolled(t).Agent.RemoteConfigPolicy, []string{"http://127.0.0.1:4320"}, nil)
}

// What the packaged policy lets a server do: a Logfire config, but not what turns the
// collector against the host (SEC-4's file and environment reads, and sends elsewhere).
func TestPackagedPolicy(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "t", "--endpoint", "wss://logfire-us.pydantic.dev/v1/opamp", "--log-path", "/var/log/app", "--no-start")
	require.NoError(t, err, out)
	p := e.loadEnrolled(t).Agent.RemoteConfigPolicy
	require.NoError(t, p.Check("", []byte(`receivers:
  otlp:
    protocols: {grpc: {endpoint: 127.0.0.1:4317}, http: {endpoint: 127.0.0.1:4318}}
  hostmetrics:
    collection_interval: 30s
    scrapers: {cpu: {}, memory: {}, filesystem: {}}
  filelog:
    include: [/var/log/app/*.log]
processors:
  memory_limiter: {check_interval: 1s, limit_percentage: 80}
  resourcedetection: {detectors: [system]}
  batch: {}
exporters:
  otlphttp:
    endpoint: https://logfire-us.pydantic.dev
    headers: {Authorization: "a-write-token"}
service:
  pipelines:
    traces: {receivers: [otlp], processors: [memory_limiter, batch], exporters: [otlphttp]}
    metrics: {receivers: [otlp, hostmetrics], processors: [memory_limiter, resourcedetection, batch], exporters: [otlphttp]}
    logs: {receivers: [otlp, filelog], processors: [memory_limiter, batch], exporters: [otlphttp]}
`)))
	for name, tc := range map[string]struct{ body, wantErr string }{
		"a file read":            {`receivers: {filelog: {include: [/etc/shadow]}}`, `reads "/etc/shadow"`},
		"a file read, new name":  {`receivers: {file_log: {include: [/etc/shadow]}}`, `reads "/etc/shadow"`},
		"elsewhere, new name":    {`exporters: {otlp_http: {endpoint: "https://collector.example.com"}}`, "allowed_exporter_endpoints doesn't allow"},
		"a credential read":      {`receivers: {filelog: {include: [/run/credentials/pydantic-opamp-supervisor.service/opamp-enroll-token]}}`, "allowed_log_paths doesn't allow"},
		"a file provider":        {`exporters: {otlphttp: {headers: {x: "${file:/etc/shadow}"}}}`, "references the file provider"},
		"an env provider":        {`exporters: {otlphttp: {headers: {x: "${env:AWS_SECRET_ACCESS_KEY}"}}}`, "references the env provider"},
		"another destination":    {`exporters: {otlphttp: {endpoint: "https://collector.example.com"}}`, "allowed_exporter_endpoints doesn't allow"},
		"a component not listed": {`exporters: {file: {path: /etc/cron.d/x}}`, `configures exporter "file"`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, p.Check("", []byte(tc.body)), tc.wantErr)
		})
	}
}

// The Supervisor makes and saves its own instance UID, so enrollment names none. The server's
// exporter credential for logfire/otlp reaches the Collector as LOGFIRE_TOKEN.
func TestEnrollBindsTheExporterCredential(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "bootstrap-token\n", "--endpoint", "wss://opamp.example.com/v1/opamp", "--accept-packages", "--no-start")
	require.NoError(t, err, out)
	cfg := e.read(t, "supervisor.yaml")
	assert.NotContains(t, cfg, "instance_id")
	assert.Contains(t, cfg, "accepts_other_connection_settings: true")
	assert.Contains(t, cfg, "accepts_restart_command: true")
	assert.NotContains(t, cfg, "bootstrap-token")
	assert.NotContains(t, out, "bootstrap-token")
	loaded := e.loadEnrolled(t)
	assertPackages(t, loaded, true, "otelcol")
	assert.Empty(t, loaded.Agent.InstanceID)
	assert.Equal(t, map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}, loaded.Agent.ConnectionEnv)
	assert.True(t, loaded.Capabilities.AcceptsOtherConnectionSettings)
	assert.True(t, loaded.Capabilities.AcceptsRestartCommand)
	out, err = newEnrollment(t).run(t, "bootstrap-token", "--endpoint", "wss://opamp.example.com/v1/opamp", "--instance-id", "f0cff0a3-08e5-4c49-96ce-1366b14525ed")
	require.Error(t, err, "enrollment no longer takes an instance UID")
	assert.NotContains(t, out, "bootstrap-token")
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
		{"a plaintext exporter endpoint", "t", "isn't encrypted", []string{"--endpoint", "wss://x", "--exporter-endpoint", "http://x"}},
		{"an exporter endpoint not a URL", "t", "must be an https:// or http:// URL", []string{"--endpoint", "wss://x", "--exporter-endpoint", "x:443"}},
		{"an exporter endpoint with a path", "t", "scheme://host[:port], with nothing else", []string{"--endpoint", "wss://x", "--exporter-endpoint", "https://x/v1/traces"}},
		{"an exporter endpoint with user info", "t", "with nothing else", []string{"--endpoint", "wss://x", "--exporter-endpoint", "https://x@y"}},
		{"user info in the endpoint", "t", "with nothing else", []string{"--endpoint", "wss://u@x/v1/opamp"}},
		{"a relative log path", "t", "absolute path", []string{"--endpoint", "wss://x", "--log-path", "var/log"}},
		{"a log path with a glob", "t", "glob characters", []string{"--endpoint", "wss://x", "--log-path", "/var/log/*"}},
		{"a log path climbing out", "t", "..", []string{"--endpoint", "wss://x", "--log-path", "/var/log/../../etc"}},
		{"a log path with a quote", "t", "can't hold quotes", []string{"--endpoint", "wss://x", "--log-path", `/var/log/"`}},
		{"a log path with a newline", "t", "can't hold a newline", []string{"--endpoint", "wss://x", "--log-path", "/var/log/a\n/b"}},
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

// assertPackages checks the enrolled Supervisor accepts packages from this repository's release
// workflow alone, or no packages at all.
func assertPackages(t *testing.T, cfg config.Supervisor, accepted bool, binary string) {
	t.Helper()
	assert.Equal(t, accepted, cfg.Capabilities.AcceptsPackages)
	if !accepted {
		return
	}
	assert.Equal(t, binary, cfg.Agent.Package.AgentBinary)
	assert.Equal(t, config.VerifierTypeCosign, cfg.Agent.Package.Verifier.Type)
	assert.Equal(t, config.PydanticCosignVerifier(), cfg.Agent.Package.Verifier.Cosign)
}

// --accept-packages turns on the collector updates the server offers, checked against this
// repository's release workflow's signature; without it, there are none.
func TestEnrollAcceptsPackages(t *testing.T) {
	e := newEnrollment(t)
	out, err := e.run(t, "t", "--endpoint", "wss://opamp.example.com/v1/opamp", "--accept-packages", "--no-start")
	require.NoError(t, err, out)
	cfg := e.loadEnrolled(t)
	assertPackages(t, cfg, true, "otelcol")
	assertPackagedPolicy(t, cfg.Agent.RemoteConfigPolicy, []string{"https://opamp.example.com"}, nil)

	e = newEnrollment(t)
	out, err = e.run(t, "t", "--endpoint", "wss://opamp.example.com/v1/opamp", "--no-start")
	require.NoError(t, err, out)
	assertPackages(t, e.loadEnrolled(t), false, "")
}

// hostLogsConfig is testdata/host-logs-<goos>.yaml: a host monitoring config that reads the
// system's logs, as a Logfire server offers one, with the given replacements made.
func hostLogsConfig(t *testing.T, goos string, replacements ...string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "host-logs-"+goos+".yaml"))
	require.NoError(t, err)
	return []byte(strings.NewReplacer(replacements...).Replace(string(body)))
}

// assertHostLogs checks a policy enrolled with --host-logs takes the host monitoring config, and
// one enrolled without refuses it.
func assertHostLogs(t *testing.T, withHostLogs, without config.RemoteConfigPolicy, body []byte, refusal string) {
	t.Helper()
	bindings := map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}
	require.NoError(t, withHostLogs.CheckWithConnectionEnv("", body, bindings))
	require.ErrorContains(t, without.CheckWithConnectionEnv("", body, bindings), refusal)
}

// --host-logs lets the server read the system's logs: the files under /var/log, with
// file_storage keeping its place in the Supervisor's own directory, and nowhere else.
func TestEnrollHostLogs(t *testing.T) {
	enroll := func(args ...string) config.RemoteConfigPolicy {
		e := newEnrollment(t)
		out, err := e.run(t, "t", append([]string{"--endpoint", "wss://logfire-us.pydantic.dev/v1/opamp", "--no-start"}, args...)...)
		require.NoError(t, err, out)
		return e.loadEnrolled(t).Agent.RemoteConfigPolicy
	}
	p := enroll("--host-logs", "--log-path", "/srv/app/logs")
	assert.Equal(t, []string{"/srv/app/logs", "/var/log"}, p.AllowedLogPaths)
	// The test's stand-in for /var/lib/pydantic-opamp, as loadEnrolled makes it.
	require.Len(t, p.AllowedStorageDirectories, 1)
	storage := p.AllowedStorageDirectories[0]
	assert.Equal(t, "collector", filepath.Base(storage))
	body := hostLogsConfig(t, "linux", "/var/lib/pydantic-opamp/collector", storage)
	assertHostLogs(t, p, enroll(), body, `configures extension "file_storage/host_logs"`)
	require.ErrorContains(t, p.Check("", []byte(`extensions: {file_storage: {directory: /etc/cron.d}}`)), "allowed_storage_directories doesn't allow")
	require.ErrorContains(t, p.Check("", []byte(`receivers: {filelog: {include: [/etc/shadow]}}`)), "allowed_log_paths doesn't allow")
}
