// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension

import (
	"context"
	"testing"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// The UID the core makes (or kept) reaches the extension as upstream's AgentIdentification, the
// way a UID the server assigns does; the one it started with doesn't come back as a change.
func TestTheClientsUIDReachesTheExtension(t *testing.T) {
	var got [][]byte
	c := newCoreClient(createDefaultConfig().(*Config), zap.NewNop())
	c.callbacks = types.Callbacks{OnMessage: func(_ context.Context, m *types.MessageData) {
		got = append(got, m.AgentIdentification.GetNewInstanceUid())
	}}
	placeholder, made := [16]byte{1}, [16]byte{2}
	c.uid = placeholder

	c.onNotification(nil, opamp.Notification{Kind: opamp.InstanceUIDChanged, InstanceUID: made})
	c.onNotification(nil, opamp.Notification{Kind: opamp.InstanceUIDChanged, InstanceUID: made})
	assert.Equal(t, [][]byte{made[:]}, got)
}
