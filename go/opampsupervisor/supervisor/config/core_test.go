// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/extensiontest"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/extensions"
)

// The shared core's client takes an auth extension's headers on every request over HTTP only,
// and issued connection settings would be overwritten by them on each one.
func TestSupervisor_ValidateCoreClient(t *testing.T) {
	bearerID := component.MustNewID("bearertokenauth")
	exts := extensions.Config{bearerID: extensiontest.NewNopFactory().CreateDefaultConfig()}

	for _, tc := range []struct {
		name        string
		endpoint    string
		noAuth      bool
		accepts     bool
		errContains string
	}{
		{name: "auth over http", endpoint: "https://opamp.example/v1/opamp"},
		{name: "accepts without auth, over ws", endpoint: "wss://opamp.example/v1/opamp", noAuth: true, accepts: true},
		{name: "auth over ws", endpoint: "wss://opamp.example/v1/opamp"},
		{
			name:        "auth with connection settings",
			endpoint:    "https://opamp.example/v1/opamp",
			accepts:     true,
			errContains: "server.auth can't be used with capabilities.accepts_opamp_connection_settings",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Supervisor{
				Server:       OpAMPServer{Endpoint: tc.endpoint, Auth: bearerID},
				Extensions:   exts,
				Capabilities: Capabilities{AcceptsOpAMPConnectionSettings: tc.accepts},
			}
			if tc.noAuth {
				cfg.Server.Auth = component.ID{}
			}
			err := cfg.Validate()
			if tc.errContains != "" {
				require.ErrorContains(t, err, tc.errContains)
				return
			}
			require.NoError(t, err)
		})
	}
}
