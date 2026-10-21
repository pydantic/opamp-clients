// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension

import (
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/extension/extensiontest"
)

// A health report carries the time the collector first reported healthy, while it runs, and 0
// before that and once it stops.
func TestHealthReportsKeepTheStartTime(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	var reported []uint64
	o := newTestOpampAgent(cfg, set, &mockOpAMPClient{
		setHealthFunc: func(health *protobufs.ComponentHealth) error {
			reported = append(reported, health.StartTimeUnixNano)
			return nil
		},
	}, nil)

	now := time.Now()
	at := func(d time.Duration) uint64 { return uint64(now.Add(d).UnixNano()) }

	o.setHealth(&protobufs.ComponentHealth{Healthy: false})
	o.setHealth(&protobufs.ComponentHealth{Healthy: true, StatusTimeUnixNano: at(0)})
	o.setHealth(&protobufs.ComponentHealth{Healthy: false, StatusTimeUnixNano: at(time.Second)})
	o.setHealth(&protobufs.ComponentHealth{Healthy: true, StatusTimeUnixNano: at(2 * time.Second)})
	require.NoError(t, o.NotReady())

	assert.Equal(t, []uint64{0, at(0), at(0), at(0), 0}, reported)

	// Ready carries no status time: the start time is when it was reported.
	reported = nil
	require.NoError(t, o.Ready())
	require.Len(t, reported, 1)
	assert.GreaterOrEqual(t, reported[0], at(0))
}
