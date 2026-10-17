// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// fakeTime is a wall clock and timers a test moves by hand.
type fakeTime struct {
	mu     sync.Mutex
	at     time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }

func newFakeTime(ms uint64) *fakeTime { return &fakeTime{at: time.UnixMilli(int64(ms))} }

func (ft *fakeTime) now() time.Time { ft.mu.Lock(); defer ft.mu.Unlock(); return ft.at }

func (ft *fakeTime) after(d time.Duration, f func()) stopper {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{at: ft.at.Add(d), f: f}
	ft.timers = append(ft.timers, t)
	return t
}

// set moves the clock to ms and runs the timers due by then, as time.AfterFunc would.
func (ft *fakeTime) set(ms uint64) {
	ft.mu.Lock()
	ft.at = time.UnixMilli(int64(ms))
	var due []*fakeTimer
	for _, t := range ft.timers {
		if !t.stopped && !t.at.After(ft.at) {
			t.stopped = true
			due = append(due, t)
		}
	}
	ft.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// pending is how many timers are armed and not stopped.
func (ft *fakeTime) pending() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for _, t := range ft.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// fakeReporter keeps what the agent reported from its timer.
type fakeReporter struct {
	mu         sync.Mutex
	effective  []*protobufs.EffectiveConfig
	health     []*protobufs.ComponentHealth
	components []*protobufs.AvailableComponents
}

func (r *fakeReporter) SetEffectiveConfig(e *protobufs.EffectiveConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.effective = append(r.effective, e)
}

func (r *fakeReporter) SetAvailableComponents(c *protobufs.AvailableComponents) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.components = append(r.components, c)
}

func (r *fakeReporter) SetHealth(h *protobufs.ComponentHealth) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = append(r.health, h)
}

// timedAgent is an agent on the fake clock, reporting to a fake client.
func timedAgent(t *testing.T, stateDir string, ft *fakeTime) (*agent, *fakeReporter) {
	t.Helper()
	a, err := newAgentAt(context.Background(), &server{endpoint: "http://127.0.0.1:1", stateDir: stateDir}, NewControls(), "",
		ft.now, ft.after)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.stop(context.Background()) })
	r := &fakeReporter{}
	a.mu.Lock()
	a.reporter = r
	a.mu.Unlock()
	return a, r
}

func sameJSON(t testing.TB, got string, want json.RawMessage) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("%q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

// Debug mode over time, and what the answer comes to with each save, through the agent
// (debug.json, saving.json): its decisions at each step's clock, a restart that loads what the
// client kept applied, and its timer going off on its own, without the server, when the layer said
// to ask again. saving.json's cases whose save fails are the layer's: here the client keeps the
// config applied, and its save failing holds APPLIED back rather than refusing the config (the
// client's own tests).
func TestTheDebugAndSavingVectors(t *testing.T) {
	type step struct {
		Call     string `json:"call"`
		Document string `json:"document"`
		Save     string `json:"save"`
		Now      uint64 `json:"now_unix_ms"`
		Want     struct {
			Status       string          `json:"status"`
			Error        string          `json:"error"`
			SamplingRate json.RawMessage `json:"sampling_rate"`
			RefreshAt    *uint64         `json:"refresh_at_unix_ms"`
			Effective    *string         `json:"effective"`
		} `json:"want"`
	}
	type vectors struct {
		About string
		Cases []struct {
			Name  string `json:"name"`
			Steps []step `json:"steps"`
		}
	}
	var v, saving vectors
	decodeStrict(t, readVectors[json.RawMessage](t, "debug.json"), &v)
	decodeStrict(t, readVectors[json.RawMessage](t, "saving.json"), &saving)
	if len(v.Cases) < 9 || len(saving.Cases) < 8 {
		t.Fatalf("%d and %d cases: debug.json or saving.json was cut short", len(v.Cases), len(saving.Cases))
	}
	for _, c := range append(v.Cases, saving.Cases...) {
		if slices.ContainsFunc(c.Steps, func(s step) bool { return s.Save == "failed" }) {
			continue
		}
		// A next-start change is another SDK's case: this one carries every setting out at once,
		// the instrumentation switches too, so nothing it applies waits for a restart.
		if slices.ContainsFunc(c.Steps, func(s step) bool {
			return strings.Contains(s.Document, `"instrumentations":{"mode":"override"`)
		}) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			dir, ft := t.TempDir(), newFakeTime(0)
			ft.set(c.Steps[0].Now)
			a, rep := timedAgent(t, dir, ft)
			var status *protobufs.RemoteConfigStatus
			for i, s := range c.Steps {
				reported := len(rep.effective)
				switch s.Call {
				case "decide":
					ft.set(s.Now)
					if s.Save == "nowhere" {
						a.server.stateDir = "" // nowhere to keep state: nothing saved
					} else {
						a.server.stateDir = dir
					}
					status = a.remoteConfig(context.Background(), remote(s.Document))
					if status.GetStatus() == applied && s.Save != "nowhere" {
						keepApplied(t, dir, s.Document) // as the client does, before APPLIED goes out
					}
					if !strings.HasPrefix(status.GetErrorMessage(), s.Want.Error) {
						t.Fatalf("step %d: error %q, want %q...", i, status.GetErrorMessage(), s.Want.Error)
					}
				case "load":
					_ = a.stop(context.Background())
					ft = newFakeTime(s.Now)
					a, rep = timedAgent(t, a.server.stateDir, ft)
				case "refresh":
					ft.set(s.Now) // the timer goes off, if it is due
				default:
					t.Fatalf("call %q", s.Call)
				}
				a.mu.Lock()
				rate, refreshAt, effective := a.controls.Sampler.rate, a.refreshAt, effectiveBody(a)
				a.mu.Unlock()
				w := s.Want
				if w.Status != "" {
					want := map[string]protobufs.RemoteConfigStatuses{"applied": applied, "failed": failed_}[w.Status]
					if status.GetStatus() != want {
						t.Fatalf("step %d: status %v, want %s", i, status, w.Status)
					}
				}
				if w.SamplingRate != nil {
					var want *float64
					if err := json.Unmarshal(w.SamplingRate, &want); err != nil {
						t.Fatal(err)
					}
					if (rate == nil) != (want == nil) || rate != nil && *rate != *want {
						t.Fatalf("step %d: the sampler's rate %v, want %s", i, rate, w.SamplingRate)
					}
				}
				if w.RefreshAt != nil {
					if refreshAt != *w.RefreshAt {
						t.Fatalf("step %d: refresh at %d, want %d", i, refreshAt, *w.RefreshAt)
					}
					if timers := ft.pending(); (refreshAt != 0) != (timers == 1) || timers > 1 {
						t.Fatalf("step %d: %d timers armed for a refresh at %d", i, timers, refreshAt)
					}
				}
				if w.Effective != nil && effective != *w.Effective {
					t.Fatalf("step %d: effective %s, want %s", i, effective, *w.Effective)
				}
				if s.Call == "refresh" && refreshAt == 0 && len(rep.effective) == reported && i > 0 &&
					c.Steps[i-1].Call != "refresh" {
					t.Fatalf("step %d: the end of debug mode wasn't reported", i)
				}
			}
		})
	}
}

// debugWindow is a policy with the head sampling ratio and debug mode until the given time.
func debugWindow(ratio, until string) string {
	return every("head_sampling", override(`"ratio":`+ratio), "debug", override(`"until":"`+until+`"`))
}

// A timer armed before a newer decision is nothing when it goes off: that decision armed its own.
func TestAStaleTimerIsNothing(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, rep := timedAgent(t, t.TempDir(), ft)
	a.remoteConfig(context.Background(), remote(debugWindow("0.1", "2026-10-14T12:10:00Z")))
	a.mu.Lock()
	stale := a.gen
	a.mu.Unlock()
	a.remoteConfig(context.Background(), remote(debugWindow("0.1", "2026-10-14T12:20:00Z")))
	ft.mu.Lock()
	ft.at = time.UnixMilli(int64(vectorNow + 10*60_000)) // the first window's end, no timer run
	ft.mu.Unlock()
	a.fired(stale)
	if *a.controls.Sampler.rate != 1 || len(rep.effective) != 0 {
		t.Fatal("the stale timer ended the newer window")
	}
	ft.set(vectorNow + 20*60_000)
	if *a.controls.Sampler.rate != 0.1 || len(rep.effective) != 1 || effectiveBody(a) != sorted(t, every("head_sampling", override(`"ratio":0.1`))) {
		t.Fatalf("the newer window's end: rate %v, %d reports", *a.controls.Sampler.rate, len(rep.effective))
	}
}

// The timer's deadline decides, not the wall clock read when it goes off: a clock moved back
// since doesn't keep debug mode on.
func TestTheEndIsOnTheTimersDeadline(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, _ := timedAgent(t, t.TempDir(), ft)
	a.remoteConfig(context.Background(), remote(debugWindow("0.1", "2026-10-14T12:10:00Z")))
	a.mu.Lock()
	a.refresh(time.UnixMilli(int64(vectorNow + 60_000))) // the wall clock says nine minutes are left
	refreshAt := a.refreshAt
	a.mu.Unlock()
	if *a.controls.Sampler.rate != 0.1 || refreshAt != 0 {
		t.Fatalf("the timer went off: debug mode should be over (rate %v, refresh at %d)", *a.controls.Sampler.rate, refreshAt)
	}
}

// Stopped, the agent's timer is stopped too.
func TestStopStopsTheTimer(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, _ := timedAgent(t, t.TempDir(), ft)
	a.remoteConfig(context.Background(), remote(every("debug", override(`"until":"2026-10-14T12:10:00Z"`))))
	if ft.pending() != 1 {
		t.Fatalf("%d timers", ft.pending())
	}
	_ = a.stop(context.Background())
	if ft.pending() != 0 {
		t.Fatal("the timer outlived the agent")
	}
	a.fired(a.gen) // one already going off does nothing
}

// On the real clock and timer: a window of a second and a half ends on its own, and the
// sampler is back to the rate sent.
func TestADebugWindowEndsOnItsOwn(t *testing.T) {
	a, err := newAgent(context.Background(), &server{endpoint: "http://127.0.0.1:1", stateDir: t.TempDir()}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stop(context.Background())
	until := time.Now().Add(1500 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	if st := a.remoteConfig(context.Background(), remote(debugWindow("0.5", until))); st.GetStatus() != applied {
		t.Fatal(st)
	}
	rate := func() float64 { a.mu.Lock(); defer a.mu.Unlock(); return *a.controls.Sampler.rate }
	if rate() != 1 {
		t.Fatalf("in debug mode: rate %v, want 1", rate())
	}
	for deadline := time.Now().Add(10 * time.Second); rate() != 0.5; {
		if time.Now().After(deadline) {
			t.Fatal("debug mode didn't end")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := func() string { a.mu.Lock(); defer a.mu.Unlock(); return effectiveBody(a) }(); got != sorted(t, every("head_sampling", override(`"ratio":0.5`))) {
		t.Fatalf("effective %s", got)
	}
}

// A FAILED config that comes after the wall clock passed debug_until, before the timer went off
// (a machine that slept: the timer's clock doesn't count it), ends debug mode, as the effective
// config it reports says. Before, it disarmed the end and debug mode stayed on.
func TestAFailedConfigAfterDebugUntilEndsDebugMode(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, rep := timedAgent(t, t.TempDir(), ft)
	a.remoteConfig(context.Background(), remote(debugWindow("0.1", "2026-10-14T12:10:00Z")))
	ft.mu.Lock()
	ft.at = time.UnixMilli(int64(vectorNow + 25*60_000)) // past the end, no timer run
	ft.mu.Unlock()
	st := a.remoteConfig(context.Background(), remote(v1("head_sampling", override(`"ratio":7`))))
	if st.GetStatus() != failed_ {
		t.Fatalf("status %v", st.GetStatus())
	}
	a.mu.Lock()
	rate, refreshAt, eff := *a.controls.Sampler.rate, a.refreshAt, effectiveBody(a)
	a.mu.Unlock()
	if rate != 0.1 || refreshAt != 0 || eff != sorted(t, every("head_sampling", override(`"ratio":0.1`))) || len(rep.effective) != 1 {
		t.Fatalf("debug mode is still in force: rate %v, refresh at %d, effective %s, %d reports", rate, refreshAt, eff,
			len(rep.effective))
	}
}
