// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/commander"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// restartTestSupervisor is a Supervisor as Start leaves it but for the commander, which the
// test sets, and the agent loop, which it starts. healths holds the health it reports.
type restartTestSupervisor struct {
	*Supervisor
	mu      sync.Mutex
	healths []*protobufs.ComponentHealth
}

func (r *restartTestSupervisor) unhealthy() []*protobufs.ComponentHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*protobufs.ComponentHealth
	for _, h := range r.healths {
		if !h.Healthy {
			out = append(out, h)
		}
	}
	return out
}

func newRestartTestSupervisor(t *testing.T, emptyConfig bool) *restartTestSupervisor {
	t.Helper()
	s, err := NewSupervisor(t.Context(), zap.NewNop(), setupSupervisorConfig(t, configTemplate))
	require.NoError(t, err)
	r := &restartTestSupervisor{Supervisor: s}
	s.runCtx = t.Context()
	s.setInstanceID(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb"))
	s.opampClient = &mockOpAMPClient{
		setHealthFunc: func(h *protobufs.ComponentHealth) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.healths = append(r.healths, h)
		},
		updateEffectiveConfigFunc: func(context.Context) error { return nil },
		setRemoteConfigStatusFunc: func(*protobufs.RemoteConfigStatus) error { return nil },
	}
	s.cfgState.Store(&configState{mergedConfig: "receivers: {}\n", configMapIsEmpty: emptyConfig})
	return r
}

func newHelperCommander(t *testing.T, helper string, env ...string) *commander.Commander {
	t.Helper()
	agentEnv := map[string]string{supervisorRestartHelperEnv: helper}
	for i := 0; i+1 < len(env); i += 2 {
		agentEnv[env[i]] = env[i+1]
	}
	cmdr, err := commander.NewCommander(zap.NewNop(), filepath.Join(t.TempDir(), agentLogFileName), config.Agent{
		Executable: os.Args[0],
		Env:        agentEnv,
	})
	require.NoError(t, err)
	return cmdr
}

// startAgentLoop runs the agent loop until the test ends; it stops the collector as it ends.
func startAgentLoop(t *testing.T, s *Supervisor) {
	t.Helper()
	s.agentWG.Go(s.runAgentProcess)
	t.Cleanup(func() {
		close(s.doneChan)
		s.agentWG.Wait()
	})
}

// A Restart in the server's first reply can come before Start has made the commander. It
// waits for the agent loop instead of panicking, and the loop carries it out.
func TestRestartCommandBeforeTheCommanderIsSet(t *testing.T) {
	r := newRestartTestSupervisor(t, false)
	require.Nil(t, r.commander)

	require.NotPanics(t, func() { require.NoError(t, r.handleRestartCommand()) })

	cmdr := newHelperCommander(t, "sleep")
	r.commander = cmdr
	startAgentLoop(t, r.Supervisor)
	require.Eventually(t, cmdr.IsRunning, 5*time.Second, 10*time.Millisecond,
		"the agent loop carries out the restart that came before it ran")
}

// The restart runs on the agent loop: the client's goroutine, which also sends heartbeats
// and reads the server's messages, isn't held up for the collector's stop.
func TestRestartCommandDoesNotWaitForTheCollectorToStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper waits for os.Interrupt")
	}
	r := newRestartTestSupervisor(t, false)
	ready := filepath.Join(t.TempDir(), "ready")
	cmdr := newHelperCommander(t, "slow-stop", helperReadyFileEnv, ready)
	r.commander = cmdr
	require.NoError(t, cmdr.Start(t.Context()))
	isReady := func() bool { _, err := os.Stat(ready); return err == nil }
	require.Eventually(t, isReady, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(ready))
	startAgentLoop(t, r.Supervisor)

	began := time.Now()
	require.NoError(t, r.handleRestartCommand())
	require.Less(t, time.Since(began), time.Second, "the restart command waited for the collector to stop (2 s)")

	require.Eventually(t, isReady, 10*time.Second, 20*time.Millisecond, "the collector was restarted")
	require.Empty(t, r.unhealthy(), "a restart is not an unexpected exit")
}

// With a deliberately empty config (the no-op config) the collector is not running, and a
// restart doesn't start it.
func TestRestartCommandKeepsAnEmptyConfigStopped(t *testing.T) {
	r := newRestartTestSupervisor(t, true)
	cmdr := newHelperCommander(t, "sleep")
	r.commander = cmdr
	require.NoError(t, cmdr.Start(t.Context()))
	startAgentLoop(t, r.Supervisor)

	require.NoError(t, r.handleRestartCommand())

	require.Eventually(t, func() bool { return !cmdr.IsRunning() }, 5*time.Second, 10*time.Millisecond,
		"the restart stops the collector")
	require.Never(t, cmdr.IsRunning, 6*time.Second, 50*time.Millisecond,
		"nor does the loop start it again (its restart backoff is 5 s)")
	require.Empty(t, r.unhealthy(), "a restart is not an unexpected exit")
}
