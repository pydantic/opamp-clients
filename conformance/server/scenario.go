// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Scenario is the server half of a conformance scenario (the "server" object of a
// scenario file). The runner reads the rest.
type Scenario struct {
	// HeartbeatIntervalS, when set, is offered in every response as
	// connection_settings.opamp.heartbeat_interval_seconds. Zero is a valid
	// offer (it disables heartbeats), so "not set" is nil.
	HeartbeatIntervalS *uint64 `json:"heartbeat_interval_s"`
	// Capabilities, when set, replaces the server's capabilities in every response.
	Capabilities *uint64 `json:"capabilities"`
	// Lax servers never set ReportFullState on a sequence gap or unknown status.
	Lax    bool    `json:"lax"`
	Faults []Fault `json:"faults"`
	// Credentials, when set, make the server require a bearer token and offer the agent its own
	// (Registration On First Use).
	Credentials *Credentials `json:"credentials"`
	// RemoteConfig, when set, is the config the server wants every agent to run.
	RemoteConfig *RemoteConfig `json:"remote_config"`
	// OwnTelemetry, when set, is where the server wants agents to send their own telemetry.
	OwnTelemetry *OwnTelemetry `json:"own_telemetry"`
	// CustomCapabilities, when set, are the server's, in every response.
	CustomCapabilities []string `json:"custom_capabilities"`
	// Packages, when set, are offered to agents that accept them (docs/packages.md).
	Packages *Packages `json:"packages"`
	// HonourUIDRequest: every message with RequestInstanceUid gets a new UUID v7 instance UID,
	// as the spec asks of a server. Otherwise the flag is ignored.
	HonourUIDRequest bool `json:"honour_uid_request"`
	// UIDRequestDelayS: an honouring server answers a message asking for a UID this late, so a
	// client that sends more meanwhile (pipelining over WebSocket) sends them asking too.
	UIDRequestDelayS float64 `json:"uid_request_delay_s"`
	// TLS, when set, makes the server listen with TLS only (tls.go).
	TLS *TLSOpts `json:"tls"`
	// Proxy, when set, starts a proxy in front of the server (proxy.go).
	Proxy *ProxyOpts `json:"proxy"`
	// Cookie: every response sets a cookie (Set-Cookie), which no client may send back.
	Cookie bool `json:"cookie"`
	// Hostile, when set, has the server send malformed values (hostile.go).
	Hostile *Hostile `json:"hostile"`
	// Hold, when set, has the server answer nothing (hold.go): a client stops mid-connect or
	// mid-download, and the log says when it let go.
	Hold *Hold `json:"hold"`
}

// Hold: what the server holds, from FromS seconds after the driver's first request, until the
// client lets go.
type Hold struct {
	FromS float64 `json:"from_s"`
	// UntilS, when set, ends the hold: what comes after it (UntilS seconds after the driver's first
	// request) is answered as usual. What was held before stays held.
	UntilS float64 `json:"until_s"`
	// Count, when set, holds only that many (of each kind): what comes after is answered as usual.
	// A client that starts slowly (a JVM in a container) still has its first download held.
	Count int64 `json:"count"`
	// Messages: every OpAMP request and WebSocket upgrade, never answered.
	Messages bool `json:"messages"`
	// Downloads: every package file, half sent, then nothing more.
	Downloads bool `json:"downloads"`
}

// TLSOpts: the server listens with TLS, with a certificate from a CA made at start. The runner
// hands the driver the CA (DRIVER_CA_FILE) and, for client_auth, a client certificate and key the
// same CA signed (DRIVER_CLIENT_CERT, DRIVER_CLIENT_KEY).
type TLSOpts struct {
	// ClientAuth: "none" (default), "request" (verify a client certificate if one is presented)
	// or "require" (refuse a handshake without a valid one).
	ClientAuth string `json:"client_auth"`
	// Untrusted: the CA handed to the driver is not the one that signed the server's certificate,
	// so a client that verifies the server refuses to connect.
	Untrusted bool `json:"untrusted"`
}

// ProxyOpts: a proxy the driver is told to use (DRIVER_PROXY).
type ProxyOpts struct {
	// Kind: "connect" (an HTTP proxy) or "socks5".
	Kind string     `json:"kind"`
	Auth *ProxyAuth `json:"auth"`
}

// ProxyAuth: the proxy refuses clients without these credentials (Basic Proxy-Authorization, or
// SOCKS5 username/password).
type ProxyAuth struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

// Packages are offered as the spec says: to an agent that declares AcceptsPackages, while the
// aggregate hash it reports differs from the offer's. Offer first; Then (if set) once the agent
// reports every package of Offer settled, INSTALLED or INSTALL_FAILED, under Offer's hash:
// however long the agent takes to start, it sees both.
type Packages struct {
	Offer []Package `json:"offer"`
	Then  []Package `json:"then"`
}

// Package is served at /packages/<name>/<version>, to requests with "Authorization: Bearer
// packages" (the header the offer gives).
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Content string `json:"content"`
	// Corrupt: the content hash offered is not the file's.
	Corrupt bool `json:"corrupt"`
	// Gzip: the file is served gzipped, with Content-Encoding: gzip. The bytes sent are the
	// file: the content hash offered is theirs, so a client that decompresses gets another file.
	Gzip bool `json:"gzip"`
	// Redirect, when set, answers the download with redirects (redirect.go).
	Redirect *Redirect `json:"redirect"`

	// Set for a hostile package (hostile.go): its kind, and the download URL (a template) and
	// headers offered instead of the usual ones.
	hostile string
	url     string
	headers [][2]string
}

// OwnTelemetry is offered as the spec says: to an agent that declares ReportsOwn<Signal>, in every
// response while the connection-settings hash the agent last reported differs from the offer's.
type OwnTelemetry struct {
	// Signal: "metrics", "traces" or "logs".
	Signal string `json:"signal"`
	// Endpoint: the full OTLP/HTTP URL; empty for this server's own receiver (/v1/<signal>, as
	// the agent reaches the server).
	Endpoint string `json:"endpoint"`
	// Token, if set, is offered as "Authorization: Bearer <Token>".
	Token string `json:"token"`
}

// RemoteConfig is served as the spec says: in every response while the hash the agent last
// reported differs from the config's. Always: in every response, whatever the agent reports (a
// stubborn server).
type RemoteConfig struct {
	Body   string `json:"body"`
	Always bool   `json:"always"`
}

// Credentials: the server accepts Bootstrap until the agent connects with the token it offered,
// then only that (spec: the server drops the old settings once the new ones work).
type Credentials struct {
	Bootstrap string `json:"bootstrap"`
	// Offer is offered (same endpoint, "Authorization: Bearer <Offer>") in every response from
	// message OfferOn until the agent reports APPLIED or FAILED for it.
	Offer   string `json:"offer"`
	OfferOn int    `json:"offer_on"`
	// Broken: the server never accepts the token it offers.
	Broken bool `json:"broken"`
}

// Fault is something the server does to the client. Exactly one trigger is set. Times (at_s,
// window_s) are seconds after the driver's first request (an OpAMP message or a WebSocket
// upgrade), not after the server started: a driver that starts late meets them all the same.
type Fault struct {
	// Triggers.
	AtS         *float64  `json:"at_s"`       // seconds after the driver's first request
	OnMessage   *int      `json:"on_message"` // the Nth AgentToServer the server receives (1-based)
	WindowS     []float64 `json:"window_s"`   // [from, to) seconds: http_status refuses every request in the window; other actions apply to each message in it with probability P
	P           float64   `json:"p"`          // window_s, other than http_status: chance per message (0 means 1)
	Action      string    `json:"action"`
	Status      int       `json:"status"`        // http_status, reject
	RetryAfter  string    `json:"retry_after"`   // http_status: Retry-After header value
	Location    string    `json:"location"`      // http_status: a Location header (a redirect)
	RetryAfterS float64   `json:"retry_after_s"` // unavailable: retry_info
	Code        int       `json:"code"`          // close_frame
	UID         string    `json:"uid"`           // assign_uid: 32 hex chars, "v7" or "not-rfc4122"
	Body        string    `json:"body"`          // offer_config
	ContentType string    `json:"content_type"`  // offer_config
	ConfigKey   string    `json:"config_key"`    // offer_config: the config map entry ("" by default)
}

var actions = map[string]bool{
	"http_status":       true, // window_s: reject requests / upgrades with Status and Retry-After
	"reject":            true, // on_message: 413 over HTTP, BAD_REQUEST over WebSocket
	"unavailable":       true, // on_message: UNAVAILABLE (WebSocket) or 503 (HTTP)
	"report_full_state": true, // on_message: set ReportFullState
	"ask_components":    true, // on_message: set ReportAvailableComponents
	"custom_ping":       true, // on_message: custom message io.opentelemetry.echo "ping", Body as data
	"custom_other":      true, // on_message: custom message for io.opentelemetry.other, which no agent declares
	"assign_uid":        true, // on_message: agent_identification.new_instance_uid
	"offer_config":      true, // on_message: remote_config
	"close_frame":       true, // at_s or on_message: WebSocket Close frame
	"tcp_drop":          true, // at_s or on_message: close the TCP connection, no Close frame (on_message over HTTP: before the reply)
	"accept_then_drop":  true, // window_s: accept every WebSocket upgrade, then close the TCP connection at once
	"restart":           true, // at_s: forget everything about the agent
}

func loadScenario(path string) (*Scenario, error) {
	if path == "" {
		return &Scenario{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Server Scenario `json:"server"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, f := range file.Server.Faults {
		if !actions[f.Action] {
			return nil, fmt.Errorf("%s: fault %d: unknown action %q", path, i, f.Action)
		}
		triggers := 0
		if f.AtS != nil {
			triggers++
		}
		if f.OnMessage != nil {
			triggers++
		}
		if f.WindowS != nil {
			triggers++
		}
		if triggers != 1 {
			return nil, fmt.Errorf("%s: fault %d (%s): needs exactly one of at_s, on_message, window_s", path, i, f.Action)
		}
		if f.Action == "accept_then_drop" && (len(f.WindowS) != 2 || f.P != 0) {
			return nil, fmt.Errorf("%s: fault %d (accept_then_drop): needs window_s [from, to), and no p", path, i)
		}
	}
	if t := file.Server.TLS; t != nil {
		switch t.ClientAuth {
		case "", "none", "request", "require":
		default:
			return nil, fmt.Errorf("%s: tls.client_auth must be none, request or require, not %q", path, t.ClientAuth)
		}
	}
	if p := file.Server.Proxy; p != nil && p.Kind != "connect" && p.Kind != "socks5" {
		return nil, fmt.Errorf("%s: proxy.kind must be connect or socks5, not %q", path, p.Kind)
	}
	if p := file.Server.Packages; p != nil {
		for _, list := range [][]Package{p.Offer, p.Then} {
			for _, pkg := range list {
				if pkg.Redirect != nil {
					if err := pkg.Redirect.check(); err != nil {
						return nil, fmt.Errorf("%s: package %s: redirect: %w", path, pkg.Name, err)
					}
				}
			}
		}
	}
	if h := file.Server.Hostile; h != nil {
		if err := h.resolve(&file.Server); err != nil {
			return nil, fmt.Errorf("%s: hostile: %w", path, err)
		}
	}
	return &file.Server, nil
}
