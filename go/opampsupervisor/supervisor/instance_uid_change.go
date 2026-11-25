// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"encoding/hex"
	"slices"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
)

// A server can give the host a new instance UID: an enrollment with a new key over a kept
// state, say, where the client starts with the UID it kept and the server takes the host as a
// new one. The collector running then still has the old UID until the Supervisor starts it again
// with the new config, so its messages to the local OpAMP server carry it for that while.

// notePreviousInstanceID says the change once, and remembers the replaced UID.
func (s *Supervisor) notePreviousInstanceID(old, replacement uuid.UUID) {
	if old == replacement {
		return
	}
	s.previousInstanceID.Store(&old)
	s.telemetrySettings.Logger.Info("The OpAMP server gave this host a new instance UID",
		zap.String("instance_uid", replacement.String()), zap.String("previous_instance_uid", old.String()))
}

// ignoreOtherInstanceUID says why a message to the local OpAMP server with an instance UID that
// isn't the Supervisor's (want) is ignored: at debug for the UID the server replaced (the
// collector started before it has it until it starts again), as a warning for any other.
func (s *Supervisor) ignoreOtherInstanceUID(message *protobufs.AgentToServer, want uuid.UUID) {
	fields := []zap.Field{zap.String("instance_uid", hex.EncodeToString(message.GetInstanceUid())), zap.String("expected_instance_uid", want.String())}
	if prev := s.previousInstanceID.Load(); prev != nil && slices.Equal(message.GetInstanceUid(), prev[:]) {
		s.telemetrySettings.Logger.Debug("Ignoring a message from the collector with the instance UID the server replaced, until it starts again", fields...)
		return
	}
	s.telemetrySettings.Logger.Warn("Ignoring a message to the local OpAMP server with another instance UID", fields...)
}
