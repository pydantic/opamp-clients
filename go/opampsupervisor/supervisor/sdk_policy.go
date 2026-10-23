// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

// The Logfire SDK policy, for the collector: the server sends it in the logfire-sdk-policy.json
// entry of the config map, beside the collector's YAML or alone, as it sends it to an SDK. The
// Supervisor doesn't merge that entry into the collector's config as YAML (it isn't any); it
// checks the policy with the SDK settings layer, as the SDKs do, and puts the collector's
// logfirepolicy processor, configured with it, first in every traces, metrics and logs pipeline,
// so the collector redacts and filters as an SDK under the same policy does. It tells
// the server it takes the policy (logfire.opamp.config_format and supported_controls in the agent
// description) only when the collector lists the processor among its available components.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/collector/confmap"

	"github.com/pydantic/opamp-clients/go/logfirepolicyprocessor/sdkpolicy"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

const sdkPolicyFile = sdkpolicy.PolicyFile

// policyProcessor is the processor's ID in the collector's config.
var policyProcessor = sdkpolicy.Processor

// policyPipelines are the pipeline types the processor goes into.
var policyPipelines = []string{"traces", "metrics", "logs"}

// collectorHasPolicyProcessor says whether the collector listed the logfirepolicy processor among
// its available components. A collector that reported none (without
// capabilities::reports_available_components) is taken not to have it.
func (s *Supervisor) collectorHasPolicyProcessor() bool {
	if s.availableComponents == nil {
		return false
	}
	ac, _ := s.availableComponents.Load().(*protobufs.AvailableComponents)
	_, ok := ac.GetComponents()["processors"].GetSubComponentMap()[policyProcessor]
	return ok
}

// checkSDKPolicy refuses a policy the settings layer refuses for what the collector carries out:
// the processor's controls, or none without it (then a policy is taken only with every control
// local, and changes nothing). The error is the layer's, which starts with the control.
func (s *Supervisor) checkSDKPolicy(body []byte) error {
	host := &settingspb.Host{}
	if s.collectorHasPolicyProcessor() {
		host = sdkpolicy.Host()
	}
	_, err := sdkpolicy.SettingsFor(context.Background(), body, host)
	return err
}

// composeSDKPolicy configures the processor with the remote config's policy and puts it first in
// every traces, metrics and logs pipeline of conf (the config composed so far: the local config
// files' pipelines and the remote config's). Without a policy entry, or without the processor in
// the collector (where checkSDKPolicy took the policy only with nothing to do), conf is as it was.
func (s *Supervisor) composeSDKPolicy(conf *confmap.Conf, rc *protobufs.AgentRemoteConfig) error {
	entry, ok := rc.GetConfig().GetConfigMap()[sdkPolicyFile]
	if !ok || !s.collectorHasPolicyProcessor() {
		return nil
	}
	pipelines, _ := conf.Get("service::pipelines").(map[string]any)
	add := map[string]any{"processors": map[string]any{policyProcessor: map[string]any{"policy": string(entry.GetBody())}}}
	withProcessor := map[string]any{}
	for id, p := range pipelines {
		if !slices.Contains(policyPipelines, strings.SplitN(id, "/", 2)[0]) {
			continue
		}
		pipeline, _ := p.(map[string]any)
		if pipeline == nil {
			pipeline = map[string]any{}
		}
		var processors []any
		if list, ok := pipeline["processors"].([]any); ok {
			processors = slices.DeleteFunc(slices.Clone(list), func(v any) bool { return v == policyProcessor })
		}
		withProcessor[id] = map[string]any{"processors": append([]any{policyProcessor}, processors...)}
	}
	if len(withProcessor) > 0 {
		add["service"] = map[string]any{"pipelines": withProcessor}
	}
	if err := conf.Merge(confmap.NewFromStringMap(add)); err != nil {
		return fmt.Errorf("%s: %w", sdkPolicyFile, err)
	}
	return nil
}

// withSDKPolicyDescription is the agent description's non-identifying attributes with the
// policy's format and the processor's controls when the collector has the processor, and without
// them otherwise.
func (s *Supervisor) withSDKPolicyDescription(attrs []*protobufs.KeyValue) []*protobufs.KeyValue {
	attrs = slices.DeleteFunc(slices.Clone(attrs), func(kv *protobufs.KeyValue) bool {
		return strings.HasPrefix(kv.GetKey(), "logfire.opamp.")
	})
	if !s.collectorHasPolicyProcessor() {
		return attrs
	}
	policy, err := sdkpolicy.Description(context.Background())
	if err != nil {
		s.telemetrySettings.Logger.Error("Can't describe the Logfire SDK policy the collector takes: " + err.Error())
		return attrs
	}
	return append(attrs, policy...)
}
