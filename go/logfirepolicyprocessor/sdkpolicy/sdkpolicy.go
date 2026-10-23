// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package sdkpolicy is the logfirepolicy processor's reading of a Logfire SDK policy, apart from
// the processor: what it carries out, the settings a policy sets for it, and what an agent running
// it adds to its description. The Supervisor uses it without linking the Collector's processor
// packages.
package sdkpolicy // import "github.com/pydantic/opamp-clients/go/logfirepolicyprocessor/sdkpolicy"

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// PolicyFile is the config map entry a server sends the policy in.
const PolicyFile = "logfire-sdk-policy.json"

// Processor is the processor's component type, as a collector config names it.
const Processor = "logfirepolicy"

// live are the settings this processor carries out, by their setting names
// (docs/sdk-settings.md, "The controls"): signal_exports, log_levels and attribute_redaction.
// Not head_sampling or sampling_rules: in an SDK the ratio decides only new traces and a span
// whose parent was sampled is kept, but a collector sees only finished spans, so it can't tell a
// trace a rule, a sampled caller or debug mode kept from one the ratio would drop, and the SDKs'
// trace-ID ratio samplers don't all read the trace ID alike. Not the rate caps (they would cap
// per collector, and again after an SDK's), debug, app_settings or instrumentations (nothing in a
// collector to carry them out). A policy that overrides one of those is refused by the layer,
// with the control's name.
var live = []string{
	"send_traces",
	"send_metrics",
	"send_logs",
	"send_logs_level",
	"send_logs_level_by_scope",
	"drop_attributes",
	"mask_attributes",
}

// Host is what this processor carries out, as the settings layer takes it.
func Host() *settingspb.Host { return &settingspb.Host{Live: slices.Clone(live)} }

var (
	layerOnce sync.Once
	theLayer  *sdksettings.Layer
	layerErr  error
)

// layer is one settings layer for the process: it keeps nothing between calls.
func layer(ctx context.Context) (*sdksettings.Layer, error) {
	layerOnce.Do(func() { theLayer, layerErr = sdksettings.New(context.WithoutCancel(ctx)) })
	return theLayer, layerErr
}

// Settings are what the policy (the body of a logfire-sdk-policy.json entry) sets, as the
// settings layer reads it for this processor's host; a policy the layer refuses is an error
// with the layer's reason (it starts with the control, or `schema:`, `policy:`, ...). Nothing is
// kept: a policy is read whole each time.
func Settings(ctx context.Context, policy []byte) (*settingspb.Settings, error) {
	return SettingsFor(ctx, policy, Host())
}

// SettingsFor is Settings for another host: an agent whose collector has no logfirepolicy
// processor carries out no control (an empty host), and so takes a policy only with every
// control local.
func SettingsFor(ctx context.Context, policy []byte, host *settingspb.Host) (*settingspb.Settings, error) {
	l, err := layer(ctx)
	if err != nil {
		return nil, err
	}
	config := &protobufs.AgentRemoteConfig{ConfigHash: []byte(PolicyFile), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{PolicyFile: {Body: policy, ContentType: "application/json"}}}}
	d, err := l.Decide(ctx, config, nil, nil, host, nil)
	if err != nil {
		return nil, err
	}
	if d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
		return nil, errors.New(d.GetStatus().GetErrorMessage())
	}
	return d.GetApply(), nil
}

// Description is what an agent running this processor adds to its description's non-identifying
// attributes, so that a server sends it the policy and overrides only the controls it carries
// out: logfire.opamp.config_format and logfire.opamp.supported_controls, as the settings layer
// writes them for an SDK.
func Description(ctx context.Context) ([]*protobufs.KeyValue, error) {
	l, err := layer(ctx)
	if err != nil {
		return nil, err
	}
	d, err := l.Describe(ctx, nil, Host(), 0)
	if err != nil {
		return nil, err
	}
	var out []*protobufs.KeyValue
	for _, kv := range d.GetNonIdentifyingAttributes() {
		if strings.HasPrefix(kv.GetKey(), "logfire.opamp.") {
			out = append(out, kv)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the SDK settings layer described no policy format")
	}
	return out, nil
}
