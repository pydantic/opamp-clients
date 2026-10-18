// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Changed here: the slow-stop and fake-collector helpers.

package supervisor

import (
	"fmt"
	"os"
	"os/signal"
	"testing"
	"time"

	"go.uber.org/goleak"
)

const supervisorRestartHelperEnv = "OTEL_SUPERVISOR_RESTART_HELPER"

// helperReadyFileEnv names a file the slow-stop helper creates once it handles os.Interrupt.
const helperReadyFileEnv = "OTEL_SUPERVISOR_HELPER_READY_FILE"

func TestMain(m *testing.M) {
	switch os.Getenv(supervisorRestartHelperEnv) {
	case "sleep":
		// Re-run this test binary as a stand-in Collector process so supervisor
		// restart tests can exercise real process handling without a helper script.
		time.Sleep(time.Minute)
		os.Exit(0)
	case "emit-on-interrupt":
		// Emit a shutdown log line from the child process to verify restart paths
		// clear logs produced by the intentional stop before starting again.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		_, _ = fmt.Fprint(os.Stderr, "shutdown error")
		os.Exit(0)
	case "slow-stop":
		// A Collector that takes a while to stop, so tests can tell whether a restart holds up
		// its caller for the stop.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		if ready := os.Getenv(helperReadyFileEnv); ready != "" {
			_ = os.WriteFile(ready, nil, 0o600)
		}
		<-sigCh
		time.Sleep(2 * time.Second)
		os.Exit(0)
	case "fake-collector":
		// A Collector whose opamp extension connects to the Supervisor's local server as the
		// config the Supervisor wrote says (localserver_test.go).
		os.Exit(runFakeCollector())
	}
	goleak.VerifyTestMain(m)
}
