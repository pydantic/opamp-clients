// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"maps"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// resourceAttributesEnv is the variable OpenTelemetry reads resource attributes from: the
// Collector's resource detection processor takes it with its env detector. legacyResourceEnv is
// the older name the detector reads when resourceAttributesEnv is empty.
const (
	resourceAttributesEnv = "OTEL_RESOURCE_ATTRIBUTES"
	legacyResourceEnv     = "OTEL_RESOURCE"
)

// collectorEnv is what the Supervisor adds to the Collector's environment, over agent.env and its
// own: the connection's credentials (agent.connection_env), and the host key the agent
// description carries, as pydantic.fleet.host_key in OTEL_RESOURCE_ATTRIBUTES. A config whose
// resource detection processor lists the env detector then stamps the key on the host's data,
// with no environment reference in the config (which a remote config policy refuses), so a server
// can tell the data is that host's Collector's. The machine ID itself never leaves the host.
//
// The attributes the Collector would otherwise have read (from the connection's, agent.env's or
// the host's OTEL_RESOURCE_ATTRIBUTES, or else OTEL_RESOURCE) are kept; a host key among them is
// the Supervisor's to say, as in the agent description: replaced, or dropped when it reports none.
func (s *Supervisor) collectorEnv() map[string]string {
	env := map[string]string{}
	if e := s.connectionEnv.Load(); e != nil {
		maps.Copy(env, *e)
	}
	key := reportedHostKey(s.configuredAgentDescription().GetNonIdentifyingAttributes())
	name, base, ok := lookupEnv(env, resourceAttributesEnv)
	if !ok {
		name, base, ok = lookupEnv(s.config.Agent.Env, resourceAttributesEnv)
	}
	if !ok {
		base, ok = os.LookupEnv(resourceAttributesEnv)
	}
	if !ok || base == "" {
		// The detector reads the older variable only while this one is empty.
		if _, legacy, found := lookupEnv(s.config.Agent.Env, legacyResourceEnv); found {
			base = legacy
		} else {
			base = os.Getenv(legacyResourceEnv)
		}
	}
	value := withHostKeyResourceAttribute(base, key)
	if key == "" && value == base {
		return env // nothing to add, nothing to drop
	}
	env[name] = value
	return env
}

// lookupEnv finds name in an environment map, as the system compares names: on Windows, in any
// case. It returns the name as the map spells it, so the value set under it replaces that one.
func lookupEnv(env map[string]string, name string) (string, string, bool) {
	if v, ok := env[name]; ok {
		return name, v, true
	}
	if runtime.GOOS == "windows" {
		for k, v := range env {
			if strings.EqualFold(k, name) {
				return k, v, true
			}
		}
	}
	return name, "", false
}

// reportedHostKey is the host key an agent description's attributes carry: "" for none.
func reportedHostKey(attrs []*protobufs.KeyValue) string {
	for _, kv := range attrs {
		if kv.GetKey() == hostKeyAttribute {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// withHostKeyResourceAttribute puts pydantic.fleet.host_key=key in an OTEL_RESOURCE_ATTRIBUTES
// value (comma-separated key=value pairs) in place of one it has, or leaves it out for key "".
// The key is percent-encoded, as the format wants: a configured one can hold anything, and one the
// detector can't parse would lose every attribute (a UUID comes out as it is).
func withHostKeyResourceAttribute(value, key string) string {
	var pairs []string
	for _, pair := range strings.Split(value, ",") {
		name, _, _ := strings.Cut(pair, "=")
		if strings.TrimSpace(pair) == "" || strings.TrimSpace(name) == hostKeyAttribute {
			continue
		}
		pairs = append(pairs, pair)
	}
	if key != "" {
		pairs = append(pairs, hostKeyAttribute+"="+strings.ReplaceAll(url.QueryEscape(key), "+", "%20"))
	}
	return strings.Join(pairs, ",")
}
