// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"os"
	"sort"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// coreProxyVariables are the variables core mode's proxy rule reads (tests/shell-tables/README.md,
// its proxy rule). The core applies the rule; the client only reads them, once.
var coreProxyVariables = []string{
	"REQUEST_METHOD",
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",
}

// coreProxies is what core mode tells the core about proxies. Once, at the start (Config.proxy):
// the proxy the operator configured (its URL as given, credentials and all, its CONNECT headers,
// and ResolveLocally), none with Direct, and otherwise the environment's variables, from which
// the core picks each connection's proxy by core mode's one rule, whatever Go's own convention
// says. The core speaks to the proxy itself, and refuses one it can't use.
type coreProxies struct {
	choice *corepb.ProxyChoice
}

// newCoreProxies reads env (os.LookupEnv when nil; tests give their own).
func newCoreProxies(p Proxy, env func(string) (string, bool)) *coreProxies {
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
		for _, name := range coreProxyVariables {
			if v, ok := env(name); ok {
				c.Environment = append(c.Environment, &corepb.Header{Name: name, Value: lossy(v)})
			}
		}
	}
	return &coreProxies{choice: c}
}

// answer is the proxy_answer for stream id. The core this client runs was told which proxy to
// use and asks nothing; one that asks anyway (the adversary's) hears the configured proxy, or none:
// the environment's rule is the core's alone.
func (r *coreProxies) answer(id uint64) *corepb.Input {
	a := &corepb.ProxyAnswer{Id: id}
	if !r.choice.Direct && r.choice.Url != "" {
		a.Proxy, a.Headers, a.ResolveLocally = r.choice.Url, r.choice.Headers, r.choice.ResolveLocally
	}
	return &corepb.Input{Kind: &corepb.Input_ProxyAnswer{ProxyAnswer: a}}
}
