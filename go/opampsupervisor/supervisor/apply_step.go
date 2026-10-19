// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"fmt"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// The agent loop's decisions about the remote config apply, as one pure step: what an event does
// to the apply, and what the loop must then do. runAgentProcess gathers what the step needs (the
// apply timer, the collector's health and whether it runs, a start's result), takes the step and
// does what it says. Kept pure, every rule here is tested on its own (apply_step_test.go).

// applyEvent is an event of the agent loop the step decides on.
type applyEvent int

const (
	// applyCollectorExited: the collector exited unexpectedly.
	applyCollectorExited applyEvent = iota
	// applyRestarted: the restart timer fired and the collector was started on the config file.
	applyRestarted
	// applyTimedOut: the apply window ended.
	applyTimedOut
)

// applyInput is an event and what the loop knew when it took it.
type applyInput struct {
	event applyEvent
	// windowOpen: the apply timer was running (it is stopped now). On an exit.
	windowOpen bool
	// exitCode: the collector's exit code. On an exit.
	exitCode int
	// startErr, starting, since: the start's error, whether it started the collector (not a
	// config it isn't started on), and the collector connections counted before it. On a
	// restart.
	startErr error
	starting bool
	since    uint64
	// healthy: the collector reported healthy on a connection made since the apply began.
	// running: the collector runs. On a timeout.
	healthy bool
	running bool
}

// applyReport is an outcome the loop reports with reportApply.
type applyReport struct {
	status  protobufs.RemoteConfigStatuses
	message string
	// restore: roll back to the last working config, if it can.
	restore bool
}

// applyEffects is what the loop does after a step.
type applyEffects struct {
	// report, if set, is reported, in its message the collector's crash details on an exit.
	report *applyReport
	// backoff: start the collector again on the restart backoff, unless the report rolled back
	// (the rollback starts it).
	backoff bool
	// openWindow: (re)start the apply timer.
	openWindow bool
}

const (
	statusApplying = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING
	statusApplied  = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	statusFailed   = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
)

// stepApply is the agent loop's step on an exit, a restart or the end of an apply window: the
// apply after it, and what the loop must do.
func stepApply(a configApply, in applyInput) (configApply, applyEffects) {
	var fx applyEffects
	report := func(status protobufs.RemoteConfigStatuses, message string, restore bool) {
		a.status, a.message = status, message
		fx.report = &applyReport{status: status, message: message, restore: restore}
	}
	switch in.event {
	case applyCollectorExited:
		switch {
		case in.windowOpen:
			// FAILED now, rather than at the timeout. A retry window rolls nothing back: the
			// restored last working config crashing as well is retried on the restart backoff.
			report(statusFailed, fmt.Sprintf("Agent exited unexpectedly with exit code %d while applying configuration", in.exitCode), !a.retry)
		case a.status == statusApplied:
			// An APPLIED config the collector then exits on is FAILED.
			report(statusFailed, fmt.Sprintf("Agent exited unexpectedly with exit code %d after applying configuration", in.exitCode), false)
		}
		fx.backoff = true
	case applyRestarted:
		switch {
		case in.startErr != nil:
			// Changed here: tried again on the restart backoff. Nothing else would start it.
			report(statusFailed, in.startErr.Error(), false)
			fx.backoff = true
		case in.starting && a.status == statusFailed:
			// The config the collector failed on gets another apply window with this start,
			// and is APPLIED if it is healthy at its end.
			a.status, a.message = statusApplying, ""
			a.since, a.retry = in.since, true
			fx.openWindow = true
		}
	case applyTimedOut:
		switch {
		case !in.running:
			// Changed here: the collector exited as the window ended, its exit still to be
			// handled. Its health from before is not the config's: FAILED, as an exit in the
			// window is.
			report(statusFailed, "The collector exited as the config apply window ended", !a.retry)
		case !in.healthy:
			report(statusFailed, "Config apply timeout exceeded", !a.retry)
		default:
			report(statusApplied, "", false)
		}
	}
	return a, fx
}
