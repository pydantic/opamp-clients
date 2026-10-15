// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// rustProxies answers the core's proxy_query in Rust mode: the proxy the operator configured (its
// URL as given, credentials and all, its CONNECT headers, and ResolveLocally), none with Direct,
// and otherwise the environment's by Rust mode's one rule (tests/shell-tables/README.md, "Rust
// mode's proxy rule"), whatever Go's own
// convention says. The core speaks to the proxy itself, and refuses one it can't use.
type rustProxies struct {
	fixed          string // the configured proxy's URL
	headers        []*corepb.Header
	direct         bool
	resolveLocally bool
	// env reads the environment (os.LookupEnv; tests give their own).
	env func(string) (string, bool)
}

func newRustProxies(p Proxy, env func(string) (string, bool)) *rustProxies {
	if env == nil {
		env = os.LookupEnv
	}
	r := &rustProxies{fixed: lossy(p.URL), direct: p.Direct, resolveLocally: p.ResolveLocally, env: env}
	names := make([]string, 0, len(p.ConnectHeader))
	for name := range p.ConnectHeader {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range p.ConnectHeader[name] {
			r.headers = append(r.headers, &corepb.Header{Name: lossy(name), Value: lossy(v)})
		}
	}
	return r
}

// answer is the proxy_answer for stream id's URL.
func (r *rustProxies) answer(id uint64, rawURL string) *corepb.Input {
	a := &corepb.ProxyAnswer{Id: id}
	switch {
	case r.direct:
	case r.fixed != "":
		a.Proxy, a.Headers, a.ResolveLocally = r.fixed, r.headers, r.resolveLocally
	default:
		if p := rustEnvProxy(r.env, rawURL); p != "" {
			a.Proxy = lossy(p)
			a.ResolveLocally = r.resolveLocally && isSOCKS5URL(p)
		}
	}
	return &corepb.Input{Kind: &corepb.Input_ProxyAnswer{ProxyAnswer: a}}
}

func isSOCKS5URL(p string) bool {
	l := strings.ToLower(p)
	return strings.HasPrefix(l, "socks5://") || strings.HasPrefix(l, "socks5h://")
}

// rustEnvProxy is the proxy the environment names for rawURL by Rust mode's rule, or "" for none:
//  1. REQUEST_METHOD set at all (CGI): none (httpoxy).
//  2. Each variable's upper-case name if it is set (even empty), else its lower-case one; a proxy
//     variable empty or only spaces is unset.
//  3. http:// and ws://: HTTP_PROXY, else ALL_PROXY; https:// and wss://: HTTPS_PROXY, else
//     ALL_PROXY. The value, trimmed, as it is (the core reads one without :// as http://).
//  4. NO_PROXY's entries send the host direct (rustNoProxy).
func rustEnvProxy(env func(string) (string, bool), rawURL string) string {
	if _, cgi := env("REQUEST_METHOD"); cgi {
		return ""
	}
	read := func(upper string) (string, bool) {
		if v, ok := env(upper); ok {
			return v, true
		}
		return env(strings.ToLower(upper))
	}
	proxy := func(upper string) string {
		v, _ := read(upper)
		return strings.TrimSpace(v)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	named := proxy("HTTP_PROXY")
	if scheme == "https" || scheme == "wss" {
		named = proxy("HTTPS_PROXY")
	}
	if named == "" {
		named = proxy("ALL_PROXY")
	}
	if named == "" {
		return ""
	}
	noProxy, _ := read("NO_PROXY")
	if rustNoProxy(noProxy, u.Hostname()) {
		return ""
	}
	return named
}

// rustNoProxy says whether NO_PROXY's list sends host (a name, or an address without brackets)
// direct: an address by an address entry that is it or a network holding it (IPv4 and IPv6 never
// match each other); a name by * , by an entry equal to it (a leading dot or none), or by one it
// ends with after a dot. Names are compared ignoring case; nothing is resolved.
func rustNoProxy(list, host string) bool {
	ip, err := netip.ParseAddr(host)
	isIP := err == nil && ip.Zone() == ""
	name := strings.ToLower(host)
	for _, raw := range strings.Split(list, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if net, ok := noProxyNetwork(entry); ok {
			if isIP && net.Contains(ip) {
				return true
			}
			continue
		}
		// A name entry: never an address's.
		if isIP {
			continue
		}
		d := strings.ToLower(entry)
		switch {
		case d == "*", d == name, strings.HasPrefix(d, ".") && d[1:] == name:
			return true
		case strings.HasSuffix(name, d) && (strings.HasPrefix(d, ".") || name[len(name)-len(d)-1] == '.'):
			return true
		}
	}
	return false
}

// noProxyNetwork reads an address entry: an address (a network of one), or address/prefix. A
// zone, brackets or anything else is no address.
func noProxyNetwork(entry string) (netip.Prefix, bool) {
	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(entry)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}
