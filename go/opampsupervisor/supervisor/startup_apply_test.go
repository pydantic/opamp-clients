// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/commander"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// The apply of the config the Supervisor starts on: the collector's first start, and the
// client's hand-over of the config it resumes.

// A collector the Supervisor can't start the first time is FAILED, and started again on the
// restart backoff, as after a start the restart timer couldn't make. Nothing started it again
// until the server sent another config.
func TestAFirstStartThatFailsIsTriedAgain(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	exe := filepath.Join(t.TempDir(), "collector")
	cmdr, err := commander.NewCommander(zap.NewNop(), filepath.Join(t.TempDir(), agentLogFileName), config.Agent{
		Executable:      exe,
		Env:             map[string]string{supervisorRestartHelperEnv: "sleep"},
		StopGracePeriod: 10 * time.Second,
	})
	require.NoError(t, err)
	a.commander = cmdr
	saved := receiversConfig("saved", "nop")
	a.remoteConfig.Store(saved)
	_, err = a.composeMergedConfig(saved)
	require.NoError(t, err)
	require.NoError(t, a.writeAgentConfig())
	startAgentLoop(t, a.Supervisor)

	a.waitFor(t, "saved FAILED")
	copyExecutable(t, os.Args[0], exe)
	require.Eventually(t, a.commander.IsRunning, 15*time.Second, 50*time.Millisecond,
		"the collector is started again on the restart backoff")
}

// The config the Supervisor starts on is answered when the client hands it over again, as on
// any apply: APPLIED once the collector started on it reports healthy, FAILED if it doesn't, and
// in neither case is the collector restarted. When the collector had run it last, it was APPLIED
// at once, before the collector started on it had reported anything.
func TestAResumedConfigIsAnsweredOnTheNewCollectorsHealth(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "healthy", false: "never healthy"}[healthy], func(t *testing.T) {
			a := newApplyTestSupervisor(t, "sleep")
			saved := receiversConfig("saved", "nop")
			a.resumed = opamp.Resumed{RemoteConfig: saved, AppliedRemoteConfig: saved}
			require.NoError(t, a.loadAndWriteInitialMergedConfig())
			startAgentLoop(t, a.Supervisor)
			require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
			pid := a.commander.Pid()
			conn := a.connect()

			a.offer(t, saved) // the client's hand-over
			a.waitFor(t, "saved APPLYING")
			require.NotContains(t, a.reported(), "saved APPLIED", "nothing is known of this collector yet")
			if healthy {
				a.health(conn, true)
				a.waitFor(t, "saved APPLIED")
			} else {
				a.waitFor(t, "saved FAILED")
				require.NotContains(t, a.reported(), "saved APPLIED")
			}
			require.Equal(t, pid, a.commander.Pid(), "answering the resumed config doesn't restart the collector")
		})
	}
}

// A resumed empty config map runs no collector, and that is all it asks: the hand-over of it is
// APPLIED, as a new empty config is. With nothing known of the config the Supervisor starts on,
// the window the hand-over opened found no collector running and made it FAILED.
func TestAResumedEmptyConfigIsApplied(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	a.config.Agent.ConfigApplyTimeout = 300 * time.Millisecond
	empty := &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{}, ConfigHash: []byte("empty")}
	a.resumed = opamp.Resumed{RemoteConfig: empty, AppliedRemoteConfig: empty}
	require.NoError(t, a.loadAndWriteInitialMergedConfig())
	require.True(t, a.cfgState.Load().(*configState).configMapIsEmpty)
	startAgentLoop(t, a.Supervisor)

	a.offer(t, empty) // the client's hand-over
	a.waitFor(t, "empty APPLIED")
	time.Sleep(time.Second) // past an apply window
	require.NotContains(t, a.reported(), "empty FAILED")
	require.False(t, a.commander.IsRunning())
}

// Health counts for an apply only while the connection it came on is the collector's. A
// collector that reported healthy and then crashed says nothing of the one started after it,
// which may not even have connected: the window the client's hand-over opens at a start counted
// health from any connection since the Supervisor started, and made the config APPLIED for a
// collector that never reported.
func TestHealthOfACollectorSinceGoneDoesNotCount(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	apply := &configApply{since: 0}
	conn := a.connect()
	a.health(conn, true)
	require.True(t, a.healthySince(apply))

	a.agentConn.release(conn) // the collector exited
	require.False(t, a.healthySince(apply), "its connection is closed")
	later := a.connect()
	require.False(t, a.healthySince(apply), "the next collector hasn't reported")
	a.health(later, true)
	require.True(t, a.healthySince(apply))
}

// copyExecutable puts a copy of the executable from at to.
func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	require.NoError(t, err)
	require.NoError(t, dst.Close())
}
