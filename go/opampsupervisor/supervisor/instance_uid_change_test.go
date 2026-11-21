// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// uidClient is an OpAMP client that takes the agent description and nothing else.
type uidClient struct{ client.OpAMPClient }

func (uidClient) SetAgentDescription(*protobufs.AgentDescription) error { return nil }

// When the server gives the host a new instance UID (an enrollment with a new key over a kept
// state), that is said once at info, and the collector's messages with the replaced UID, which
// it sends until it starts again, are no warning; a message with any other UID still is (six
// warnings followed one such reinstall).
func TestANewInstanceUIDFromTheServerIsSaidOnce(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	old, replacement, other := uuid.MustParse("01a1ffa1-abdc-7578-a4ae-09e0202d71ce"), uuid.MustParse("01a1ffa2-f985-7ab6-b003-19036f4b7ae9"), uuid.New()
	s := newComposeMergedConfigTestSupervisor(t, writeValidationExecutable(t, 0), false)
	s.config.Agent.ConfigFiles = nil
	s.persistentState = &persistentState{InstanceID: old, logger: zap.NewNop()}
	s.opampClient = uidClient{}
	s.telemetrySettings.Logger = zap.New(core)

	s.processAgentIdentificationMessage(&protobufs.AgentIdentification{NewInstanceUid: replacement[:]})
	assert.Equal(t, replacement, s.persistentState.instanceID())
	for range 6 {
		assert.NotNil(t, s.handleAgentOpAMPMessage(nil, &protobufs.AgentToServer{InstanceUid: old[:]}))
	}
	s.handleAgentOpAMPMessage(nil, &protobufs.AgentToServer{InstanceUid: other[:]})

	assert.Equal(t, 1, logs.FilterMessage("The OpAMP server gave this host a new instance UID").FilterLevelExact(zapcore.InfoLevel).Len())
	assert.Equal(t, 6, logs.FilterMessageSnippet("the instance UID the server replaced").Len(), "the replaced UID's messages, at debug")
	assert.Equal(t, 6, logs.FilterMessageSnippet("the instance UID the server replaced").FilterLevelExact(zapcore.DebugLevel).Len())
	assert.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).Len(), "another UID's")

	// The same UID again is no change.
	s.processAgentIdentificationMessage(&protobufs.AgentIdentification{NewInstanceUid: replacement[:]})
	assert.Equal(t, 1, logs.FilterMessage("The OpAMP server gave this host a new instance UID").Len())
}
