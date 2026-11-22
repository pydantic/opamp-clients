// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// askAside makes a request to set the state in dir aside, as a helper that runs does, this long ago.
func askAside(t *testing.T, dir string, pid int, ago time.Duration) {
	t.Helper()
	request := filepath.Join(dir, setAsideRequest+strconv.Itoa(pid))
	require.NoError(t, os.Mkdir(request, 0o700))
	at := time.Now().Add(-ago)
	require.NoError(t, os.Chtimes(request, at, at))
}

// refusedLast leaves the status of a run the server refused.
func refusedLast(t *testing.T, dir string) {
	t.Helper()
	r := newStatusReport(dir)
	r.started(nil, "wss://opamp.example.com", [16]byte{1})
	r.connectFailed(nil, 401, "refused")
	r.close(nil)
}

// deadPID is a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

func TestSetStateAside(t *testing.T) {
	me := os.Getpid()
	for name, tc := range map[string]struct {
		ask  func(t *testing.T, dir string)
		last func(r *statusReport) // nil: none
		want bool
	}{
		"asked, and refused": {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.connectFailed(nil, 401, "") }, true},
		"by two helpers": {func(t *testing.T, dir string) { askAside(t, dir, deadPID(t), 0); askAside(t, dir, me, 0) },
			func(r *statusReport) { r.connectFailed(nil, 401, "") }, true},
		"not asked":              {func(*testing.T, string) {}, func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"by a helper that ended": {func(t *testing.T, dir string) { askAside(t, dir, deadPID(t), 0) }, func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"a while ago":            {func(t *testing.T, dir string) { askAside(t, dir, me, setAsideFor+time.Second) }, func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"later, by the clock":    {func(t *testing.T, dir string) { askAside(t, dir, me, -time.Hour) }, func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"by no process": {func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, setAsideRequest+"x"), 0o700))
		},
			func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"a file": {func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, setAsideRequest+strconv.Itoa(me)), nil, 0o600))
		}, func(r *statusReport) { r.connectFailed(nil, 401, "") }, false},
		"no run":            {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, nil, false},
		"accepted":          {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.connected(nil); r.accepted(nil) }, false},
		"accepted before":   {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.accepted(nil); r.connectFailed(nil, 401, "") }, false},
		"unreachable":       {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.connectFailed(nil, 0, "") }, false},
		"not allowed":       {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.connectFailed(nil, 403, "") }, false},
		"refused, then not": {func(t *testing.T, dir string) { askAside(t, dir, me, 0) }, func(r *statusReport) { r.connectFailed(nil, 401, ""); r.connected(nil) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, coreClientStateFile)
			require.NoError(t, os.WriteFile(state, []byte("state"), 0o600))
			tc.ask(t, dir)
			if tc.last != nil {
				r := newStatusReport(dir)
				tc.last(r)
				r.close(nil)
			}
			setStateAside(zaptest.NewLogger(t), dir, time.Now())
			assert.Equal(t, !tc.want, isFile(state))
			aside := statesSetAside(dir)
			if tc.want {
				require.Len(t, aside, 1)
				got, err := os.ReadFile(aside[0])
				require.NoError(t, err)
				assert.Equal(t, "state", string(got))
			} else {
				assert.Empty(t, aside)
			}
			assert.Empty(t, namesWith(dir, setAsideRequest), "asked once")
			setStateAside(zaptest.NewLogger(t), dir, time.Now())
			assert.Equal(t, aside, statesSetAside(dir))
		})
	}
}

// A process that has another's ID, started since, isn't it.
func TestAReusedProcessIDIsNotTheHelper(t *testing.T) {
	started, ok := processStarted(os.Getpid())
	require.True(t, ok)
	assert.WithinDuration(t, time.Now(), started, time.Hour)
	assert.True(t, ranAt(os.Getpid(), time.Now()))
	assert.False(t, ranAt(os.Getpid(), started.Add(-time.Minute)))
	assert.False(t, ranAt(deadPID(t), time.Now()))
	_, ok = processStarted(1 << 22)
	assert.False(t, ok)
}

// No state set aside is replaced by a later one (the state of a try that failed), or removed.
func TestStatesSetAsideStay(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, coreClientStateFile)
	now := time.Now()
	for i := range 3 {
		require.NoError(t, os.WriteFile(state, []byte(fmt.Sprint("state ", i)), 0o600))
		refusedLast(t, dir)
		askAside(t, dir, os.Getpid(), 0)
		setStateAside(zaptest.NewLogger(t), dir, now.Add(time.Duration(i)*time.Second))
		assert.NoFileExists(t, state)
		aside := statesSetAside(dir)
		require.Len(t, aside, i+1)
		for j, path := range aside {
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprint("state ", j), string(got))
		}
	}
	// In the second of another (or after the clock went back to it): the next free name.
	require.NoError(t, os.Mkdir(filepath.Join(dir, setAsidePrefix+now.UTC().Format("20060102T150405Z")+fmt.Sprintf(".2-%d", os.Getpid())), 0o700))
	require.NoError(t, os.WriteFile(state, []byte("state 3"), 0o600))
	refusedLast(t, dir)
	askAside(t, dir, os.Getpid(), 0)
	setStateAside(zaptest.NewLogger(t), dir, now)
	assert.NoFileExists(t, state)
	require.Len(t, statesSetAside(dir), 4)
	got, err := os.ReadFile(filepath.Join(dir, setAsidePrefix+now.UTC().Format("20060102T150405Z")+fmt.Sprintf(".3-%d", os.Getpid())))
	require.NoError(t, err)
	assert.Equal(t, "state 3", string(got))
	// The status lists them.
	r := newStatusReport(dir)
	r.started(nil, "wss://opamp.example.com", [16]byte{1})
	st, err := ReadStatus(dir)
	require.NoError(t, err)
	assert.Equal(t, statesSetAside(dir), st.SetAside)
	var out bytes.Buffer
	st.Print(&out, time.Now())
	for _, path := range st.SetAside {
		assert.Contains(t, out.String(), "credentials the server refused before this host enrolled again, in "+path)
	}
}

// A state that can't be set aside, or isn't a file, stays as it is, and what else is in the directory too.
func TestSetStateAsideLeavesWhatItCannotMove(t *testing.T) {
	t.Run("not a file", func(t *testing.T) {
		for _, plant := range []func(path string) error{
			func(path string) error { return os.Mkdir(path, 0o700) },
			func(path string) error { return os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), path) },
		} {
			dir := t.TempDir()
			state := filepath.Join(dir, coreClientStateFile)
			if err := plant(state); err != nil {
				t.Skip(err)
			}
			before, err := os.Lstat(state)
			require.NoError(t, err)
			refusedLast(t, dir)
			askAside(t, dir, os.Getpid(), 0)
			setStateAside(zaptest.NewLogger(t), dir, time.Now())
			after, err := os.Lstat(state)
			require.NoError(t, err)
			assert.Equal(t, before.Mode(), after.Mode())
		}
	})
	t.Run("a request that can't be removed", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, coreClientStateFile), []byte("state"), 0o600))
		refusedLast(t, dir)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, setAsideRequest+strconv.Itoa(os.Getpid()), "in"), 0o700))
		setStateAside(zaptest.NewLogger(t), dir, time.Now())
		assert.FileExists(t, filepath.Join(dir, coreClientStateFile))
	})
	t.Run("read-only", func(t *testing.T) {
		if os.Getuid() <= 0 {
			t.Skip("needs a user who can't write everywhere")
		}
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, coreClientStateFile), []byte("state"), 0o600))
		refusedLast(t, dir)
		askAside(t, dir, os.Getpid(), 0)
		require.NoError(t, os.Chmod(dir, 0o500))
		setStateAside(zaptest.NewLogger(t), dir, time.Now())
		require.NoError(t, os.Chmod(dir, 0o700))
		assert.FileExists(t, filepath.Join(dir, coreClientStateFile))
	})
}

// A host the server deleted enrolls again, as a new one, when asked to. However its Supervisor ends before
// the server took it on the credentials it issued then (a key for one host is used up for them), the next
// start goes on with those; the old ones are only set aside, also while the server refuses everything.
func TestAHostEnrollsAgain(t *testing.T) {
	for name := range newCoreTestServer(t).endpoints() {
		for _, how := range []string{"stopped", "killed"} {
			t.Run(name+" "+how, func(t *testing.T) {
				ts := newCoreTestServer(t)
				endpoint := ts.endpoints()[name]
				// Under ts.mu, as refuse and respond run.
				deleted, usedUp, takes, down := false, false, false, false
				ts.record(func() {
					ts.refuse = func(auth string) bool {
						if down {
							return true
						}
						return auth == "Bearer issued-1" && deleted || auth == "Bearer enroll" && usedUp
					}
				})
				ts.setRespond(func(auth string, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
					switch {
					case auth == "Bearer enroll":
						key := "Bearer issued-1"
						if deleted {
							key, usedUp = "Bearer issued-2", true
						}
						return &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
							Hash: []byte(key),
							Opamp: &protobufs.OpAMPConnectionSettings{DestinationEndpoint: endpoint,
								Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: key}}}},
						}}
					case auth == "Bearer issued-2" && !takes:
						return &protobufs.ServerToAgent{ErrorResponse: &protobufs.ServerErrorResponse{ErrorMessage: "not yet"}}
					}
					return nil
				})
				start := func(dir string) (*Supervisor, string) {
					s := clientSupervisor(t, config.OpAMPServer{Endpoint: endpoint, Headers: http.Header{"Authorization": {"Bearer enroll"}}},
						config.Capabilities{AcceptsOpAMPConnectionSettings: true}, config.Storage{Directory: dir})
					var err error
					s.persistentState, err = s.clientPersistentState()
					require.NoError(t, err)
					require.NoError(t, s.startOpAMPClient())
					return s, s.persistentState.InstanceID.String()
				}
				dir := t.TempDir()
				state := filepath.Join(dir, coreClientStateFile)
				refusedRun := func() {
					s, _ := start(dir)
					require.Eventually(t, func() bool { return lastRunRefused(dir) }, 15*time.Second, 10*time.Millisecond)
					require.NoError(t, s.stopOpAMPClient())
				}

				s, uid := start(dir)
				ts.waitMsg("the issued settings applied", func(m *protobufs.AgentToServer) bool {
					return m.ConnectionSettingsStatus.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED
				})
				require.NoError(t, s.stopOpAMPClient())
				saved, err := os.ReadFile(state)
				require.NoError(t, err)

				// The server refuses everything for a while, twice asked: each state is kept, and put back by a
				// person, the host is what it was.
				ts.record(func() { down = true })
				for range 2 {
					refusedRun()
					askAside(t, dir, os.Getpid(), 0)
					refusedRun()
				}
				aside := statesSetAside(dir)
				require.Len(t, aside, 2)
				kept, err := os.ReadFile(aside[0])
				require.NoError(t, err)
				assert.Equal(t, saved, kept)
				require.NoError(t, os.Rename(aside[0], state))
				ts.record(func() { down = false })
				s, back := start(dir)
				assert.Equal(t, uid, back)
				require.NoError(t, s.stopOpAMPClient())

				// The host is deleted.
				ts.record(func() { deleted = true })
				refusedRun()
				before, err := os.ReadFile(state)
				require.NoError(t, err)
				askAside(t, dir, os.Getpid(), 0)
				ts.forget()
				s, again := start(dir)
				assert.NotEqual(t, uid, again)
				require.Eventually(t, func() bool {
					b, err := os.ReadFile(state)
					return err == nil && bytes.Contains(b, []byte("Bearer issued-2"))
				}, 15*time.Second, time.Millisecond)
				if how == "killed" {
					// What a kill, or a power loss, leaves there.
					left := t.TempDir()
					b, err := os.ReadFile(state)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(left, coreClientStateFile), b, 0o600))
					for _, path := range statesSetAside(dir) {
						b, err := os.ReadFile(path)
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(filepath.Join(left, filepath.Base(path)), b, 0o600))
					}
					require.NoError(t, s.stopOpAMPClient())
					dir = left
				} else {
					require.NoError(t, s.stopOpAMPClient())
				}
				ts.record(func() { takes = true })
				ts.forget()
				s, stays := start(dir)
				require.Eventually(t, func() bool { return slices.Contains(ts.seenAuths(), "Bearer issued-2") }, 15*time.Second, 5*time.Millisecond)
				assert.NotContains(t, ts.seenAuths(), "Bearer issued-1")
				assert.Equal(t, again, stays)
				require.NoError(t, s.stopOpAMPClient())
				aside = statesSetAside(dir)
				require.Len(t, aside, 2)
				// The name put back was free again: not by the order.
				first, err := os.ReadFile(aside[0])
				require.NoError(t, err)
				second, err := os.ReadFile(aside[1])
				require.NoError(t, err)
				assert.True(t, bytes.Equal(before, first) || bytes.Equal(before, second))
			})
		}
	}
}

// What the new host's state isn't saved leaves the old one aside.
func TestANewStateThatIsNotSavedLeavesTheOldOneAside(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() <= 0 {
		t.Skip("needs a directory that can't be written")
	}
	ts := newCoreTestServer(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, coreClientStateFile), []byte("old"), 0o600))
	refusedLast(t, dir)
	askAside(t, dir, os.Getpid(), 0)
	s := clientSupervisor(t, config.OpAMPServer{Endpoint: ts.endpoints()["ws"]}, config.Capabilities{}, config.Storage{Directory: dir})
	var err error
	s.persistentState, err = s.clientPersistentState()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o500))
	require.NoError(t, s.startOpAMPClient())
	ts.waitMsg("a message", func(*protobufs.AgentToServer) bool { return true })
	require.NoError(t, s.stopOpAMPClient())
	require.NoError(t, os.Chmod(dir, 0o700))
	require.Len(t, statesSetAside(dir), 1)
	s = clientSupervisor(t, config.OpAMPServer{Endpoint: ts.endpoints()["ws"]}, config.Capabilities{}, config.Storage{Directory: dir})
	_, err = s.clientPersistentState()
	require.NoError(t, err)
	s.closeOpenedClient()
	require.Len(t, statesSetAside(dir), 1)
}
