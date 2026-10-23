// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package logfirepolicyprocessor is a Collector processor that carries out a Logfire SDK policy
// (logfire-sdk-policy/v1) on the telemetry going through it: the controls that mean the same in a
// collector as in an SDK. The policy is read and checked by the SDK settings layer, as the SDK
// integrations read it (package sdkpolicy), and matched with the matchers the Go integration uses
// (github.com/pydantic/opamp-clients/go/opamp/sdksettings/match), so a collector and an SDK under
// the same policy redact and filter alike. See README.md.
package logfirepolicyprocessor // import "github.com/pydantic/opamp-clients/go/logfirepolicyprocessor"

import (
	"context"

	"github.com/pydantic/opamp-clients/go/logfirepolicyprocessor/sdkpolicy"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// Settings are what the policy sets for this processor (sdkpolicy.Settings).
func Settings(ctx context.Context, policy []byte) (*settingspb.Settings, error) {
	return sdkpolicy.Settings(ctx, policy)
}
