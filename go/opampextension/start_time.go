// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension // import "github.com/pydantic/opamp-clients/go/opampextension"

import (
	"sync"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// startTimeState guards opampAgent.startTimeUnixNano: Ready, NotReady and the status loop report
// health from different goroutines.
type startTimeState struct {
	mu sync.Mutex
	// stopped is set from NotReady to the next Ready: reports carry no start time, and a healthy
	// one doesn't set it.
	stopped bool
}

// reportedStartTime is the start time a health report carries: the time of the first healthy
// report (its status time, or now when it has none), kept for every later report until the
// collector stops. The specification makes start_time_unix_nano the time the component started,
// and 0 while it isn't running. Upstream's never kept it, so each healthy report carried its own
// status time, and an unhealthy one from a running collector 0.
func (o *opampAgent) reportedStartTime(ch *protobufs.ComponentHealth) uint64 {
	o.startTimeState.mu.Lock()
	defer o.startTimeState.mu.Unlock()
	if o.startTimeState.stopped {
		return 0
	}
	if ch.Healthy && o.startTimeUnixNano == 0 {
		o.startTimeUnixNano = ch.StatusTimeUnixNano
		if o.startTimeUnixNano == 0 {
			o.startTimeUnixNano = uint64(time.Now().UnixNano())
		}
	}
	return o.startTimeUnixNano
}

// markStopped is NotReady's: the collector is stopping, so every report from now on carries no
// start time, even a healthy one racing it from the status loop, until Ready.
func (o *opampAgent) markStopped() {
	o.startTimeState.mu.Lock()
	defer o.startTimeState.mu.Unlock()
	o.startTimeState.stopped = true
	o.startTimeUnixNano = 0
}

// markStarted is Ready's: reports carry the start time again, set by the next healthy one.
func (o *opampAgent) markStarted() {
	o.startTimeState.mu.Lock()
	defer o.startTimeState.mu.Unlock()
	o.startTimeState.stopped = false
}
