// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's extension/opampextension. Changed here: the
// component's type, stability and feature gate are declared here instead of by mdatagen.

package opampextension

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/featuregate"
)

// componentType is upstream's, so a collector config written for upstream's extension works
// unchanged. A collector can have only one of the two.
var componentType = component.MustNewType("opamp")

// remoteRestartsGate is upstream's gate under upstream's ID, for the same reason.
var remoteRestartsGate = featuregate.GlobalRegistry().MustRegister(
	"extension.opampextension.RemoteRestarts",
	featuregate.StageAlpha,
	featuregate.WithRegisterDescription("When enabled, the OpAMP extension supports restart commands from the OpAMP server through the `CommandType_Restart` command."),
	featuregate.WithRegisterReferenceURL("https://github.com/open-telemetry/opentelemetry-collector-contrib/pull/45056"),
	featuregate.WithRegisterFromVersion("v0.145.0"),
)

// NewFactory returns the extension's factory, for a collector's build (ocb's manifest) in place
// of upstream's opampextension.
func NewFactory() extension.Factory {
	return extension.NewFactory(
		componentType,
		createDefaultConfig,
		createExtension,
		component.StabilityLevelAlpha,
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		Server: &OpAMPServer{},
		Capabilities: Capabilities{
			ReportsEffectiveConfig:     true,
			ReportsHealth:              true,
			ReportsAvailableComponents: true,
			AcceptsRestartCommand:      false,
		},
		PPIDPollInterval: 5 * time.Second,
	}
}

func createExtension(_ context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
	return newOpampAgent(cfg.(*Config), set)
}
