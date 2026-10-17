// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
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
			name:   "an auth extension over ws",
			mutate: func(c *Config) { c.Server.WS.Auth = auth },
		},
		{
			name: "accepting connection settings with an auth extension over ws",
			mutate: func(c *Config) {
				c.Server.WS.Auth = auth
				c.StateFile = "/var/lib/otelcol/opamp.state"
				c.Capabilities.AcceptsOpAMPConnectionSettings = true
			},
			wantErr: "accepts_opamp_connection_settings can't be used with an auth extension",
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

// tokenAuth is an auth extension whose token the test renews.
type tokenAuth struct {
	component.StartFunc
	component.ShutdownFunc
	token *atomic.Value
}

func (a tokenAuth) RoundTripper(base http.RoundTripper) (http.RoundTripper, error) {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+a.token.Load().(string))
		return base.RoundTrip(r)
	}), nil
}

// Over WebSocket the auth extension gives the headers of each upgrade: the first one carries its
// token in place of the configured header, and an upgrade after the token is renewed carries the
// new one.
func TestAuthExtensionOverWebSocket(t *testing.T) {
	ts := newTestServer(t)
	id := component.MustNewID("testauth")
	cfg := createDefaultConfig().(*Config)
	cfg.Server = transports(ts)["ws"]
	cfg.Server.WS.Headers = map[string]configopaque.String{"Authorization": "Bearer configured"}
	cfg.Server.WS.Auth = id
	require.NoError(t, cfg.Validate())

	token := &atomic.Value{}
	token.Store("first")
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	set.Logger = zaptest.NewLogger(t)
	o, err := newOpampAgent(cfg, set)
	require.NoError(t, err)
	host := &authHost{availableComponentsHost{t: t}, map[component.ID]component.Component{id: tokenAuth{token: token}}}
	require.NoError(t, o.Start(t.Context(), host))
	defer stopAgent(t, o)

	ts.waitMsg("a first message", func(*protobufs.AgentToServer) bool { return true })
	assert.Equal(t, []string{"Bearer first"}, ts.seenAuths())

	// The client asks the extension again about every second: an upgrade after that carries the
	// renewed token. Each round drops the connection and waits for the client to open another.
	token.Store("renewed")
	deadline := time.Now().Add(waitFor)
	for round := 0; !slices.Contains(ts.seenAuths(), "Bearer renewed"); round++ {
		require.True(t, time.Now().Before(deadline), "no upgrade with the renewed token: %v", ts.seenAuths())
		ts.forget()
		ts.dropNext()
		// A health change sends a message on the open connection, and the server drops that
		// connection. The client then opens a new one.
		o.setHealth(&protobufs.ComponentHealth{Healthy: true, Status: fmt.Sprintf("round %d", round)})
		ts.waitMsg("a message after the reconnection", func(*protobufs.AgentToServer) bool { return len(ts.auths) > 0 })
	}
	assert.NotContains(t, ts.seenAuths(), "Bearer configured", "the configured header is replaced")
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
