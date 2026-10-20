// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The auth extension, for the core's client (coreclient.go): over plain HTTP it wraps the HTTP
// client's transport, so it runs on every request. Over WebSocket it is asked for its headers
// through a request that goes nowhere, as upstream's makeHeadersFunc does, but about every second
// (go/opamp's AuthHeader), so each upgrade carries what it gives then; the request is to the
// server's endpoint (upstream's, to example.com), and each call gets its own headers.

package opampextension

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/extensionauth"
	"go.uber.org/zap"
)

// newHTTPClient is the client go/opamp runs on (platform mode): net/http with the extension's TLS
// settings and the environment's proxies (or the configured socket), through the auth extension if one is configured over
// plain HTTP. Over WebSocket go/opamp takes the TLS settings, proxy and dialer of its transport,
// and the auth extension gives its headers through newAuthHeader.
func newHTTPClient(serverCfg *OpAMPServer, tlsCfg *tls.Config, host component.Host) (*http.Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = tlsCfg
	// With a local IPC socket (a Unix domain socket or a Windows named pipe), each connection
	// dials it in place of TCP, over HTTP and WebSocket alike, and no proxy is used. The
	// endpoint then gives only the URL scheme, request path and Host header.
	if socket := serverCfg.GetSocket(); socket != nil {
		base.Proxy = nil
		base.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return socket.Dial(ctx)
		}
	}

	if serverCfg.HTTP == nil {
		return &http.Client{Transport: base}, nil
	}
	authExt, err := authExtension(serverCfg, host)
	if err != nil || authExt == nil {
		return &http.Client{Transport: base}, err
	}
	rt, err := authExt.RoundTripper(base)
	if err != nil {
		return nil, fmt.Errorf("could not create roundtripper for authentication: %w", err)
	}
	return &http.Client{Transport: rt}, nil
}

// newAuthHeader is go/opamp's AuthHeader over WebSocket: the headers the auth extension sets on
// a request to the server's endpoint. Nil when no auth extension is configured over WebSocket.
func newAuthHeader(serverCfg *OpAMPServer, host component.Host, logger *zap.Logger) (func() http.Header, error) {
	if serverCfg.WS == nil {
		return nil, nil
	}
	authExt, err := authExtension(serverCfg, host)
	if err != nil || authExt == nil {
		return nil, err
	}
	// The extension's round tripper is made once: one that renews a token keeps it between calls.
	rt, err := authExt.RoundTripper(captureHeaders{})
	if err != nil {
		return nil, fmt.Errorf("could not create roundtripper for authentication: %w", err)
	}
	endpoint := httpURL(serverCfg.WS.Endpoint)
	return func() http.Header {
		got := http.Header{}
		ctx := context.WithValue(context.Background(), capturedKey{}, &got)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
		if err == nil {
			var resp *http.Response
			if resp, err = rt.RoundTrip(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		if err != nil {
			// No headers, rather than a connection on stale ones: the server turns it away.
			logger.Warn("The auth extension gave no headers for the OpAMP connection", zap.Error(err))
			return http.Header{}
		}
		return got
	}, nil
}

// authExtension is the configured auth extension, nil if none is configured.
func authExtension(serverCfg *OpAMPServer, host component.Host) (extensionauth.HTTPClient, error) {
	extID := serverCfg.GetAuthExtensionID()
	if extID == (component.ID{}) {
		return nil, nil
	}
	ext, ok := host.GetExtensions()[extID]
	if !ok {
		return nil, fmt.Errorf("could not find auth extension %q", extID)
	}
	authExt, ok := ext.(extensionauth.HTTPClient)
	if !ok {
		return nil, fmt.Errorf("auth extension %q is not an extensionauth.HTTPClient", extID)
	}
	return authExt, nil
}

// capturedKey is the context key of the header a captureHeaders fills in.
type capturedKey struct{}

// captureHeaders is the transport under the auth extension's round tripper over WebSocket: it
// sends nothing, keeps the headers of the request it is given, and answers 204.
type captureHeaders struct{}

func (captureHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	if got, ok := req.Context().Value(capturedKey{}).(*http.Header); ok {
		*got = req.Header.Clone()
	}
	return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// httpURL is a WebSocket endpoint as the HTTP URL of its upgrade request: ws:// as http://, and
// wss:// as https://, so that an auth extension that checks the scheme sees the one it carries.
func httpURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	return u.String()
}
