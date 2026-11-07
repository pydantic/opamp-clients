// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMain puts the default state directory (and every child process's) under the tests' own
// home, never the user's.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "state-home")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"XDG_STATE_HOME", "LOCALAPPDATA", "HOME"} {
		os.Setenv(k, home)
	}
	root, err := os.MkdirTemp("", "state-named")
	if err != nil {
		panic(err)
	}
	checkoutDir, codeDir, envDir = filepath.Join(root, "checkout"), filepath.Join(root, "code"), filepath.Join(root, "env")
	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// Named state directories the tests can make and lock (serverFromEnv does both, as a process
// starting would), outside the default one's home.
var checkoutDir, codeDir, envDir string

// slots is how many processes of one program keep their own identity by default (the core's).
const slots = 16

// released: the process that held the default state directory ends.
func released(s *server) {
	if s != nil && s.stateLock != nil {
		_ = s.stateLock.Close()
	}
}

func stateHome(dir string) map[string]string {
	return map[string]string{"XDG_STATE_HOME": dir, "HOME": dir, "LOCALAPPDATA": dir}
}

func with(vars map[string]string, more map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range vars {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func TestByDefaultAnInstanceKeepsItsUIDAcrossRestarts(t *testing.T) {
	vars := with(stateHome(t.TempDir()), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp",
		"OTEL_SERVICE_NAME": "checkout"})
	start := func(vars map[string]string) *agent {
		t.Helper()
		s := serverFromEnv(env(vars))
		a, err := newAgent(context.Background(), s, NewControls(), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.layer.Close(context.Background()) })
		return a
	}
	first := start(vars)
	if first.server.stateDir == "" || !strings.HasPrefix(filepath.Base(filepath.Dir(first.server.stateDir)), "checkout-") {
		t.Fatalf("a state directory of the service's own: %q", first.server.stateDir)
	}
	if fi, err := os.Stat(first.server.stateDir); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("readable by the user only: %v %v", fi, err)
	}
	// Another process of it, running at once: a slot of its own, so an identity of its own.
	other := start(vars)
	if other.server.stateDir == first.server.stateDir || other.uid == first.uid {
		t.Fatal("two processes at once share an identity")
	}
	released(first.server)
	released(other.server)
	again := start(vars)
	if again.server.stateDir != first.server.stateDir || again.uid != first.uid {
		t.Fatal("a restarted process takes back its slot, and its UID")
	}
	released(again.server)
	// Another service is another instance.
	elsewhere := start(with(vars, map[string]string{"OTEL_SERVICE_NAME": "billing"}))
	if elsewhere.uid == first.uid {
		t.Fatal("another service shares the identity")
	}
	released(elsewhere.server)
}

// A service.instance.id that isn't a UUID can't be the instance UID, so it is replaced by one: kept
// in the default state directory, the same at every start, rather than a new one each time.
func TestAServiceInstanceIDThatIsntAUUIDKeepsTheDefaultStateDirectory(t *testing.T) {
	attrs := "service.instance.id=web-1"
	vars := with(stateHome(t.TempDir()), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s",
		"OTEL_SERVICE_NAME": "web", "OTEL_RESOURCE_ATTRIBUTES": attrs})
	var uids []uuid.UUID
	for range 2 {
		s := serverFromEnv(env(vars))
		if s.stateDir == "" {
			t.Fatal("no default state directory")
		}
		uids = append(uids, identity(s.stateDir, attrs, time.Now()))
		released(s) // as the process exiting would
	}
	if uids[0] != uids[1] {
		t.Fatalf("each start is a new instance: %v", uids)
	}
}

func TestTheDefaultStateDirectoryGivesWay(t *testing.T) {
	dir := t.TempDir()
	base := with(stateHome(dir), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s"})
	// Turned off, or a service.instance.id the application chose (a UUID, so the instance UID): none.
	if s := serverFromEnv(env(with(base, map[string]string{"OTEL_OPAMP_STATE_DIR": "none"}))); s.stateDir != "" {
		t.Fatal("OTEL_OPAMP_STATE_DIR=none: no state directory")
	}
	chosen := "service.instance.id=" + uuid.NewString()
	if s := serverFromEnv(env(with(base, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": chosen}))); s.stateDir != "" {
		t.Fatal("a chosen service.instance.id: no default state directory")
	}
	// Named: that one, locked as the default one is.
	mine := filepath.Join(dir, "mine")
	if s := serverFromEnv(env(with(base, map[string]string{"OTEL_OPAMP_STATE_DIR": mine}))); s.stateDir != mine || s.stateLock == nil {
		t.Fatalf("a named state directory: %+v", s)
	} else {
		released(s)
	}
	// Nowhere to keep it: none, and no error.
	if s := serverFromEnv(env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s"})); s.stateDir != "" {
		t.Fatal("no home: no state directory")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s := serverFromEnv(env(with(stateHome(file), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s"}))); s.stateDir != "" {
		t.Fatal("a home that can't be written: no state directory")
	}
	// Every slot taken: none.
	var taken []*server
	for range slots {
		s := serverFromEnv(env(base))
		if s.stateDir == "" {
			t.Fatal("a free slot not taken")
		}
		taken = append(taken, s)
	}
	if s := serverFromEnv(env(base)); s.stateDir != "" {
		t.Fatal("every slot taken: no state directory")
	}
	for _, s := range taken {
		released(s)
	}
}

// Processes sharing a named state directory (started with the same OTEL_OPAMP_STATE_DIR): the
// first keeps the directory and the UID already in it; the others take numbered folders in it, each
// its own UID, taken back at their next start. They used to join the fleet as one instance.
func TestProcessesSharingANamedStateDirectoryEachKeepTheirOwnInstance(t *testing.T) {
	named := filepath.Join(t.TempDir(), "mine")
	vars := map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_SERVICE_NAME": "checkout",
		"OTEL_OPAMP_STATE_DIR": named}
	start := func() *agent {
		t.Helper()
		a, err := newAgent(context.Background(), serverFromEnv(env(vars)), NewControls(), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.layer.Close(context.Background()) })
		return a
	}
	before := start()
	if before.server.stateDir != named {
		t.Fatalf("the first process keeps the directory: %q", before.server.stateDir)
	}
	released(before.server)
	running := []*agent{start(), start(), start()}
	for i, want := range []string{named, filepath.Join(named, "2"), filepath.Join(named, "3")} {
		if running[i].server.stateDir != want {
			t.Fatalf("process %d: %q, want %q", i, running[i].server.stateDir, want)
		}
	}
	if running[0].uid != before.uid || running[1].uid == running[0].uid || running[2].uid == running[0].uid ||
		running[1].uid == running[2].uid {
		t.Fatal("processes sharing a named directory share an identity, or the first lost its own")
	}
	released(running[1].server)
	again := start()
	if again.server.stateDir != filepath.Join(named, "2") || again.uid != running[1].uid {
		t.Fatal("a restarted process takes back its folder, and its UID")
	}
	released(running[0].server)
	released(running[2].server)
	released(again.server)
	// Every slot taken: a new instance each start, said once at INFO.
	var taken []*server
	for range slots {
		taken = append(taken, serverFromEnv(env(vars)))
	}
	said := records(t)
	if s := serverFromEnv(env(vars)); s.stateDir != "" {
		t.Fatal("every slot taken: no state directory")
	}
	want := "INFO opamp: 16 processes already keep their identity in " + named + ": this one is a new instance each start"
	if len(*said) != 1 || (*said)[0] != want {
		t.Fatalf("said %q, want %q", *said, want)
	}
	for _, s := range taken {
		released(s)
	}
	// One that can't be used: a warning (the default one's only at debug), a new instance each start.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	*said = nil
	if s := serverFromEnv(env(with(vars, map[string]string{"OTEL_OPAMP_STATE_DIR": file}))); s.stateDir != "" {
		t.Fatal("a named directory that can't be used: no state directory")
	}
	if len(*said) != 1 || (*said)[0] != "WARN opamp: the state directory "+file+" can't be used: each start is a new instance" {
		t.Fatalf("said %q", *said)
	}
}

// The default directory is the core's to name (docs/default-state-dir.md): here, that the slot
// this package takes is under it, named for the service.
func TestTheDefaultStateDirectoryIsNamedForTheService(t *testing.T) {
	dir := t.TempDir()
	vars := with(stateHome(dir), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://s",
		"OTEL_RESOURCE_ATTRIBUTES": "service.name=pay%20ments/eu,service.namespace=shop"})
	s := serverFromEnv(env(vars))
	defer released(s)
	rel, _ := filepath.Rel(dir, s.stateDir)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "opamp" || !strings.HasPrefix(parts[1], "pay_ments_eu-") || len(parts[1]) != len("pay_ments_eu-")+8 ||
		parts[2] != "1" {
		t.Fatalf("%s", rel)
	}
	if programName([]string{`/opt/app/checkout.exe`}) != "checkout" {
		t.Fatal("the program's name")
	}
}

func TestAFailedSetupGivesItsSlotBack(t *testing.T) {
	vars := with(stateHome(t.TempDir()), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_SERVICE_NAME": "checkout", "OTEL_EXPORTER_OTLP_PROTOCOL": "carrier-pigeon"})
	for range slots + 1 {
		if _, err := setup(context.Background(), env(vars)); err == nil {
			t.Fatal("an unknown OTLP protocol should fail Setup")
		}
	}
	s := serverFromEnv(env(vars))
	t.Cleanup(func() { release(s.stateLock) })
	if filepath.Base(s.stateDir) != "1" {
		t.Fatalf("failed Setups kept their slots: now %q", s.stateDir)
	}
}

func TestSetupAgainAfterShutdownKeepsTheSlotAndTheUID(t *testing.T) {
	// The exporters are read from the process's environment.
	for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
		t.Setenv(k, "none")
	}
	vars := with(stateHome(t.TempDir()), map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_SERVICE_NAME": "checkout"})
	ctx := context.Background()
	first, err := setup(ctx, env(vars))
	if err != nil {
		t.Fatal(err)
	}
	uid, dir := first.agent.uid, first.agent.server.stateDir
	if filepath.Base(dir) != "1" {
		t.Fatalf("the first Setup's slot: %q", dir)
	}
	_ = first.Shutdown(ctx) // the client never reached a server: its goodbye may fail
	second, err := setup(ctx, env(vars))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(ctx)
	if second.agent.server.stateDir != dir || second.agent.uid != uid {
		t.Fatalf("after Shutdown, Setup again took %q (%s); the first had %q (%s)", second.agent.server.stateDir,
			second.agent.uid, dir, uid)
	}
}
