// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"errors"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepApply(t *testing.T) {
	cfg := &protobufs.AgentRemoteConfig{ConfigHash: []byte("a")}
	applying := configApply{config: cfg, status: statusApplying, since: 3}
	applied := configApply{config: cfg, status: statusApplied, since: 3}
	failed := configApply{config: cfg, status: statusFailed, since: 3}
	retrying := configApply{config: cfg, status: statusApplying, since: 3, retry: true}

	tests := []struct {
		name   string
		apply  configApply
		in     applyInput
		status protobufs.RemoteConfigStatuses
		report *applyReport // nil: nothing reported
		fx     applyEffects // report aside
	}{
		{
			name:   "an exit in the apply window is FAILED, and rolls back",
			apply:  applying,
			in:     applyInput{event: applyCollectorExited, windowOpen: true, exitCode: 2},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "Agent exited unexpectedly with exit code 2 while applying configuration", restore: true},
			fx:     applyEffects{backoff: true},
		},
		{
			name:   "an exit in a retry window is FAILED, and rolls nothing back",
			apply:  retrying,
			in:     applyInput{event: applyCollectorExited, windowOpen: true, exitCode: 2},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "Agent exited unexpectedly with exit code 2 while applying configuration"},
			fx:     applyEffects{backoff: true},
		},
		{
			name:   "an APPLIED config the collector exits on is FAILED",
			apply:  applied,
			in:     applyInput{event: applyCollectorExited, exitCode: 1},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "Agent exited unexpectedly with exit code 1 after applying configuration"},
			fx:     applyEffects{backoff: true},
		},
		{
			name:   "an exit of a FAILED config's collector reports nothing more",
			apply:  failed,
			in:     applyInput{event: applyCollectorExited, exitCode: 1},
			status: statusFailed,
			fx:     applyEffects{backoff: true},
		},
		{
			name:   "a restart of a FAILED config opens a retry window",
			apply:  failed,
			in:     applyInput{event: applyRestarted, starting: true, since: 7},
			status: statusApplying,
			fx:     applyEffects{openWindow: true},
		},
		{
			name:   "a restart of an APPLIED config opens nothing",
			apply:  applied,
			in:     applyInput{event: applyRestarted, starting: true, since: 7},
			status: statusApplied,
		},
		{
			// The collector stayed down until a new config came.
			name:   "a restart that can't start the collector is FAILED, and tried again",
			apply:  failed,
			in:     applyInput{event: applyRestarted, startErr: errors.New("cannot start the agent")},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "cannot start the agent"},
			fx:     applyEffects{backoff: true},
		},
		{
			name:   "healthy at the window's end, and running, is APPLIED",
			apply:  applying,
			in:     applyInput{event: applyTimedOut, healthy: true, running: true},
			status: statusApplied,
			report: &applyReport{status: statusApplied},
		},
		{
			name:   "not healthy at the window's end is FAILED, and rolls back",
			apply:  applying,
			in:     applyInput{event: applyTimedOut, running: true},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "Config apply timeout exceeded", restore: true},
		},
		{
			name:   "not healthy at a retry window's end is FAILED, and rolls nothing back",
			apply:  retrying,
			in:     applyInput{event: applyTimedOut, running: true},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "Config apply timeout exceeded"},
		},
		{
			// The collector reported healthy, then exited as the window ended, and the loop took
			// the timer before the exit: its health made the config APPLIED.
			name:   "healthy but exited at the window's end is FAILED",
			apply:  applying,
			in:     applyInput{event: applyTimedOut, healthy: true},
			status: statusFailed,
			report: &applyReport{status: statusFailed, message: "The collector exited as the config apply window ended", restore: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, fx := stepApply(tt.apply, tt.in)
			assert.Equal(t, tt.status, next.status)
			assert.Same(t, cfg, next.config, "the step is about the apply's config")
			if tt.report == nil {
				assert.Nil(t, fx.report)
			} else {
				require.NotNil(t, fx.report)
				assert.Equal(t, *tt.report, *fx.report)
				assert.Equal(t, tt.report.message, next.message)
			}
			fx.report = nil
			assert.Equal(t, tt.fx, fx)
		})
	}
}

// A retry window is about the collector the restart started: its health counts from the
// connections made after it, and an exit in it rolls nothing back.
func TestStepApplyRetryWindowIsTheRestartedCollectors(t *testing.T) {
	failed := configApply{status: statusFailed, since: 3, message: "Config apply timeout exceeded"}
	next, _ := stepApply(failed, applyInput{event: applyRestarted, starting: true, since: 7})
	assert.Equal(t, uint64(7), next.since)
	assert.True(t, next.retry)
	assert.Empty(t, next.message)
}
