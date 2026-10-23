// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// sdkPolicy is a Logfire SDK policy with the two controls it must send and the others given.
func sdkPolicy(controls string) string {
	return `{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"local"},` +
		`"signal_exports":{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}}` + controls + `}`
}

const redacting = `,"attribute_redaction":{"mode":"override","drop":["secret"],"mask":["*token*"]}`

func withPolicy(hash string, files map[string]string) *protobufs.AgentRemoteConfig {
	m := map[string]*protobufs.AgentConfigObject{}
	for name, body := range files {
		m[name] = &protobufs.AgentConfigObject{Body: []byte(body)}
	}
	return &protobufs.AgentRemoteConfig{ConfigHash: []byte(hash), Config: &protobufs.AgentConfigMap{ConfigMap: m}}
}

// components is what a collector lists, with the logfirepolicy processor or without.
func components(withPolicyProcessor bool) *protobufs.AvailableComponents {
	processors := map[string]*protobufs.ComponentDetails{"batch": {}}
	if withPolicyProcessor {
		processors["logfirepolicy"] = &protobufs.ComponentDetails{}
	}
	return &protobufs.AvailableComponents{Components: map[string]*protobufs.ComponentDetails{
		"processors": {SubComponentMap: processors}, "receivers": {SubComponentMap: map[string]*protobufs.ComponentDetails{"nop": {}}},
	}}
}

// policySupervisor is a Supervisor set up as Start leaves it before composing a config, under a
// remote config policy that doesn't list logfirepolicy (the Supervisor adds it, not the server),
// with a collector that has the processor or doesn't; reported gets each status.
func policySupervisor(t *testing.T, withPolicyProcessor bool, reported *[]*protobufs.RemoteConfigStatus) *Supervisor {
	cfg := setupSupervisorConfig(t, configTemplate)
	cfg.Agent.RemoteConfigPolicy = config.RemoteConfigPolicy{AllowedComponents: map[string][]string{
		"receivers": {"nop"}, "processors": {"batch"}, "exporters": {"nop"}, "connectors": {"forward"},
	}}
	s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
	require.NoError(t, err)
	s.persistentState, err = s.clientPersistentState()
	require.NoError(t, err)
	s.agentDescription.Store(&protobufs.AgentDescription{})
	s.setAvailableComponents(components(withPolicyProcessor))
	s.opampClient = &mockOpAMPClient{setRemoteConfigStatusFunc: func(rcs *protobufs.RemoteConfigStatus) error {
		*reported = append(*reported, rcs)
		return nil
	}}
	return s
}

func merged(t *testing.T, s *Supervisor) map[string]any {
	t.Helper()
	conf, err := config.NewConfFromYAML([]byte(s.cfgState.Load().(*configState).mergedConfig))
	require.NoError(t, err)
	return conf.ToStringMap()
}

const collectorYAML = `receivers: {nop: }
processors: {batch: }
exporters: {nop: }
connectors: {forward: }
service:
  pipelines:
    traces: {receivers: [nop], processors: [batch], exporters: [forward]}
    traces/out: {receivers: [forward], exporters: [nop]}
    metrics: {receivers: [nop], exporters: [nop]}
    logs/app: {receivers: [nop], processors: [batch], exporters: [nop]}
`

// The policy beside the collector's YAML: it isn't merged as YAML, the processor gets it, and
// the processor goes first in every traces, metrics and logs pipeline, before the server's own
// processors, whatever the remote config policy allows.
func TestTheSDKPolicyGoesToTheProcessorInEveryPipeline(t *testing.T) {
	var reported []*protobufs.RemoteConfigStatus
	s := policySupervisor(t, true, &reported)
	policy := sdkPolicy(redacting)
	require.True(t, s.processRemoteConfigMessage(t.Context(), withPolicy("both", map[string]string{
		"collector.yaml": collectorYAML, sdkPolicyFile: policy,
	})))
	for _, r := range reported {
		require.NotEqual(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, r.Status, r.ErrorMessage)
	}
	got := merged(t, s)
	for _, key := range []string{"schema", "head_sampling", "signal_exports", "attribute_redaction"} {
		require.NotContains(t, got, key, "the policy was merged as YAML")
	}
	require.Equal(t, map[string]any{"policy": policy}, got["processors"].(map[string]any)["logfirepolicy"])
	require.Contains(t, got["processors"], "batch")
	pipelines := got["service"].(map[string]any)["pipelines"].(map[string]any)
	want := map[string][]any{
		"traces":     {"logfirepolicy", "batch"},
		"traces/out": {"logfirepolicy"},
		"metrics":    {"logfirepolicy"},
		"logs/app":   {"logfirepolicy", "batch"},
	}
	for id, processors := range want {
		require.Equal(t, processors, pipelines[id].(map[string]any)["processors"], id)
		require.NotEmpty(t, pipelines[id].(map[string]any)["receivers"], id)
	}

	// A new policy changes the config; the same one again doesn't.
	other := sdkPolicy(`,"log_levels":{"mode":"override","level":"warn"}`)
	changed, err := s.composeMergedConfig(withPolicy("other", map[string]string{"collector.yaml": collectorYAML, sdkPolicyFile: other}))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, other, merged(t, s)["processors"].(map[string]any)["logfirepolicy"].(map[string]any)["policy"])
	changed, err = s.composeMergedConfig(withPolicy("other", map[string]string{"collector.yaml": collectorYAML, sdkPolicyFile: other}))
	require.NoError(t, err)
	require.False(t, changed)

	// Without the policy, no processor.
	changed, err = s.composeMergedConfig(withPolicy("yaml", map[string]string{"collector.yaml": collectorYAML}))
	require.NoError(t, err)
	require.True(t, changed)
	got = merged(t, s)
	require.NotContains(t, got["processors"], "logfirepolicy")
	require.Equal(t, []any{"batch"}, got["service"].(map[string]any)["pipelines"].(map[string]any)["traces"].(map[string]any)["processors"])
}

// A policy the settings layer refuses is FAILED with the layer's reason, which starts with the
// control, and the config the collector runs stays the current one.
func TestASDKPolicyTheLayerRefusesIsFailed(t *testing.T) {
	var reported []*protobufs.RemoteConfigStatus
	s := policySupervisor(t, true, &reported)
	require.True(t, s.processRemoteConfigMessage(t.Context(), withPolicy("good", map[string]string{
		"collector.yaml": collectorYAML, sdkPolicyFile: sdkPolicy(redacting),
	})))
	for hash, c := range map[string]struct{ policy, reason string }{
		"rules":  {sdkPolicy(`,"sampling_rules":{"mode":"override","rules":[{"rate":0}]}`), "sampling_rules: rules: this SDK cannot apply the requested override"},
		"caps":   {sdkPolicy(`,"trace_rate_limit":{"mode":"override","traces_per_second":3}`), "trace_rate_limit: traces_per_second: this SDK cannot apply the requested override"},
		"broken": {`{"schema":`, "policy: "},
		"schema": {`{"schema":"logfire-sdk-policy/v2"}`, "schema: "},
	} {
		require.False(t, s.processRemoteConfigMessage(t.Context(), withPolicy(hash, map[string]string{
			"collector.yaml": collectorYAML, sdkPolicyFile: c.policy,
		})), hash)
		last := reported[len(reported)-1]
		require.Equal(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, last.Status, hash)
		require.Equal(t, []byte(hash), last.LastRemoteConfigHash)
		require.Contains(t, last.ErrorMessage, c.reason, hash)
		require.Equal(t, []byte("good"), s.remoteConfig.Load().GetConfigHash())
	}
}

// A collector without the processor carries out no control: a policy with every control local
// changes nothing and adds no processor (the collector couldn't start with one), and one that
// overrides a control is FAILED, as an SDK that can't carry it out answers.
func TestACollectorWithoutTheProcessorTakesOnlyALocalPolicy(t *testing.T) {
	var reported []*protobufs.RemoteConfigStatus
	s := policySupervisor(t, false, &reported)
	require.True(t, s.processRemoteConfigMessage(t.Context(), withPolicy("local", map[string]string{
		"collector.yaml": collectorYAML, sdkPolicyFile: sdkPolicy(""),
	})))
	got := merged(t, s)
	require.NotContains(t, got["processors"], "logfirepolicy")
	require.NotContains(t, got, "schema")
	require.Equal(t, []any{"batch"}, got["service"].(map[string]any)["pipelines"].(map[string]any)["traces"].(map[string]any)["processors"])

	require.False(t, s.processRemoteConfigMessage(t.Context(), withPolicy("redacting", map[string]string{
		"collector.yaml": collectorYAML, sdkPolicyFile: sdkPolicy(redacting),
	})))
	last := reported[len(reported)-1]
	require.Equal(t, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, last.Status)
	require.Contains(t, last.ErrorMessage, "attribute_redaction: ")
}

// The agent description says the collector takes the policy, and which controls, exactly while
// the collector lists the processor; the user's own attributes stay.
func TestTheDescriptionSaysTheCollectorTakesThePolicy(t *testing.T) {
	cfg := setupSupervisorConfig(t, configTemplate)
	cfg.Agent.Description.NonIdentifyingAttributes = map[string]string{"host.role": "edge"}
	s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
	require.NoError(t, err)
	attrs := func() map[string]*protobufs.AnyValue {
		out := map[string]*protobufs.AnyValue{}
		for _, kv := range s.agentDescription.Load().(*protobufs.AgentDescription).GetNonIdentifyingAttributes() {
			_, dup := out[kv.GetKey()]
			require.False(t, dup, kv.GetKey())
			out[kv.GetKey()] = kv.GetValue()
		}
		return out
	}
	collector := func() *protobufs.AgentDescription {
		return &protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "os.type", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "linux"}}},
		}}
	}

	// The description first, then the components (as a bootstrap may get them).
	s.setAgentDescription(collector())
	require.NotContains(t, attrs(), "logfire.opamp.config_format")
	s.setAvailableComponents(components(true))
	got := attrs()
	require.Equal(t, "logfire-sdk-policy/v1", got["logfire.opamp.config_format"].GetStringValue())
	var controls []string
	for _, v := range got["logfire.opamp.supported_controls"].GetArrayValue().GetValues() {
		controls = append(controls, v.GetStringValue())
	}
	require.Equal(t, []string{"signal_exports.traces", "signal_exports.metrics", "signal_exports.logs",
		"log_levels", "attribute_redaction"}, controls)
	require.Equal(t, "linux", got["os.type"].GetStringValue())
	require.Equal(t, "edge", got["host.role"].GetStringValue())

	// A new description from the collector keeps them, once.
	s.setAgentDescription(collector())
	require.Contains(t, attrs(), "logfire.opamp.supported_controls")

	// A collector without the processor: neither.
	s.setAvailableComponents(components(false))
	require.NotContains(t, attrs(), "logfire.opamp.config_format")
	require.NotContains(t, attrs(), "logfire.opamp.supported_controls")
	s.setAgentDescription(collector())
	require.NotContains(t, attrs(), "logfire.opamp.config_format")
	require.Equal(t, "edge", attrs()["host.role"].GetStringValue())
}
