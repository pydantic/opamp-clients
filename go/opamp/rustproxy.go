// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"os"
	"sort"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// rustProxyVariables are the variables Rust mode's proxy rule reads (tests/shell-tables/README.md,
// "Rust mode's proxy rule"). The core applies the rule; the client only reads them, once.
var rustProxyVariables = []string{
	"REQUEST_METHOD",
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",
}

// rustProxies is what Rust mode tells the core about proxies. Once, at the start (Config.proxy):
// the proxy the operator configured (its URL as given, credentials and all, its CONNECT headers,
// and ResolveLocally), none with Direct, and otherwise the environment's variables, from which
// the core picks each connection's proxy by Rust mode's one rule, whatever Go's own convention
// says. The core speaks to the proxy itself, and refuses one it can't use.
type rustProxies struct {
	choice *corepb.ProxyChoice
}

// newRustProxies reads env (os.LookupEnv when nil; tests give their own).
func newRustProxies(p Proxy, env func(string) (string, bool)) *rustProxies {
	if env == nil {
		env = os.LookupEnv
	}
	c := &corepb.ProxyChoice{Direct: p.Direct, Url: lossy(p.URL), ResolveLocally: p.ResolveLocally}
	names := make([]string, 0, len(p.ConnectHeader))
	for name := range p.ConnectHeader {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range p.ConnectHeader[name] {
			c.Headers = append(c.Headers, &corepb.Header{Name: lossy(name), Value: lossy(v)})
		}
	}
	if !p.Direct && p.URL == "" {
		for _, name := range rustProxyVariables {
			if v, ok := env(name); ok {
				c.Environment = append(c.Environment, &corepb.Header{Name: name, Value: lossy(v)})
			}
		}
	}
	return &rustProxies{choice: c}
}

// answer is the proxy_answer for stream id. The core this client runs was told which proxy to
// use and asks nothing; one that asks anyway (the adversary's) hears the configured proxy, or none:
// the environment's rule is the core's alone.
func (r *rustProxies) answer(id uint64) *corepb.Input {
	a := &corepb.ProxyAnswer{Id: id}
	if !r.choice.Direct && r.choice.Url != "" {
		a.Proxy, a.Headers, a.ResolveLocally = r.choice.Url, r.choice.Headers, r.choice.ResolveLocally
	}
	return &corepb.Input{Kind: &corepb.Input_ProxyAnswer{ProxyAnswer: a}}
}
