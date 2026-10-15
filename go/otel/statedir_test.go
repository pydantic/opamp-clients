// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

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
		uids = append(uids, identity(s.stateDir, attrs))
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
	// Named: that one, unlocked as before.
	mine := filepath.Join(dir, "mine")
	if s := serverFromEnv(env(with(base, map[string]string{"OTEL_OPAMP_STATE_DIR": mine}))); s.stateDir != mine || s.stateLock != nil {
		t.Fatalf("a named state directory: %+v", s)
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

func TestTheDefaultStateDirectoryIsNamedForTheService(t *testing.T) {
	dir := t.TempDir()
	vars := with(stateHome(dir), map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=pay%20ments/eu,service.namespace=shop"})
	got, f := defaultStateDir(env(vars), []string{"/usr/bin/checkout"})
	defer f.Close()
	rel, _ := filepath.Rel(dir, got)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "opamp" || !strings.HasPrefix(parts[1], "pay_ments_eu-") || len(parts[1]) != len("pay_ments_eu-")+8 ||
		parts[2] != "1" {
		t.Fatalf("%s", rel)
	}
	long, g := defaultStateDir(env(with(vars, map[string]string{"OTEL_SERVICE_NAME": strings.Repeat("a", 100)})), nil)
	defer g.Close()
	if name := filepath.Base(filepath.Dir(long)); name != strings.Repeat("a", 64)+name[64:] || len(name) != 64+1+8 {
		t.Fatalf("cut to 64 characters: %s", name)
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
