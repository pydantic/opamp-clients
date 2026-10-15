// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampextension

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/extensionauth"
	"go.opentelemetry.io/collector/extension/extensiontest"
	"go.uber.org/zap/zaptest"
)

// What this extension adds to upstream's config, and the combinations it refuses.
func TestConfig_ValidateAdditions(t *testing.T) {
	ws := func() *OpAMPServer { return &OpAMPServer{WS: &commonFields{Endpoint: "wss://127.0.0.1:4320/v1/opamp"}} }
	httpSrv := func() *OpAMPServer {
		return &OpAMPServer{HTTP: &httpFields{commonFields: commonFields{Endpoint: "https://127.0.0.1:4320/v1/opamp"}}}
	}
	auth := component.MustNewID("bearertokenauth")
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "accepting connection settings with a state file",
			mutate: func(c *Config) {
				c.StateFile = "/var/lib/otelcol/opamp.state"
				c.Capabilities.AcceptsOpAMPConnectionSettings = true
			},
		},
		{
			name:    "accepting connection settings needs a state file",
			mutate:  func(c *Config) { c.Capabilities.AcceptsOpAMPConnectionSettings = true },
			wantErr: "accepts_opamp_connection_settings requires state_file",
		},
		{
			name: "accepting connection settings with an auth extension",
			mutate: func(c *Config) {
				c.Server = httpSrv()
				c.Server.HTTP.Auth = auth
				c.StateFile = "/var/lib/otelcol/opamp.state"
				c.Capabilities.AcceptsOpAMPConnectionSettings = true
			},
			wantErr: "accepts_opamp_connection_settings can't be used with an auth extension",
		},
		{
			name:    "a state key needs a state file",
			mutate:  func(c *Config) { c.StateKey = "auto" },
			wantErr: "state_key requires state_file",
		},
		{
			name:    "an auth extension over ws",
			mutate:  func(c *Config) { c.Server.WS.Auth = auth },
			wantErr: "an auth extension is supported over http only, not ws",
		},
		{
			name:   "an auth extension over http",
			mutate: func(c *Config) { c.Server = httpSrv(); c.Server.HTTP.Auth = auth },
		},
		{
			name:    "no server",
			mutate:  func(c *Config) { c.Server = nil },
			wantErr: "opamp server must have at least ws or http set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			cfg.Server = ws()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

// headerAuth is an auth extension that sets a header on every request.
type headerAuth struct {
	component.StartFunc
	component.ShutdownFunc
	value string
}

var _ extensionauth.HTTPClient = headerAuth{}

func (a headerAuth) RoundTripper(base http.RoundTripper) (http.RoundTripper, error) {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", a.value)
		return base.RoundTrip(r)
	}), nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type authHost struct {
	availableComponentsHost
	exts map[component.ID]component.Component
}

func (h *authHost) GetExtensions() map[component.ID]component.Component { return h.exts }

// Over plain HTTP the auth extension wraps the client's transport, so every request carries what
// it sets (a refreshed token included), and the extension's own headers are replaced by it.
func TestAuthExtensionOverHTTP(t *testing.T) {
	ts := newTestServer(t)
	id := component.MustNewID("testauth")
	cfg := createDefaultConfig().(*Config)
	cfg.Server = transports(ts)["http"]
	cfg.Server.HTTP.Auth = id
	require.NoError(t, cfg.Validate())

	set := extensiontest.NewNopSettings(extensiontest.NopType)
	set.Logger = zaptest.NewLogger(t)
	o, err := newOpampAgent(cfg, set)
	require.NoError(t, err)
	assert.Equal(t, []component.ID{id}, o.Dependencies())
	host := &authHost{availableComponentsHost{t: t}, map[component.ID]component.Component{id: headerAuth{value: "Bearer from-auth"}}}
	require.NoError(t, o.Start(t.Context(), host))
	defer stopAgent(t, o)

	// Two polls at least: the header is set on each request, not once.
	ts.waitMsg("a second message", func(m *protobufs.AgentToServer) bool { return m.SequenceNum >= 1 })
	auths := ts.seenAuths()
	require.GreaterOrEqual(t, len(auths), 2)
	for _, a := range auths {
		assert.Equal(t, "Bearer from-auth", a)
	}
}

// A configured auth extension that isn't there, or isn't an HTTP client's, stops the start.
func TestAuthExtensionMissing(t *testing.T) {
	ts := newTestServer(t)
	id := component.MustNewID("testauth")
	for name, exts := range map[string]map[component.ID]component.Component{
		"missing":          nil,
		"not an http auth": {id: struct{ headerAuthless }{}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			cfg.Server = transports(ts)["http"]
			cfg.Server.HTTP.Auth = id
			o, err := newOpampAgent(cfg, extensiontest.NewNopSettings(extensiontest.NopType))
			require.NoError(t, err)
			require.Error(t, o.Start(t.Context(), &authHost{availableComponentsHost{t: t}, exts}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, o.Shutdown(ctx))
		})
	}
}

type headerAuthless struct {
	component.StartFunc
	component.ShutdownFunc
}
