// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"net/url"
	"strings"
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

	// No machine ID, no key: nothing is added, and a host key the host's environment sets is
	// dropped, as the agent description reports none.
	s.config.Agent.Description.NonIdentifyingAttributes = nil
	hostKey = func() string { return "" }
	assert.Equal(t, map[string]string{"LOGFIRE_TOKEN": "t"}, s.collectorEnv())
	s.config.Agent.Env = nil
	assert.Equal(t, "deployment.environment=prod", s.collectorEnv()[resourceAttributesEnv])
}

// A host that sets only the older OTEL_RESOURCE keeps its attributes: the detector reads it only
// while OTEL_RESOURCE_ATTRIBUTES is empty, which the host key would otherwise make it.
func TestTheCollectorKeepsTheOlderResourceVariable(t *testing.T) {
	defer func(f func() string) { hostKey = f }(hostKey)
	hostKey = func() string { return "k" }
	t.Setenv(resourceAttributesEnv, "")
	t.Setenv(legacyResourceEnv, "deployment.environment=prod")
	s := &Supervisor{config: config.Supervisor{}}
	assert.Equal(t, "deployment.environment=prod,pydantic.fleet.host_key=k", s.collectorEnv()[resourceAttributesEnv])
	s.config.Agent.Env = map[string]string{legacyResourceEnv: "team=infra"}
	assert.Equal(t, "team=infra,pydantic.fleet.host_key=k", s.collectorEnv()[resourceAttributesEnv])

	// Without a key the older variable is left to the detector as it was.
	hostKey = func() string { return "" }
	assert.NotContains(t, s.collectorEnv(), resourceAttributesEnv)
}

func TestWithHostKeyResourceAttribute(t *testing.T) {
	// A configured key is percent-encoded: one the env detector can't parse loses every attribute.
	for key, want := range map[string]string{
		"4805ee72-bc31-42c6-8529-affc6d73868e": "pydantic.fleet.host_key=4805ee72-bc31-42c6-8529-affc6d73868e",
		"rack 4, row 2":                        "pydantic.fleet.host_key=rack%204%2C%20row%202",
		"100%":                                 "pydantic.fleet.host_key=100%25",
		"a+b=c":                                "pydantic.fleet.host_key=a%2Bb%3Dc",
		"héte":                                 "pydantic.fleet.host_key=h%C3%A9te",
	} {
		assert.Equal(t, want, withHostKeyResourceAttribute("", key), key)
		got, err := url.QueryUnescape(strings.TrimPrefix(want, "pydantic.fleet.host_key="))
		assert.NoError(t, err)
		assert.Equal(t, key, got, "the detector reads back the key")
	}
	assert.Equal(t, "a=1", withHostKeyResourceAttribute("a=1,pydantic.fleet.host_key=x", ""), "none: dropped")
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
