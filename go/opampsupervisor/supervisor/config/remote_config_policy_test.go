// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package config

import (
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
exporters: {otlphttp: {endpoint: "${env:ENDPOINT}"}}
service:
  pipelines:
    traces: {receivers: [otlp], processors: [batch], exporters: [otlphttp, debug]}
`,
		"a pipeline using components the local config configures": `service: {pipelines: {logs: {receivers: [filelog], exporters: [otlphttp]}}}`,
		"a bare env reference":            `exporters: {otlphttp: {endpoint: "${ENDPOINT}"}}`,
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
