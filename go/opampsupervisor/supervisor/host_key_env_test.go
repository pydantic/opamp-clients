// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// The Collector is given the host key the agent description carries, as a resource attribute its
// resource detection processor reads (the env detector), with the connection's credentials and
// what OTEL_RESOURCE_ATTRIBUTES already said.
func TestTheCollectorIsGivenTheHostKey(t *testing.T) {
	defer func(f func() string) { hostKey = f }(hostKey)
	hostKey = func() string { return hostKeyFrom("2f65b672a3c448a88da7f5e695eb9659") }
	const key = "4805ee72-bc31-42c6-8529-affc6d73868e"
	t.Setenv(resourceAttributesEnv, "")

	s := &Supervisor{config: config.Supervisor{}}
	assert.Equal(t, map[string]string{resourceAttributesEnv: "pydantic.fleet.host_key=" + key}, s.collectorEnv())

	// The connection's credentials stay.
	s.connectionEnv.Store(&map[string]string{"LOGFIRE_TOKEN": "t"})
	assert.Equal(t, map[string]string{"LOGFIRE_TOKEN": "t", resourceAttributesEnv: "pydantic.fleet.host_key=" + key}, s.collectorEnv())

	// What the host's environment says, then agent.env, is kept; a host key there is replaced.
	t.Setenv(resourceAttributesEnv, "deployment.environment=prod,pydantic.fleet.host_key=forged")
	assert.Equal(t, "deployment.environment=prod,pydantic.fleet.host_key="+key, s.collectorEnv()[resourceAttributesEnv])
	s.config.Agent.Env = map[string]string{resourceAttributesEnv: "team=infra"}
	assert.Equal(t, "team=infra,pydantic.fleet.host_key="+key, s.collectorEnv()[resourceAttributesEnv])

	// The key the description carries: one the config sets, or none when it sets "".
	s.config.Agent.Description.NonIdentifyingAttributes = map[string]string{hostKeyAttribute: "configured"}
	assert.Equal(t, "team=infra,pydantic.fleet.host_key=configured", s.collectorEnv()[resourceAttributesEnv])
	s.config.Agent.Description.NonIdentifyingAttributes = map[string]string{hostKeyAttribute: ""}
	assert.NotContains(t, s.collectorEnv(), resourceAttributesEnv, "agent.env's own value applies unchanged")

	// No machine ID, no key: nothing is added.
	s.config.Agent.Description.NonIdentifyingAttributes = nil
	hostKey = func() string { return "" }
	assert.Equal(t, map[string]string{"LOGFIRE_TOKEN": "t"}, s.collectorEnv())
}

func TestWithHostKeyResourceAttribute(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "pydantic.fleet.host_key=k",
		"a=1":                             "a=1,pydantic.fleet.host_key=k",
		"a=1,,b=2,":                       "a=1,b=2,pydantic.fleet.host_key=k",
		" pydantic.fleet.host_key =x,a=1": "a=1,pydantic.fleet.host_key=k",
		"pydantic.fleet.host_key.other=1": "pydantic.fleet.host_key.other=1,pydantic.fleet.host_key=k",
	} {
		assert.Equal(t, want, withHostKeyResourceAttribute(in, "k"), in)
	}
}
