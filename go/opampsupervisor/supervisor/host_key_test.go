// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// Upstream's tests compare whole agent descriptions: they run without the host key.
// TestTheAgentDescriptionCarriesTheHostKey sets one.
func init() { hostKey = func() string { return "" } }

func nonIdentifying(ad *protobufs.AgentDescription) map[string]string {
	m := map[string]string{}
	for _, kv := range ad.GetNonIdentifyingAttributes() {
		m[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return m
}

// The host key is derived from the machine ID alone, the same way on every system: a fixed
// value per machine ID, a version 4 UUID, none of the machine ID in it, and none without one.
func TestTheHostKeyIsDerivedFromTheMachineID(t *testing.T) {
	ids := []string{"2f65b672a3c448a88da7f5e695eb9659", "50DCC3EF-6AD8-5EAA-AC56-451AD340113C", "dd216d7e-6521-42dd-9df2-dd1e2e938473"}
	seen := map[string]bool{}
	for _, id := range ids {
		key := hostKeyFrom(id)
		u, err := uuid.Parse(key)
		require.NoError(t, err, key)
		assert.Equal(t, uuid.Version(4), u.Version())
		assert.Equal(t, uuid.RFC4122, u.Variant())
		assert.Equal(t, key, hostKeyFrom(id+"\n"), "the trailing newline of /etc/machine-id doesn't count")
		hexID := strings.ToLower(strings.ReplaceAll(id, "-", ""))
		assert.NotContains(t, strings.ReplaceAll(key, "-", ""), hexID[:8], "no part of the machine ID")
		assert.False(t, seen[key])
		seen[key] = true
	}
	// Fixed: the same machine ID gives the same key in every build (checked against an independent
	// HMAC-SHA256 computation).
	assert.Equal(t, "eda36ca0-708d-41c1-9bf2-9a0a4eb2d05c", hostKeyFrom("2f65b672a3c448a88da7f5e695eb9659"))
	assert.Empty(t, hostKeyFrom(""))
	assert.Empty(t, hostKeyFrom(" \n"))
	assert.Empty(t, hostKeyFrom("<unknown>"), "the SDK's word for a system it can't read")
}

// The agent description the Supervisor reports carries the host key, before and after the
// collector reports; one the config gives wins, and "" there means none; a host with no machine
// ID reports none.
func TestTheAgentDescriptionCarriesTheHostKey(t *testing.T) {
	defer func(f func() string) { hostKey = f }(hostKey)
	hostKey = func() string { return hostKeyFrom("2f65b672a3c448a88da7f5e695eb9659") }
	s := &Supervisor{agentDescription: &atomic.Value{}}

	s.setAgentDescription(&protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{
		{Key: "host.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "ubuntu"}}},
	}})
	got := nonIdentifying(s.agentDescription.Load().(*protobufs.AgentDescription))
	assert.Equal(t, hostKey(), got[hostKeyAttribute])
	assert.Equal(t, "ubuntu", got["host.name"])
	assert.NotContains(t, got, "host.id", "the raw machine ID is not reported")
	assert.Equal(t, hostKey(), nonIdentifying(s.configuredAgentDescription())[hostKeyAttribute], "before the collector reports")

	s.config.Agent.Description = config.AgentDescription{NonIdentifyingAttributes: map[string]string{hostKeyAttribute: "from-config"}}
	s.setAgentDescription(&protobufs.AgentDescription{})
	assert.Equal(t, "from-config", nonIdentifying(s.agentDescription.Load().(*protobufs.AgentDescription))[hostKeyAttribute])

	// Set to "" in the config: none is reported.
	s.config.Agent.Description = config.AgentDescription{NonIdentifyingAttributes: map[string]string{hostKeyAttribute: ""}}
	s.setAgentDescription(&protobufs.AgentDescription{})
	assert.NotContains(t, nonIdentifying(s.agentDescription.Load().(*protobufs.AgentDescription)), hostKeyAttribute)
	assert.NotContains(t, nonIdentifying(s.configuredAgentDescription()), hostKeyAttribute)

	s.config.Agent.Description = config.AgentDescription{}
	hostKey = func() string { return hostKeyFrom("") }
	s.setAgentDescription(&protobufs.AgentDescription{})
	assert.NotContains(t, nonIdentifying(s.agentDescription.Load().(*protobufs.AgentDescription)), hostKeyAttribute)
}
