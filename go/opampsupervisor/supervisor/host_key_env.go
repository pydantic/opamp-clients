// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"maps"
	"os"
	"strings"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// resourceAttributesEnv is the variable OpenTelemetry reads resource attributes from: the
// Collector's resource detection processor takes it with its env detector.
const resourceAttributesEnv = "OTEL_RESOURCE_ATTRIBUTES"

// collectorEnv is what the Supervisor adds to the Collector's environment, over agent.env and its
// own: the connection's credentials (agent.connection_env), and the host key the agent
// description carries, as pydantic.fleet.host_key in OTEL_RESOURCE_ATTRIBUTES. A config whose
// resource detection processor lists the env detector then stamps the key on the host's data,
// with no environment reference in the config (which a remote config policy refuses), so a server
// can tell the data is that host's. The machine ID itself never leaves the host.
func (s *Supervisor) collectorEnv() map[string]string {
	env := map[string]string{}
	if e := s.connectionEnv.Load(); e != nil {
		maps.Copy(env, *e)
	}
	key := reportedHostKey(s.configuredAgentDescription().GetNonIdentifyingAttributes())
	if key == "" {
		return env
	}
	base, ok := env[resourceAttributesEnv]
	if !ok {
		base, ok = s.config.Agent.Env[resourceAttributesEnv]
	}
	if !ok {
		base = os.Getenv(resourceAttributesEnv)
	}
	env[resourceAttributesEnv] = withHostKeyResourceAttribute(base, key)
	return env
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

// withHostKeyResourceAttribute adds pydantic.fleet.host_key=key to an OTEL_RESOURCE_ATTRIBUTES
// value (comma-separated key=value pairs), in place of one it has: the key is the Supervisor's to
// say, as it is in the agent description. A UUID needs no percent-encoding.
func withHostKeyResourceAttribute(value, key string) string {
	var pairs []string
	for _, pair := range strings.Split(value, ",") {
		name, _, _ := strings.Cut(pair, "=")
		if strings.TrimSpace(pair) == "" || strings.TrimSpace(name) == hostKeyAttribute {
			continue
		}
		pairs = append(pairs, pair)
	}
	return strings.Join(append(pairs, hostKeyAttribute+"="+key), ",")
}
