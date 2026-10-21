// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package config

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testPolicy() RemoteConfigPolicy {
	return RemoteConfigPolicy{
		AllowedComponents: map[string][]string{
			"receivers":  {"otlp"},
			"processors": {"batch"},
			"exporters":  {"otlphttp"},
		},
		AllowedProviders: []string{"env"},
	}
}

func TestRemoteConfigPolicyAllows(t *testing.T) {
	p := testPolicy()
	for name, body := range map[string]string{
		"allowed components, named or not": `
receivers: {otlp: {}, otlp/2: {}}
processors: {batch: {}}
exporters: {otlphttp: {headers: {k: "${env:TOKEN}"}}}
service:
  pipelines:
    traces: {receivers: [otlp], processors: [batch], exporters: [otlphttp, debug]}
`,
		"a pipeline using components the local config configures": `service: {pipelines: {logs: {receivers: [filelog], exporters: [otlphttp]}}}`,
		"a bare env reference":            `exporters: {otlphttp: {headers: {k: "${TOKEN}"}}}`,
		"an escaped dollar":               `processors: {batch: {}}  # costs $${file:/x}`,
		"a dollar that isn't a reference": `processors: {batch: {}}  # ^x$ and $file`,
		"an empty file":                   ``,
		"an empty section":                `receivers:`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, p.Check("", []byte(body)))
		})
	}
	require.NoError(t, RemoteConfigPolicy{}.Check("", []byte(`receivers: {filelog: {include: ["${file:/etc/shadow}"]}}`)), "no policy: anything")
}

func TestRemoteConfigPolicyRefuses(t *testing.T) {
	p := testPolicy()
	for name, tc := range map[string]struct{ body, wantErr string }{
		"a receiver not allowed":                {`receivers: {filelog: {include: [/etc/shadow]}}`, `configures receiver "filelog", a type agent::remote_config_policy::allowed_components::receivers doesn't allow`},
		"a named one of a type not allowed":     {`exporters: {file/x: {path: /tmp/x}}`, `configures exporter "file/x"`},
		"a kind not listed":                     {`extensions: {pprof: {}}`, `configures extension "pprof"`},
		"a provider not allowed":                {`exporters: {otlphttp: {headers: {k: "${file:/etc/secret}"}}}`, "references the file provider"},
		"a provider nested in an env reference": {`exporters: {otlphttp: {endpoint: "${env:${http:x}}"}}`, "references the http provider"},
		"a provider spelled with a YAML escape": {`exporters: {otlphttp: {endpoint: "\x24{file:/etc/secret}"}}`, "references the file provider"},
		"a provider in a key":                   {`exporters: {otlphttp: {headers: {"${file:/k}": v}}}`, "references the file provider"},
		"a provider in a list":                  {`exporters: {otlphttp: {x: ["a", "\x24{file:/k}"]}}`, "references the file provider"},
		"a provider under a key not a string":   {`exporters: {otlphttp: {x: {1: "\x24{file:/k}"}}}`, "references the file provider"},
		"a section that isn't a map":            {`receivers: [otlp]`, "receivers isn't a map"},
		"text that isn't YAML":                  {`receivers: {`, "isn't YAML"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, p.Check("", []byte(tc.body)), tc.wantErr)
		})
	}
	require.ErrorContains(t, p.Check("extra.yaml", []byte(`receivers: {filelog: {}}`)), `the remote config file "extra.yaml" configures`)
}

func TestRemoteConfigPolicyValidate(t *testing.T) {
	require.NoError(t, RemoteConfigPolicy{}.Validate())
	require.NoError(t, testPolicy().Validate())
	for _, tc := range []struct {
		p       RemoteConfigPolicy
		wantErr string
	}{
		{RemoteConfigPolicy{AllowedComponents: map[string][]string{"receiver": {"otlp"}}}, `"receiver" isn't one of`},
		{RemoteConfigPolicy{AllowedComponents: map[string][]string{"receivers": {"otlp/2"}}}, `"otlp/2" isn't a component type`},
		{RemoteConfigPolicy{AllowedComponents: map[string][]string{"receivers": {""}}}, `"" isn't a component type`},
		{RemoteConfigPolicy{AllowedComponents: map[string][]string{"receivers": {"otlp"}}, AllowedProviders: []string{"${env"}}, `"${env" isn't a provider scheme`},
		{RemoteConfigPolicy{AllowedProviders: []string{"env"}}, "has no effect without allowed_components"},
	} {
		require.ErrorContains(t, tc.p.Validate(), tc.wantErr)
	}
}

// A policy like the one the packages write: the components a Logfire config uses, no
// providers, one destination, and one directory of logs.
func hostPolicy() RemoteConfigPolicy {
	return RemoteConfigPolicy{
		AllowedComponents: map[string][]string{
			"receivers":  {"otlp", "hostmetrics", "filelog", "file_log"},
			"processors": {"batch"},
			"exporters":  {"otlp", "otlp_grpc", "otlphttp", "debug"},
		},
		AllowedExporterEndpoints: []string{"https://logfire-us.pydantic.dev"},
		AllowedLogPaths:          []string{"/var/log/app"},
	}
}

// Keys holding "::" are split by confmap, as the Supervisor and the collector read the config:
// the policy reads them the same way.
func TestRemoteConfigPolicyReadsKeysAsConfmapDoes(t *testing.T) {
	p := testPolicy()
	for name, tc := range map[string]struct{ body, wantErr string }{
		"a component in a key with ::":      {`"receivers::filelog": {include: [/etc/shadow]}`, `configures receiver "filelog"`},
		"a component deep in a key with ::": {`"receivers::filelog::include": [/etc/shadow]`, `configures receiver "filelog"`},
		"a component beside one with ::":    {"receivers: {otlp: {}}\n\"receivers::filelog\": {}", `configures receiver "filelog"`},
		"a component through an anchor":     {"x: &r {filelog: {}}\nreceivers: *r", `configures receiver "filelog"`},
		"a component through a merge key":   {"x: &r {filelog: {}}\nreceivers: {<<: *r}", `configures receiver "filelog"`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, p.Check("", []byte(tc.body)), tc.wantErr)
		})
	}
}

func TestRemoteConfigPolicyExporterEndpoints(t *testing.T) {
	p := hostPolicy()
	for name, body := range map[string]string{
		"the endpoint":                    `exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev"}}`,
		"a path on it":                    `exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev/v1/traces"}}`,
		"its host in another case":        `exporters: {otlphttp: {endpoint: "HTTPS://Logfire-US.Pydantic.DEV"}}`,
		"its default port, spelled":       `exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev:443"}}`,
		"its host, fully qualified":       `exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev."}}`,
		"per-signal endpoints":            `exporters: {otlphttp: {traces_endpoint: "https://logfire-us.pydantic.dev/v1/traces", logs_endpoint: "https://logfire-us.pydantic.dev/v1/logs"}}`,
		"a gRPC exporter":                 `exporters: {otlp/grpc: {endpoint: "https://logfire-us.pydantic.dev:443"}}`,
		"headers and settings":            `exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", headers: {Authorization: "lf_token"}, compression: gzip, tls: {insecure: false}}}`,
		"an exporter with no endpoint":    `exporters: {debug: {verbosity: detailed}}`,
		"an empty endpoint":               `exporters: {otlphttp: {endpoint: "", traces_endpoint: "https://logfire-us.pydantic.dev/v1/traces"}}`,
		"the collector's own telemetry":   `service: {telemetry: {logs: {level: debug}, metrics: {readers: [{periodic: {exporter: {otlp: {protocol: http/protobuf, endpoint: "https://logfire-us.pydantic.dev/v1/metrics"}}}}]}}}`,
		"its logs to the standard output": `service: {telemetry: {logs: {output_paths: [stdout], error_output_paths: [stderr]}}}`,
		"a receiver's listening endpoint": `receivers: {otlp: {protocols: {grpc: {endpoint: "0.0.0.0:4317"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, p.Check("", []byte(body)))
		})
	}
	const notAllowed = "which agent::remote_config_policy::allowed_exporter_endpoints doesn't allow"
	for name, tc := range map[string]struct{ body, wantErr string }{
		"another host":                  {`exporters: {otlphttp: {endpoint: "https://collector.example.com"}}`, `exporter "otlphttp" sends to "https://collector.example.com", ` + notAllowed},
		"a host ending in the allowed":  {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev.example.com"}}`, notAllowed},
		"the allowed host as a user":    {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev@example.com"}}`, notAllowed},
		"another scheme":                {`exporters: {otlphttp: {endpoint: "http://logfire-us.pydantic.dev"}}`, notAllowed},
		"another port":                  {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev:8443"}}`, notAllowed},
		"the http port":                 {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev:80"}}`, notAllowed},
		"a percent-encoded host":        {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev%2eexample.com"}}`, "isn't an http:// or https:// URL"},
		"no scheme":                     {`exporters: {otlp: {endpoint: "logfire-us.pydantic.dev:443"}}`, `sends to "logfire-us.pydantic.dev:443", which isn't an http:// or https:// URL`},
		"a gRPC resolver":               {`exporters: {otlp: {endpoint: "dns:///logfire-us.pydantic.dev:443"}}`, "isn't an http:// or https:// URL"},
		"a unix socket":                 {`exporters: {otlp: {endpoint: "unix:///run/x.sock"}}`, "isn't an http:// or https:// URL"},
		"an escaped dollar":             {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev/$${x}"}}`, "holds a $"},
		"a per-signal endpoint":         {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", metrics_endpoint: "https://collector.example.com"}}`, notAllowed},
		"an endpoint not a string":      {`exporters: {otlphttp: {endpoint: [https://logfire-us.pydantic.dev]}}`, `exporter "otlphttp" endpoint isn't a string`},
		"a proxy":                       {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", proxy_url: "http://proxy.example.com"}}`, `exporter "otlphttp" sets proxy_url, which only the local config may`},
		"certificates not checked":      {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", tls: {insecure_skip_verify: true}}}`, `sets insecure_skip_verify`},
		"no TLS":                        {`exporters: {otlp: {endpoint: "https://logfire-us.pydantic.dev", tls: {insecure: true}}}`, `sets insecure`},
		"its own CA":                    {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", tls: {ca_pem: "-----BEGIN CERTIFICATE-----"}}}`, `sets ca_pem`},
		"a CA file":                     {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", tls: {ca_file: /etc/shadow}}}`, `sets ca_file`},
		"another server name":           {`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", tls: {server_name_override: example.com}}}`, `sets server_name_override`},
		"an endpoint in a key with ::":  {`"exporters::otlphttp::endpoint": "https://collector.example.com"`, notAllowed},
		"an endpoint through an anchor": {"x: &e {endpoint: \"https://collector.example.com\"}\nexporters: {otlphttp: {<<: *e}}", notAllowed},
		"own telemetry elsewhere": {`service: {telemetry: {metrics: {readers: [{periodic: {exporter: {otlp: {protocol: http/protobuf, endpoint: "https://collector.example.com"}}}}]}}}`,
			`the collector's own telemetry sends to "https://collector.example.com", ` + notAllowed},
		"own logs to a file":   {`service: {telemetry: {logs: {output_paths: [/root/.ssh/authorized_keys]}}}`, `the collector's own telemetry writes to "/root/.ssh/authorized_keys"`},
		"own errors to a file": {`service: {telemetry: {logs: {error_output_paths: [stderr, /etc/cron.d/x]}}}`, `writes to "/etc/cron.d/x"`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, p.Check("", []byte(tc.body)), tc.wantErr)
		})
	}
	p.AllowedExporterEndpoints = nil
	require.ErrorContains(t, p.Check("", []byte(`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev"}}`)), notAllowed, "none listed: none allowed")
}

func TestRemoteConfigPolicyLogPaths(t *testing.T) {
	p := hostPolicy()
	filelog := func(include string) string { return `receivers: {filelog: {include: ` + include + `}}` }
	for name, body := range map[string]string{
		"a file in the directory":   filelog(`[/var/log/app/app.log]`),
		"a glob in it":              filelog(`[/var/log/app/*.log]`),
		"a glob under it":           filelog(`["/var/log/app/**/*.log"]`),
		"a character class":         filelog(`["/var/log/app/[ab].log"]`),
		"the directory itself":      filelog(`[/var/log/app]`),
		"a dot segment":             filelog(`[/var/log/app/./app.log]`),
		"a doubled slash":           filelog(`[//var/log/app//app.log]`),
		"a string, not a list":      filelog(`/var/log/app/app.log`),
		"a comma-separated string":  filelog(`"/var/log/app/a.log,/var/log/app/b.log"`),
		"a named one":               `receivers: {filelog/app: {include: [/var/log/app/a.log], exclude: [/etc/shadow]}}`,
		"settings with no include":  `receivers: {filelog: {start_at: beginning}}`,
		"not deleting what it read": `receivers: {filelog: {include: [/var/log/app/a.log], delete_after_read: false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, p.Check("", []byte(body)))
		})
	}
	const notAllowed = "which agent::remote_config_policy::allowed_log_paths doesn't allow"
	for name, tc := range map[string]struct{ body, wantErr string }{
		"a file elsewhere":             {filelog(`[/etc/shadow]`), `receiver "filelog" reads "/etc/shadow", ` + notAllowed},
		"one of two elsewhere":         {filelog(`[/var/log/app/a.log, /etc/shadow]`), `reads "/etc/shadow"`},
		"climbing out":                 {filelog(`[/var/log/app/../../../etc/shadow]`), `reads "/var/log/app/../../../etc/shadow", which could climb out with ..`},
		"climbing to the parent":       {filelog(`[/var/log/app/..]`), "climb out with .."},
		"climbing within a glob":       {filelog(`["/var/log/app/*/../../x"]`), "climb out with .."},
		"a sibling sharing the prefix": {filelog(`[/var/log/apple/x.log]`), notAllowed},
		"a glob over the prefix":       {filelog(`["/var/log/app*/x.log"]`), notAllowed},
		"a glob above it":              {filelog(`["/var/log/*/x.log"]`), notAllowed},
		"everything":                   {filelog(`["/**"]`), notAllowed},
		"braces":                       {filelog(`["/var/log/app/{a,b}.log"]`), "has braces"},
		"braces climbing out":          {filelog(`["/var/log/app/{x,../../etc/shadow}"]`), "climb out with .."},
		"an escape":                    {filelog(`['/var/log/app/\.\./\.\./etc/shadow']`), `has a \`},
		"a relative path":              {filelog(`[var/log/app/x.log]`), "isn't absolute"},
		"a relative glob":              {filelog(`["**/*.log"]`), "isn't absolute"},
		"a comma-separated string":     {filelog(`"/var/log/app/a.log,/etc/shadow"`), `reads "/etc/shadow"`},
		"a file provider":              {filelog(`["${file:/etc/shadow}"]`), "references the file provider"},
		"an escaped dollar":            {filelog(`["/var/log/app/$${HOME}"]`), "holds a $"},
		"an include not a list":        {filelog(`{a: /var/log/app/a.log}`), `receiver "filelog" include isn't a list of paths`},
		"a named one":                  {`receivers: {filelog/x: {include: [/etc/passwd]}}`, `receiver "filelog/x" reads "/etc/passwd"`},
		"by its new name":              {`receivers: {file_log: {include: [/etc/passwd]}}`, `receiver "file_log" reads "/etc/passwd"`},
		"deleting what it read":        {`receivers: {filelog: {include: [/var/log/app/a.log], delete_after_read: true}}`, `receiver "filelog" sets delete_after_read, which only the local config may`},
		"in a key with ::":             {`"receivers::filelog::include": [/etc/shadow]`, `reads "/etc/shadow"`},
		"through an anchor":            {"x: &i [/etc/shadow]\nreceivers: {filelog: {include: *i}}", `reads "/etc/shadow"`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, p.Check("", []byte(tc.body)), tc.wantErr)
		})
	}
	p.AllowedLogPaths = nil
	require.ErrorContains(t, p.Check("", []byte(filelog(`[/var/log/app/a.log]`))), notAllowed, "none listed: none allowed")
}

// Windows paths: either slash, a drive, and no other volume.
func TestRemoteConfigPolicyLogPathsOnWindows(t *testing.T) {
	allowed := []string{`C:\ProgramData\app\logs`}
	for _, include := range []string{`C:\ProgramData\app\logs\a.log`, `C:/ProgramData/app/logs/*.log`, `C:\ProgramData\app\logs\**\*.log`} {
		require.NoError(t, logPathAllowed("windows", allowed, include), include)
	}
	for include, wantErr := range map[string]string{
		`C:\ProgramData\app\logs\..\..\secret`: "climb out with ..",
		`D:\ProgramData\app\logs\a.log`:        "doesn't allow",
		`c:\programdata\app\logs\a.log`:        "doesn't allow",
		`\\server\share\a.log`:                 "isn't absolute",
		`//?/C:/ProgramData/app/logs/a.log`:    "isn't absolute",
		`C:\ProgramData\app\logs\a.log:stream`: "has a :",
		`logs\a.log`:                           "isn't absolute",
		`\ProgramData\app\logs\a.log`:          "isn't absolute",
	} {
		require.ErrorContains(t, logPathAllowed("windows", allowed, include), wantErr, include)
	}
	require.NoError(t, validateLogPath("windows", `C:\ProgramData\app\logs`))
	require.ErrorContains(t, validateLogPath("windows", `logs`), "isn't absolute")
	// Elsewhere a backslash is the glob's escape, not a separator.
	require.ErrorContains(t, logPathAllowed("linux", []string{"/var/log/app"}, `/var/log/app\x`), `has a \`)
}

// The providers: none allowed, so neither env nor file reaches into the host, wherever the
// reference is in a string.
func TestRemoteConfigPolicyNoProviders(t *testing.T) {
	p := hostPolicy()
	for _, ref := range []string{"${env:HOME}", "${HOME}", "Bearer ${env:TOKEN}", "a${file:/etc/shadow}b", "${yaml:x}", "${http://x}", "${https:x}"} {
		require.ErrorContains(t, p.Check("", []byte(`exporters: {otlphttp: {headers: {Authorization: "`+ref+`"}}}`)), "references the", ref)
	}
	require.NoError(t, p.Check("", []byte(`exporters: {otlphttp: {headers: {Authorization: "$${env:TOKEN}"}}}`)))
}

func TestRemoteConfigPolicyValidateDestinations(t *testing.T) {
	components := map[string][]string{"exporters": {"otlphttp"}}
	require.NoError(t, RemoteConfigPolicy{AllowedComponents: components,
		AllowedExporterEndpoints: []string{"https://logfire-us.pydantic.dev", "http://127.0.0.1:4318", "https://Logfire-EU.pydantic.dev:443/"},
		AllowedLogPaths:          []string{"/var/log/app", "/var/log/syslog", "/var/log/other/"},
	}.Validate())
	for _, tc := range []struct {
		p       RemoteConfigPolicy
		wantErr string
	}{
		{RemoteConfigPolicy{AllowedComponents: components, AllowedExporterEndpoints: []string{"logfire-us.pydantic.dev"}}, `allowed_exporter_endpoints: "logfire-us.pydantic.dev" isn't an http:// or https:// URL`},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedExporterEndpoints: []string{"grpc://x"}}, "isn't an http:// or https:// URL"},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedExporterEndpoints: []string{"https://x/v1/traces"}}, `"https://x/v1/traces" has a path: give scheme://host[:port]`},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedExporterEndpoints: []string{"https://u@x"}}, "has user info"},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedExporterEndpoints: []string{"https://"}}, "has no host"},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedLogPaths: []string{"var/log"}}, `allowed_log_paths: "var/log" isn't absolute`},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedLogPaths: []string{"/var/log/*"}}, `"/var/log/*" has a glob character`},
		{RemoteConfigPolicy{AllowedComponents: components, AllowedLogPaths: []string{"/var/log/../etc"}}, "climb out with .."},
		{RemoteConfigPolicy{AllowedExporterEndpoints: []string{"https://x"}}, "has no effect without allowed_components"},
		{RemoteConfigPolicy{AllowedLogPaths: []string{"/var/log"}}, "has no effect without allowed_components"},
	} {
		require.ErrorContains(t, tc.p.Validate(), tc.wantErr)
	}
}

// Whatever the server sends, Check answers (a panic would take the Supervisor down), and a
// config it allows sends nowhere and reads nothing the policy doesn't list.
func FuzzRemoteConfigPolicyCheck(f *testing.F) {
	for _, seed := range []string{
		`receivers: {filelog: {include: [/var/log/app/*.log]}}`,
		`exporters: {otlphttp: {endpoint: "https://logfire-us.pydantic.dev", tls: {insecure: false}}}`,
		`"exporters::otlp::endpoint": "https://logfire-us.pydantic.dev:443"`,
		`service: {telemetry: {logs: {output_paths: stdout}, metrics: {readers: [{periodic: {exporter: {otlp: {endpoint: 1}}}}]}}}`,
		"x: &a {include: [/etc/shadow]}\nreceivers: {filelog: *a}",
		`receivers: {filelog: [1, 2]}`,
	} {
		f.Add([]byte(seed))
	}
	p := hostPolicy()
	f.Fuzz(func(t *testing.T, body []byte) {
		if p.Check("", body) != nil {
			return
		}
		conf, err := NewConfFromYAML(body)
		require.NoError(t, err)
		receivers, _ := conf.Get("receivers").(map[string]any)
		for id, cfg := range receivers {
			if t, _, _ := strings.Cut(id, "/"); t != "filelog" {
				continue
			}
			m, _ := cfg.(map[string]any)
			include, _ := m["include"].([]any)
			for _, path := range include {
				require.NoError(t, logPathAllowed(runtime.GOOS, p.AllowedLogPaths, path.(string)))
			}
		}
	})
}

// A local credential binding permits exactly its environment name, not the env provider.
func TestRemoteConfigPolicyConnectionEnv(t *testing.T) {
	p := testPolicy()
	p.AllowedProviders = nil
	bindings := map[string]string{"example/otlp": "BOUND_TOKEN"}
	for _, reference := range []string{"${env:BOUND_TOKEN}", "${BOUND_TOKEN}", "Bearer ${env:BOUND_TOKEN}"} {
		body := []byte(`exporters: {otlphttp: {headers: {Authorization: '` + reference + `'}}}`)
		require.NoError(t, p.CheckWithConnectionEnv("", body, bindings))
		require.ErrorContains(t, p.Check("", body), "references the env provider")
		require.ErrorContains(t, p.CheckWithConnectionEnv("", body, nil), "references the env provider")
	}
	for _, reference := range []string{
		"${env:OTHER_TOKEN}", "${OTHER_TOKEN}", "${env:BOUND_TOKEN_SUFFIX}",
		"${env:BOUND_TOKEN:-fallback}", "${env:${env:BOUND_TOKEN}}", "${${BOUND_TOKEN}}",
		"${file:/etc/secret}", "${file:${env:BOUND_TOKEN}}", "${http:example.com}",
	} {
		t.Run(reference, func(t *testing.T) {
			body := []byte(`exporters: {otlphttp: {headers: {Authorization: '` + reference + `'}}}`)
			require.Error(t, p.CheckWithConnectionEnv("", body, bindings))
		})
	}
	for _, body := range []string{
		`receivers: {filelog: {include: [/etc/secret]}}`,
		`exporters: {otlphttp: {endpoint: https://untrusted.example.com, headers: {Authorization: '${env:BOUND_TOKEN}'}}}`,
	} {
		require.Error(t, p.CheckWithConnectionEnv("", []byte(body), bindings))
	}
}
