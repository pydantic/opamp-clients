// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import "runtime"

// logSamplingGOOS is the operating system the default below is for; a variable for the tests.
var logSamplingGOOS = runtime.GOOS

// macOSLogSampling is the collector's own log sampling by default on macOS: in each minute, the
// first 5 lines with the same level and message, then one in 500. Upstream's macOS unified log
// receiver logs "Running log command" at info for each poll, and it polls again 100 ms after any
// poll that found logs, so on a busy Mac that line came about once a second, into the
// collector's log and its own telemetry. Every other line passes as before unless it repeats as
// often. It is merged with the Supervisor's own telemetry section, before the remote config, so a
// config that sets service::telemetry::logs::sampling has its own.
const macOSLogSampling = `service:
  telemetry:
    logs:
      sampling:
        enabled: true
        tick: 60s
        initial: 5
        thereafter: 500
`

// composeLogSamplingDefault is that default on macOS, and nothing elsewhere.
func (s *Supervisor) composeLogSamplingDefault() []byte {
	if logSamplingGOOS != "darwin" {
		return nil
	}
	return []byte(macOSLogSampling)
}
