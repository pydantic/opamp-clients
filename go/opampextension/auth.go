// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's extension/opampextension. Changed here: the auth
// extension wraps the HTTP client's transport, so it runs on every request, instead of being
// asked for headers through a captured dummy request; over WebSocket it is refused (Validate).

package opampextension

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/extensionauth"
)

// newHTTPClient is the client go/opamp runs on (Go mode): net/http with the extension's TLS
// settings and the environment's proxies, through the auth extension if one is configured. Over
// WebSocket go/opamp takes the TLS settings, proxy and dialer of its transport.
func newHTTPClient(serverCfg *OpAMPServer, tlsCfg *tls.Config, host component.Host) (*http.Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = tlsCfg

	extID := serverCfg.GetAuthExtensionID()
	if extID == (component.ID{}) {
		return &http.Client{Transport: base}, nil
	}

	ext, ok := host.GetExtensions()[extID]
	if !ok {
		return nil, fmt.Errorf("could not find auth extension %q", extID)
	}

	authExt, ok := ext.(extensionauth.HTTPClient)
	if !ok {
		return nil, fmt.Errorf("auth extension %q is not an extensionauth.HTTPClient", extID)
	}

	rt, err := authExt.RoundTripper(base)
	if err != nil {
		return nil, fmt.Errorf("could not create roundtripper for authentication: %w", err)
	}
	return &http.Client{Transport: rt}, nil
}
