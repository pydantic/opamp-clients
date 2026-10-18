// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Mode is who speaks HTTP and WebSocket for the client.
type Mode string

const (
	// ModeCore (the default: also what the zero value "" means, unless Config.HTTPClient is
	// set): the shared core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself
	// (in the same WebAssembly module), over TCP connections Go opens and TLS
	// Go starts on them (crypto/tls, with Config.TLS). Go only connects, starts TLS, resolves
	// names (for a SOCKS5 proxy with Proxy.ResolveLocally), answers which proxy to use, moves the
	// bytes, and writes package files.
	ModeCore Mode = "core"
	// ModePlatform: Go's net/http and gorilla/websocket carry out the core's requests, WebSocket
	// connections and downloads. For an application that brings its own *http.Client
	// (Config.HTTPClient, which selects it too) or needs net/http's own behaviour: its reading of
	// the environment's proxies, HTTP/2.
	ModePlatform Mode = "platform"
)

// TLS is how the client makes TLS connections: to https:// and wss:// endpoints, to the hosts
// package files come from, and to an https:// proxy. The zero value trusts the system's CAs,
// presents no client certificate, and speaks TLS 1.2 or later.
type TLS struct {
	// CAPEM holds the PEM certificates of the CAs to trust for the server's certificate. It
	// replaces the system's CAs (and Config's RootCAs), unless IncludeSystemCAs is set. nil: none
	// given; empty but not nil (an empty file read) is an error.
	CAPEM []byte
	// IncludeSystemCAs trusts the system's CAs as well as CAPEM. Set it when package files come
	// from hosts with public certificates while the server's is signed by your own CA.
	IncludeSystemCAs bool
	// CertPEM and KeyPEM are the client certificate (PEM, the leaf first, then any
	// intermediates) and its private key (PEM: PKCS #1, PKCS #8 or SEC 1, not encrypted),
	// presented in every handshake. Both or neither (nil: neither; empty but not nil is an
	// error). They replace Config's Certificates.
	CertPEM, KeyPEM []byte
	// MinVersion and MaxVersion bound the TLS version (tls.VersionTLS12, tls.VersionTLS13); 0:
	// TLS 1.2 and the latest Go supports. Below TLS 1.2 is refused.
	MinVersion, MaxVersion uint16
	// Config is a *tls.Config to start from, for what the fields above don't cover (a
	// GetClientCertificate that picks up renewed certificates, say). The client uses a copy;
	// the fields above apply on top of it. Its NextProtos is the client's to set, and its
	// ServerName is for platform mode only: in core mode each handshake is for the host the core
	// names, and a ServerName is an error.
	Config *tls.Config
}

func (t TLS) isZero() bool {
	return t.CAPEM == nil && !t.IncludeSystemCAs && t.CertPEM == nil && t.KeyPEM == nil &&
		t.MinVersion == 0 && t.MaxVersion == 0 && t.Config == nil
}

// Proxy is how the client reaches the server through a proxy. The zero value takes the proxy
// from the environment, for HTTP, WebSocket and downloads alike: in core mode by the rule every
// SDK's core mode shares (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY and NO_PROXY; localhost is not
// special), in platform mode by net/http's (HTTP_PROXY, HTTPS_PROXY and NO_PROXY, never proxying
// localhost). Direct goes without one.
type Proxy struct {
	// URL of the proxy, with its credentials, if it needs any, as user:password@:
	//   - http://host:port: an HTTP proxy. Every connection through it is a CONNECT tunnel. (In
	//     platform mode http:// endpoints are requested through it in absolute form instead, and
	//     https://, ws:// and wss:// ones through a CONNECT tunnel.)
	//   - https://host:port: the same, over TLS to the proxy (with Config.TLS).
	//   - socks5://host:port or socks5h://host:port: a SOCKS5 proxy (username and password
	//     authentication). Either way the proxy resolves the server's name, as opamp-go, the
	//     OpenTelemetry Collector and browsers do (unlike curl, whose socks5:// resolves locally);
	//     ResolveLocally resolves it on the client instead.
	//
	// SOCKS4 (socks4://, socks4a://) and any other scheme are refused, from here or from the
	// environment alike.
	URL string
	// ConnectHeader is sent with every CONNECT to an HTTP proxy (Proxy-Authorization from URL
	// is added to it). Not with platform mode's requests in absolute form, nor to a SOCKS5 proxy.
	ConnectHeader http.Header
	// Direct connects without a proxy, ignoring HTTP_PROXY, HTTPS_PROXY and NO_PROXY. Not with
	// URL, ConnectHeader or ResolveLocally.
	Direct bool
	// ResolveLocally resolves the server's name on the client and gives a SOCKS5 proxy (socks5://
	// or socks5h://, configured or from the environment) its IP address, for proxies that can't
	// resolve names. It is for SOCKS5 only: an HTTP proxy with it is an error. Local
	// configuration only: nothing a server offers sets it.
	ResolveLocally bool
}

func (p Proxy) isZero() bool {
	return p.URL == "" && len(p.ConnectHeader) == 0 && !p.Direct && !p.ResolveLocally
}

// tlsConfig makes the tls.Config the options describe.
func (t TLS) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{}
	if t.Config != nil {
		cfg = t.Config.Clone()
	}
	if t.CAPEM != nil {
		pool := x509.NewCertPool()
		if t.IncludeSystemCAs {
			system, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("the system's CAs: %w", err)
			}
			pool = system.Clone()
		}
		if !pool.AppendCertsFromPEM(t.CAPEM) {
			return nil, errors.New("CAPEM holds no PEM certificate")
		}
		cfg.RootCAs = pool
	} else if t.IncludeSystemCAs {
		return nil, errors.New("IncludeSystemCAs without CAPEM: the system's CAs are trusted anyway")
	}
	if (t.CertPEM != nil) != (t.KeyPEM != nil) {
		return nil, errors.New("CertPEM and KeyPEM go together")
	}
	if t.CertPEM != nil {
		pair, err := tls.X509KeyPair(t.CertPEM, t.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("the client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	if t.MinVersion != 0 {
		cfg.MinVersion = t.MinVersion
	}
	if t.MaxVersion != 0 {
		cfg.MaxVersion = t.MaxVersion
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		return nil, fmt.Errorf("TLS below 1.2 (MinVersion %#x)", cfg.MinVersion)
	}
	if cfg.MaxVersion != 0 && cfg.MaxVersion < cfg.MinVersion {
		return nil, fmt.Errorf("MaxVersion %#x is below MinVersion %#x", cfg.MaxVersion, cfg.MinVersion)
	}
	return cfg, nil
}

// proxyURL checks the proxy's URL; nil when none is given (or Direct).
func (p Proxy) proxyURL() (*url.URL, error) {
	if p.Direct {
		if p.URL != "" || len(p.ConnectHeader) > 0 || p.ResolveLocally {
			return nil, errors.New("Direct goes without a proxy: no URL, ConnectHeader or ResolveLocally with it")
		}
		return nil, nil
	}
	if p.URL == "" {
		if len(p.ConnectHeader) > 0 {
			return nil, errors.New("ConnectHeader without a proxy URL")
		}
		return nil, nil
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	}
	if err := checkProxyScheme(u, p.ResolveLocally); err != nil {
		return nil, err
	}
	if isSOCKS5(u) && len(p.ConnectHeader) > 0 {
		return nil, errors.New("ConnectHeader is for HTTP proxies, not SOCKS5")
	}
	if u.Hostname() == "" {
		return nil, errors.New("no host")
	}
	return u, nil
}

func isSOCKS5(u *url.URL) bool { return u.Scheme == "socks5" || u.Scheme == "socks5h" }

// checkProxyScheme refuses a proxy the client can't use as it's configured, whether the proxy
// was configured or came from the environment: a scheme other than http, https, socks5 and
// socks5h, and an HTTP proxy with ResolveLocally.
func checkProxyScheme(u *url.URL, resolveLocally bool) error {
	switch u.Scheme {
	case "http", "https":
		if resolveLocally {
			return fmt.Errorf("ResolveLocally is for SOCKS5 proxies, not %s:// (an HTTP proxy resolves the name itself)", u.Scheme)
		}
		return nil
	case "socks5", "socks5h":
		return nil
	case "socks4", "socks4a":
		return fmt.Errorf("SOCKS4 proxies (%s://) are not supported: use socks5:// or socks5h://", u.Scheme)
	default:
		return fmt.Errorf("proxy scheme %q: want http, https, socks5 or socks5h", u.Scheme)
	}
}
