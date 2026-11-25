// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor"
)

func writeStatusFile(t *testing.T, dir string, st supervisor.Status) {
	t.Helper()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	// Replaced whole, as the Supervisor does: a status command reading while a test writes from
	// another goroutine must never see half a file.
	tmp := filepath.Join(dir, supervisor.StatusFile+".tmp")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))
	require.NoError(t, os.Rename(tmp, filepath.Join(dir, supervisor.StatusFile)))
}

// `status` reads the status file of the storage directory the config names: 0 when all is
// well, 1 when not, 2 with nothing to read.
func TestStatusCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("server:\n  endpoint: wss://opamp.example.com/v1/opamp\nstorage:\n  directory: "+dir+"\n"), 0o600))
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := statusCommand(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, _, errOut := run("--config", cfg)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "hasn't run on this config yet")

	now := time.Now()
	well := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server:    &supervisor.ServerStatus{Connected: true, Since: now},
		Collector: &supervisor.CollectorStatus{Healthy: true, Since: now}}
	writeStatusFile(t, dir, well)
	code, out, _ := run("--config", cfg)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "connected since")
	assert.Contains(t, out, "healthy since")

	unwell := well
	unwell.Server = &supervisor.ServerStatus{Since: now, Error: "dial tcp: lookup opamp.example.com: no such host"}
	writeStatusFile(t, dir, unwell)
	code, out, _ = run("--storage", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "no such host")
	assert.Contains(t, out, "check server.endpoint")

	code, out, _ = run("--storage", dir, "--json")
	assert.Equal(t, 1, code)
	var st supervisor.Status
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	assert.False(t, st.Server.Connected)

	code, _, errOut = run("--config", filepath.Join(t.TempDir(), "missing.yaml"))
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "--storage DIR")
}

// `status --wait` waits for a Supervisor started since --since to connect: 0 once it is, 1 with
// the status (and what to check) when it isn't by then.
func TestStatusCommandWaitsForTheConnection(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := statusCommand(append([]string{"--storage", dir}, args...), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	now := time.Now()
	since := fmt.Sprint(now.Unix())

	code, _, errOut := run("--wait", "300ms", "--since", since)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "hasn't reported its status")

	untrusted := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server: &supervisor.ServerStatus{Since: now, Error: "tls: failed to verify certificate: x509: certificate signed by unknown authority"}}
	writeStatusFile(t, dir, untrusted)
	code, out, _ := run("--wait", "300ms", "--since", since)
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "--ca-file")

	// A status left by the Supervisor that ran before doesn't count, connected or not.
	old := untrusted
	old.Started = now.Add(-time.Hour)
	old.Server = &supervisor.ServerStatus{Connected: true, Since: old.Started}
	writeStatusFile(t, dir, old)
	code, _, _ = run("--wait", "300ms", "--since", since)
	assert.Equal(t, 1, code)

	go func() {
		time.Sleep(200 * time.Millisecond)
		connected := untrusted
		// Connected for longer than the server's grace to take it: connected is enough.
		connected.Server = &supervisor.ServerStatus{Connected: true, Since: time.Now().Add(-supervisor.AcceptGrace)}
		writeStatusFile(t, dir, connected)
	}()
	code, out, _ = run("--wait", "5s", "--since", since)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "connected since")

	// --json holds with --wait, connected or not.
	code, out, _ = run("--wait", "300ms", "--since", since, "--json")
	assert.Equal(t, 0, code, out)
	var printed supervisor.Status
	require.NoError(t, json.Unmarshal([]byte(out), &printed))
	assert.True(t, printed.Server.Connected)

	// A status that is there but can't be read fails at once: waiting won't fix it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, supervisor.StatusFile), []byte("{"), 0o600))
	started := time.Now()
	code, _, errOut = run("--wait", "5s", "--since", since)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "can't read the status")
	assert.Less(t, time.Since(started), 2*time.Second)
}

// A server that takes the connection but not the credentials reads as connected: with
// --require-accepted `status` exits 1 until the server has taken this Supervisor, and --wait
// waits for that, not for the connection alone.
func TestStatusCommandRequireAccepted(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := statusCommand(append([]string{"--storage", dir}, args...), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	now := time.Now()
	since := fmt.Sprint(now.Unix())
	refused := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server:    &supervisor.ServerStatus{Connected: true, Since: now.Add(-time.Minute)},
		Collector: &supervisor.CollectorStatus{Healthy: true, Since: now}}
	writeStatusFile(t, dir, refused)

	code, out, _ := run()
	assert.Equal(t, 0, code, "unchanged without the flag: a server may send a host nothing")
	assert.Contains(t, out, "Accepted       not yet")
	assert.Contains(t, out, "enrollment key")
	code, _, _ = run("--require-accepted")
	assert.Equal(t, 1, code)
	code, out, _ = run("--require-accepted", "--json")
	assert.Equal(t, 1, code)
	assert.NotContains(t, out, `"accepted"`)

	code, out, _ = run("--wait", "300ms", "--since", since, "--require-accepted")
	assert.Equal(t, 1, code, "connected is not enough")
	assert.Contains(t, out, "not yet")

	go func() {
		time.Sleep(200 * time.Millisecond)
		taken := refused
		taken.Accepted = &supervisor.AcceptedState{Since: time.Now()}
		writeStatusFile(t, dir, taken)
	}()
	code, out, _ = run("--wait", "5s", "--since", since, "--require-accepted")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "the server took this host")
	code, out, _ = run("--require-accepted", "--json")
	assert.Equal(t, 0, code, out)
	var st supervisor.Status
	require.NoError(t, json.Unmarshal([]byte(out), &st))
	require.NotNil(t, st.Accepted)

	// The server refused the credential it had offered, and the host tries again only after the
	// wait is up: the command doesn't wait for it, and says so.
	held := refused
	held.Server = &supervisor.ServerStatus{Since: now, Error: "not connecting for now"}
	held.Enrollment = &supervisor.EnrollmentState{Offers: 2, Since: now, Error: "the server says this host's identity is already bound to another installation: no", IdentityTaken: true, RetryAt: now.Add(time.Hour)}
	writeStatusFile(t, dir, held)
	began := time.Now()
	code, out, _ = run("--wait", "20s", "--since", since, "--require-accepted")
	assert.Equal(t, 1, code)
	assert.Less(t, time.Since(began), 10*time.Second)
	assert.Contains(t, out, "no: the server offered this host 2 credentials of its own, none of them in use")
	assert.Contains(t, out, "already bound to another installation: no")
	assert.Contains(t, out, "Not trying again before")
	code, out, _ = run()
	assert.Equal(t, 1, code)
	assert.NotContains(t, out, "can't reach it")
	// It tries again before the wait is up: worth waiting for.
	soon := held
	soon.Enrollment = &supervisor.EnrollmentState{Offers: 3, Since: now, Error: "lost", RetryAt: now.Add(time.Second)}
	writeStatusFile(t, dir, soon)
	go func() {
		time.Sleep(500 * time.Millisecond)
		taken := refused
		taken.Accepted = &supervisor.AcceptedState{Since: time.Now()}
		writeStatusFile(t, dir, taken)
	}()
	code, out, _ = run("--wait", "20s", "--since", since, "--require-accepted")
	assert.Equal(t, 0, code, out)

	// Accepted doesn't make an unwell Supervisor well.
	unwell := refused
	unwell.Accepted = &supervisor.AcceptedState{Since: now}
	unwell.Server = &supervisor.ServerStatus{Since: now, HTTPStatus: 401}
	writeStatusFile(t, dir, unwell)
	code, out, _ = run("--require-accepted")
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "can't reach it")
}

// --wait --require-accepted returns 0 once the host has been accepted and connected for a few
// seconds: a server may answer a host on credentials it has just ended (it deleted the host, and
// not every part of it has heard), and refuse it a moment later.
func TestStatusCommandRequireAcceptedWaitsForItToLast(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	run := func(wait string) (int, string) {
		var out, errOut bytes.Buffer
		code := statusCommand([]string{"--storage", dir, "--wait", wait, "--since", fmt.Sprint(now.Unix()), "--require-accepted"}, &out, &errOut)
		return code, out.String()
	}
	at := func(accepted, connected time.Time) supervisor.Status {
		return supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
			Server:    &supervisor.ServerStatus{Connected: true, Since: connected},
			Accepted:  &supervisor.AcceptedState{Since: accepted},
			Collector: &supervisor.CollectorStatus{Healthy: true, Since: now}}
	}
	// Accepted, and refused a second later.
	began := time.Now()
	writeStatusFile(t, dir, at(began, began))
	go func() {
		time.Sleep(time.Second)
		refused := at(began, began)
		refused.Server = &supervisor.ServerStatus{Since: time.Now(), HTTPStatus: 401}
		writeStatusFile(t, dir, refused)
	}()
	code, out := run("5s")
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "can't reach it")
	assert.Less(t, time.Since(began), 8*time.Second)

	// It lasts: 0, once it has, though the wait is up before.
	began = time.Now()
	writeStatusFile(t, dir, at(began, began))
	code, out = run("1s")
	assert.Equal(t, 0, code, out)
	assert.GreaterOrEqual(t, time.Since(began), acceptedFor)
	assert.Less(t, time.Since(began), acceptedFor+2*time.Second)

	// Connected again only now, though accepted before: not long enough.
	began = time.Now()
	writeStatusFile(t, dir, at(began.Add(-time.Minute), began))
	code, out = run("10s")
	assert.Equal(t, 0, code, out)
	assert.GreaterOrEqual(t, time.Since(began), acceptedFor)

	// For long already: at once.
	began = time.Now()
	writeStatusFile(t, dir, at(began.Add(-time.Minute), began.Add(-time.Minute)))
	code, out = run("10s")
	assert.Equal(t, 0, code, out)
	assert.Less(t, time.Since(began), time.Second)

	// Dropped and taken again in turn, never for that long, and taken whenever it is read, past the
	// wait and that: not enrolled.
	began = time.Now()
	writeStatusFile(t, dir, at(began, began))
	stop, flapped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(flapped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond):
			}
			// The connection reads as it did; the host is accepted anew, dropped between two reads.
			writeStatusFile(t, dir, at(time.Now(), began))
		}
	}()
	code, out = run("2s")
	close(stop)
	<-flapped
	assert.Equal(t, 1, code, out)
	assert.Less(t, time.Since(began), 2*time.Second+acceptedFor+2*time.Second)

	// A clock that went back doesn't hold it past the wait and that.
	began = time.Now()
	writeStatusFile(t, dir, at(began.Add(time.Hour), began.Add(time.Hour)))
	code, out = run("1s")
	assert.Equal(t, 0, code, out)
	assert.Less(t, time.Since(began), 1*time.Second+acceptedFor+2*time.Second)
}

// Once connected, `status --wait` gives the server its grace to take the Supervisor before it
// prints, so an enrollment helper's output says "Accepted yes" for a host the server takes a
// moment after the connection; it still exits 0 on the connection alone, without
// --require-accepted.
func TestStatusCommandWaitGivesTheServerItsGrace(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	st := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server:    &supervisor.ServerStatus{Connected: true, Since: now},
		Collector: &supervisor.CollectorStatus{Healthy: true, Since: now}}
	writeStatusFile(t, dir, st)
	var out bytes.Buffer
	statusPrinted := make(chan int)
	go func() {
		statusPrinted <- statusCommand([]string{"--storage", dir, "--wait", "20s", "--since", fmt.Sprint(now.Unix())}, &out, &bytes.Buffer{})
	}()
	time.Sleep(500 * time.Millisecond)
	st.Accepted = &supervisor.AcceptedState{Since: time.Now()}
	writeStatusFile(t, dir, st)
	select {
	case code := <-statusPrinted:
		assert.Equal(t, 0, code)
		assert.Contains(t, out.String(), "Accepted       yes")
	case <-time.After(5 * time.Second):
		t.Fatal("--wait didn't return once the server took the Supervisor")
	}

	// Taken by no one: printed once the grace is up (here, with the wait), saying so, and 0.
	st.Accepted = nil
	writeStatusFile(t, dir, st)
	out.Reset()
	code := statusCommand([]string{"--storage", dir, "--wait", "300ms", "--since", fmt.Sprint(now.Unix())}, &out, &bytes.Buffer{})
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "Accepted       not yet")
}

// errReplacing stands for the error Windows gives a read that meets the Supervisor's replacing of
// the status file (a sharing violation).
var errReplacing = errors.New("The process cannot access the file because it is being used by another process.")

// readsFailingFirst makes the status command's first n reads fail as Windows does while the
// Supervisor replaces the file, the reads after that reading it as it is.
func readsFailingFirst(t *testing.T, n int) *atomic.Int32 {
	var calls atomic.Int32
	readStatus = func(dir string) (*supervisor.Status, error) {
		if calls.Add(1) <= int32(n) {
			return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, supervisor.StatusFile), Err: errReplacing}
		}
		return supervisor.ReadStatus(dir)
	}
	isTransientRead = func(err error) time.Duration {
		if errors.Is(err, errReplacing) {
			return forever
		}
		return 0
	}
	t.Cleanup(func() { readStatus, isTransientRead = supervisor.ReadStatus, transientRead })
	return &calls
}

// A read that meets the Supervisor's replacing of the status file (Windows) doesn't end the wait,
// however long the run of such reads: a host that connects is reported connected, exit 0 (one
// install in twelve on a Windows host said it couldn't connect). The plain command tries such a
// read again too. Any other read error still fails at once.
func TestStatusCommandRetriesAReadThatMeetsAReplace(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	st := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server:    &supervisor.ServerStatus{Connected: true, Since: now.Add(-time.Minute)},
		Collector: &supervisor.CollectorStatus{Healthy: true, Since: now},
		Accepted:  &supervisor.AcceptedState{Since: now}}
	writeStatusFile(t, dir, st)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := statusCommand(append([]string{"--storage", dir}, args...), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	calls := readsFailingFirst(t, 60) // 60 × 50 ms: three seconds of failed reads, past any short retry
	code, out, errOut := run("--wait", "30s", "--since", fmt.Sprint(now.Unix()), "--require-accepted")
	assert.Equal(t, 0, code, errOut)
	assert.Contains(t, out, "connected since")
	assert.Greater(t, calls.Load(), int32(60))

	readsFailingFirst(t, 5)
	code, out, errOut = run()
	assert.Equal(t, 0, code, errOut)
	assert.Contains(t, out, "connected since")

	// Failing on to the end of the wait: then it says why, exit 2.
	readsFailingFirst(t, 1<<30)
	code, _, errOut = run("--wait", "300ms", "--since", fmt.Sprint(now.Unix()))
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "being used by another process")

	// Another error isn't retried.
	readStatus, isTransientRead = supervisor.ReadStatus, transientRead
	require.NoError(t, os.WriteFile(filepath.Join(dir, supervisor.StatusFile), []byte("{"), 0o600))
	started := time.Now()
	code, _, _ = run("--wait", "5s", "--since", fmt.Sprint(now.Unix()))
	assert.Equal(t, 2, code)
	assert.Less(t, time.Since(started), time.Second)
}
