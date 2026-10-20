// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/extension/extensiontest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/status"
)

// A health update that clears the error is reported, though the status stays: upstream's kept
// the last error, so the server went on seeing an error that had cleared.
func TestHealthReportingReportsAClearedError(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	statusUpdateChannel := make(chan *status.AggregateStatus)

	var mtx sync.Mutex
	var lastErrors []string
	mockOpampClient := &mockOpAMPClient{
		setHealthFunc: func(health *protobufs.ComponentHealth) error {
			mtx.Lock()
			defer mtx.Unlock()
			lastErrors = append(lastErrors, health.LastError)
			return nil
		},
	}
	sa := &mockStatusAggregator{statusChan: statusUpdateChannel}
	o := newTestOpampAgent(cfg, set, mockOpampClient, sa)
	o.initHealthReporting()
	require.NoError(t, o.Start(t.Context(), componenttest.NewNopHost()))

	now := time.Now()
	for _, err := range []error{errors.New("error A"), nil} {
		statusUpdateChannel <- &status.AggregateStatus{Event: &mockStatusEvent{
			status:    componentstatus.StatusPermanentError,
			err:       err,
			timestamp: now,
		}}
	}
	close(statusUpdateChannel)

	require.Eventually(t, func() bool {
		mtx.Lock()
		defer mtx.Unlock()
		return slices.Equal(lastErrors, []string{"", "error A", ""})
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, o.Shutdown(t.Context()))
}
