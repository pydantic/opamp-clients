// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension // import "github.com/pydantic/opamp-clients/go/opampextension"

import (
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// reportedStartTime is the start time a health report carries: the time of the first healthy
// report (its status time, or now when it has none), kept for every later report until the
// collector stops. The specification makes start_time_unix_nano the time the component started,
// and 0 while it isn't running. Upstream's never kept it, so each healthy report carried its own
// status time, and an unhealthy one from a running collector 0.
func (o *opampAgent) reportedStartTime(ch *protobufs.ComponentHealth) uint64 {
	o.startTimeMu.Lock()
	defer o.startTimeMu.Unlock()
	if ch.Healthy && o.startTimeUnixNano == 0 {
		o.startTimeUnixNano = ch.StatusTimeUnixNano
		if o.startTimeUnixNano == 0 {
			o.startTimeUnixNano = uint64(time.Now().UnixNano())
		}
	}
	return o.startTimeUnixNano
}

// clearStartTime marks the collector stopped: reports from now on carry no start time, until it
// is healthy again.
func (o *opampAgent) clearStartTime() {
	o.startTimeMu.Lock()
	defer o.startTimeMu.Unlock()
	o.startTimeUnixNano = 0
}
