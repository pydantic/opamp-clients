// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// The application's own settings (docs/app-settings.md): what AppSetting and its kin give the
// application to read, on their own and through the agent and the layer, with the shared vectors
// (testdata/sdk-settings/custom.json).

// told records what a function given to onChange is told.
type told struct {
	mu    sync.Mutex
	calls []change
}

type change struct {
	settings map[string]string
	changed  []string
}

func (t *told) f(settings map[string]string, changed []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, change{settings, changed})
}

func (t *told) all() []change {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.calls)
}

func (t *told) want(tb testing.TB, want ...change) {
	tb.Helper()
	got := t.all()
	if len(got) != len(want) {
		tb.Fatalf("told %d times %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if !sameMap(got[i].settings, want[i].settings) || !slices.Equal(got[i].changed, want[i].changed) {
			tb.Fatalf("change %d: told %v, want %v", i, got[i], want[i])
		}
	}
}

// sameMap says whether two maps hold the same entries (nil and empty alike).
func sameMap(a, b map[string]string) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

type m = map[string]string

// ownAppSettings gives the test the package's application settings of its own, as a fresh start
// has them, and puts the package's back after.
func ownAppSettings(t *testing.T) *appSettings {
	t.Helper()
	before := theAppSettings
	theAppSettings = newAppSettings()
	t.Cleanup(func() { theAppSettings = before })
	return theAppSettings
}

// --- reading -------------------------------------------------------------------------------------

func TestBeforeAnyConfigEveryReadIsItsDefault(t *testing.T) {
	ownAppSettings(t)
	if got := AppSetting("checkout.new_flow", "off"); got != "off" {
		t.Errorf("AppSetting: %q", got)
	}
	if !AppSettingBool("flag", true) || AppSettingBool("flag", false) {
		t.Error("AppSettingBool: not the default")
	}
	if AppSettingInt("limit", 7) != 7 || AppSettingFloat("ratio", 0.5) != 0.5 {
		t.Error("AppSettingInt or AppSettingFloat: not the default")
	}
	if s := AppSettings(); s == nil || len(s) != 0 {
		t.Errorf("AppSettings: %#v, want an empty map", s)
	}
}

func TestWithoutAServerEveryReadIsItsDefault(t *testing.T) {
	app := ownAppSettings(t)
	var tld told
	defer OnAppSettingsChange(tld.f)()
	sdk, err := setup(context.Background(), env(map[string]string{
		"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none"}))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	if AppSetting("a", "d") != "d" || len(AppSettings()) != 0 || len(tld.all()) != 0 || app != theAppSettings {
		t.Fatal("unmanaged: the defaults, and nothing told")
	}
}

func TestAValueInForceIsReadAsSentAndAKeyLeftOutIsItsDefault(t *testing.T) {
	app := newAppSettings()
	app.put(m{"greeting": "  Hello ", "empty": ""})
	for key, want := range map[string]string{"greeting": "  Hello ", "empty": "", "Greeting": "x", "missing": "x", "": "x"} {
		if got := app.get(key, "x"); got != want {
			t.Errorf("get(%q): %q, want %q", key, got, want)
		}
	}
}

// The typed reads, as the shared vectors say: each value, read as a bool, an int and a float,
// gives what the vector says, or the caller's default (null in the vector) for anything else.
func TestTheTypedReadsTakeWhatTheSharedVectorsSay(t *testing.T) {
	var v struct {
		Typed []struct {
			Value string   `json:"value"`
			Bool  *bool    `json:"bool"`
			Int   *int64   `json:"int"`
			Float *float64 `json:"float"`
		} `json:"typed"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sdk-settings", "custom.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Typed) < 30 {
		t.Fatalf("%d typed vectors: the file changed shape?", len(v.Typed))
	}
	for _, tc := range v.Typed {
		t.Run(fmt.Sprintf("%q", tc.Value), func(t *testing.T) {
			app := newAppSettings()
			app.put(m{"k": tc.Value})
			// Two defaults each, so a read that happens to equal one default still shows.
			for _, def := range []bool{true, false} {
				want := def
				if tc.Bool != nil {
					want = *tc.Bool
				}
				if got := app.getBool("k", def); got != want {
					t.Errorf("getBool(def %v): %v, want %v", def, got, want)
				}
			}
			for _, def := range []int64{-424242, math.MaxInt64 - 1} {
				want := def
				if tc.Int != nil {
					want = *tc.Int
				}
				if got := app.getInt("k", def); got != want {
					t.Errorf("getInt(def %d): %d, want %d", def, got, want)
				}
			}
			for _, def := range []float64{-4.25, math.Pi} {
				want := def
				if tc.Float != nil {
					want = *tc.Float
				}
				if got := app.getFloat("k", def); got != want {
					t.Errorf("getFloat(def %v): %v, want %v", def, got, want)
				}
			}
		})
	}
}

// Beyond the vectors: Unicode that folds to ASCII is no boolean, and a float too small to keep
// is zero, as JSON reads it.
func TestTheTypedReadsAtTheirEdges(t *testing.T) {
	app := newAppSettings()
	app.put(m{"long-s": "falſe", "tiny": "1e-400", "neg-zero": "-0", "vt": "\vtrue", "max": "1.7976931348623157e308"})
	if app.getBool("long-s", true) != true || app.getBool("vt", false) != false {
		t.Error("only ASCII case and only space, tab, CR and LF round it")
	}
	if f := app.getFloat("tiny", 1); f != 0 {
		t.Errorf("1e-400: %v, want 0", f)
	}
	if f := app.getFloat("neg-zero", 1); f != 0 || !math.Signbit(f) {
		t.Errorf("-0: %v", f)
	}
	if f := app.getFloat("max", 1); f != math.MaxFloat64 {
		t.Errorf("the largest float64: %v", f)
	}
	if app.getInt("missing", 3) != 3 || app.getFloat("missing", 3) != 3 || app.getBool("missing", true) != true {
		t.Error("a key left out: the default")
	}
}

func TestTheSnapshotIsACopyNoLaterChangeAlters(t *testing.T) {
	app := ownAppSettings(t)
	app.put(m{"a": "1"})
	before := AppSettings()
	before["a"] = "changed by the caller"
	before["z"] = "added by the caller"
	if AppSetting("a", "") != "1" || AppSetting("z", "d") != "d" {
		t.Fatal("changing the snapshot changed what is in force")
	}
	before = AppSettings()
	app.put(m{"a": "2", "b": "3"})
	if !sameMap(before, m{"a": "1"}) || !sameMap(AppSettings(), m{"a": "2", "b": "3"}) {
		t.Fatalf("snapshot %v, now %v", before, AppSettings())
	}
}

func TestPutKeepsACopyOfWhatItIsGiven(t *testing.T) {
	app := newAppSettings()
	given := m{"a": "1"}
	app.put(given)
	given["a"] = "2"
	if app.get("a", "") != "1" {
		t.Fatal("the map in force changed with the caller's")
	}
}

// --- being told ----------------------------------------------------------------------------------

func TestOnChangeIsToldOncePerChangeWithTheMapAndTheKeysThatChanged(t *testing.T) {
	app := newAppSettings()
	var tld told
	app.onChange(tld.f)
	app.put(m{"a": "1", "b": "2"})
	app.put(m{"b": "2", "a": "1"}) // the same again: nothing changed
	app.put(m{"a": "1", "b": "3"})
	app.put(m{"a": "1"}) // b left out: back to its default
	app.put(nil)         // custom left out
	app.put(m{})         // and again: nothing
	app.put(m{"c": "", "a": "1"})
	tld.want(t,
		change{m{"a": "1", "b": "2"}, []string{"a", "b"}},
		change{m{"a": "1", "b": "3"}, []string{"b"}},
		change{m{"a": "1"}, []string{"b"}},
		change{m{}, []string{"a"}},
		change{m{"a": "1", "c": ""}, []string{"a", "c"}}, // an empty value is a value
	)
}

func TestNotToldAtSubscriptionOfTheValuesAlreadyInForce(t *testing.T) {
	app := newAppSettings()
	app.put(m{"a": "1"})
	var tld told
	app.onChange(tld.f)
	app.put(m{"a": "1"})
	tld.want(t)
}

func TestACallbackSeesTheNewValuesThroughAppSettingToo(t *testing.T) {
	app := ownAppSettings(t)
	var seen []string
	OnAppSettingsChange(func(settings map[string]string, changed []string) {
		seen = append(seen, AppSetting("a", "")+"/"+settings["a"]+"/"+AppSettings()["a"])
	})
	app.put(m{"a": "1"})
	if !slices.Equal(seen, []string{"1/1/1"}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestEachCallbackGetsItsOwnCopy(t *testing.T) {
	app := newAppSettings()
	var second told
	app.onChange(func(settings map[string]string, changed []string) {
		settings["a"] = "scribbled"
		changed[0] = "scribbled"
	})
	app.onChange(second.f)
	app.put(m{"a": "1"})
	second.want(t, change{m{"a": "1"}, []string{"a"}})
	if app.get("a", "") != "1" {
		t.Fatal("a callback changed what is in force")
	}
}

func TestCallbacksAreToldInTheOrderTheyWereAddedAndOneAddedTwiceTwice(t *testing.T) {
	app := newAppSettings()
	var order []string
	first := func(map[string]string, []string) { order = append(order, "first") }
	app.onChange(first)
	app.onChange(func(map[string]string, []string) { order = append(order, "second") })
	app.onChange(first)
	app.put(m{"a": "1"})
	if !slices.Equal(order, []string{"first", "second", "first"}) {
		t.Fatalf("order %v", order)
	}
}

func TestACallbackRemovedIsToldNothingMoreAndRemovingItAgainDoesNothing(t *testing.T) {
	app := newAppSettings()
	var tld, other told
	remove := app.onChange(tld.f)
	app.onChange(other.f)
	app.put(m{"a": "1"})
	remove()
	remove()
	app.put(m{"a": "2"})
	if len(tld.all()) != 1 || len(other.all()) != 2 {
		t.Fatalf("told %d and %d times", len(tld.all()), len(other.all()))
	}
}

func TestRemovingOneOfACallbackAddedTwiceLeavesTheOther(t *testing.T) {
	app := newAppSettings()
	var tld told
	remove := app.onChange(tld.f)
	app.onChange(tld.f)
	remove()
	app.put(m{"a": "1"})
	if len(tld.all()) != 1 {
		t.Fatalf("told %d times", len(tld.all()))
	}
}

func TestANilCallbackIsNothing(t *testing.T) {
	app := ownAppSettings(t)
	remove := OnAppSettingsChange(nil)
	app.put(m{"a": "1"})
	remove()
}

func TestACallbackMayRemoveItselfOrAddAnotherWhileItIsTold(t *testing.T) {
	app := newAppSettings()
	var tld, later told
	var removeOnce func()
	removeOnce = app.onChange(func(map[string]string, []string) {
		removeOnce()
		app.onChange(tld.f)
	})
	app.onChange(later.f) // added before, removed by nobody: told of both
	app.put(m{"a": "1"})  // once runs and goes; tld is added, and hears from the next change on
	app.put(m{"a": "2"})
	tld.want(t, change{m{"a": "2"}, []string{"a"}})
	if len(later.all()) != 2 {
		t.Fatal("a callback removed while told took the next one with it")
	}
}

func TestACallbackThatPanicsIsLoggedAndTheRestAreStillTold(t *testing.T) {
	said := records(t)
	app := newAppSettings()
	var tld told
	app.onChange(func(map[string]string, []string) { panic("the application's bug") })
	app.onChange(tld.f)
	app.put(m{"a": "1"})
	app.put(m{"a": "2"})
	tld.want(t, change{m{"a": "1"}, []string{"a"}}, change{m{"a": "2"}, []string{"a"}})
	if app.get("a", "") != "2" {
		t.Fatal("in force all the same")
	}
	var errs []string
	for _, s := range *said {
		if strings.HasPrefix(s, "ERROR") {
			errs = append(errs, s)
		}
	}
	if len(errs) != 2 || !strings.Contains(errs[0], "panicked") {
		t.Fatalf("logged %v", *said)
	}
}

func TestAChangeIsLoggedAtInfoWithTheKeys(t *testing.T) {
	said := records(t)
	app := newAppSettings()
	app.put(m{"b": "1", "a": "1"})
	app.put(m{"b": "1", "a": "1"})
	if !slices.Equal(*said, []string{"INFO opamp: application settings changed: a, b"}) {
		t.Fatalf("logged %v", *said)
	}
}

// --- goroutines (run with -race) -----------------------------------------------------------------

func TestChangesAreToldOneAtATimeInOrderWhicheverGoroutinePutsThem(t *testing.T) {
	app := newAppSettings()
	var busy, overlaps atomic.Int32
	var last string
	app.onChange(func(settings map[string]string, changed []string) {
		if busy.Add(1) != 1 {
			overlaps.Add(1)
		}
		last = settings["n"]
		busy.Add(-1)
	})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				app.put(m{"n": fmt.Sprintf("%d-%d", i, j)})
			}
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d changes told while another was", overlaps.Load())
	}
	if last != app.get("n", "") {
		t.Fatalf("the last told %q is not what is in force %q", last, app.get("n", ""))
	}
}

func TestReadersOnOtherGoroutinesAlwaysSeeOneWholeMap(t *testing.T) {
	app := ownAppSettings(t)
	stop := make(chan struct{})
	var torn atomic.Int32
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := AppSettings()
				seen := map[string]bool{}
				for _, v := range s {
					seen[v] = true
				}
				if len(seen) > 1 {
					torn.Add(1)
				}
				_ = AppSetting("a", "")
				_ = AppSettingInt("b", 0)
				_ = AppSettingFloat("c", 0)
				_ = AppSettingBool("d", false)
			}
		}()
	}
	for n := range 2000 {
		v := fmt.Sprint(n)
		app.put(m{"a": v, "b": v, "c": v, "d": v, "e": v, "f": v})
	}
	close(stop)
	wg.Wait()
	if torn.Load() != 0 {
		t.Fatalf("%d snapshots part of one map and part of the next", torn.Load())
	}
}

func TestCallbacksAddedAndRemovedFromOtherGoroutinesWhileChangesAreTold(t *testing.T) {
	app := ownAppSettings(t)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 300 {
				OnAppSettingsChange(func(map[string]string, []string) {})()
			}
		}()
	}
	var tld told
	OnAppSettingsChange(tld.f)
	for n := range 300 {
		app.put(m{"n": fmt.Sprint(n)})
	}
	wg.Wait()
	if len(tld.all()) != 300 {
		t.Fatalf("a callback that stayed was told %d of 300 changes", len(tld.all()))
	}
}

// --- through the agent and the layer -------------------------------------------------------------

// Each shared case through the layer, as this package's host declares the settings (or the case's
// own host), then Controls: the map the application reads is the one the case says, and a refused
// case is FAILED with its reason.
func TestTheSharedCustomVectorsReachTheApplicationAsTheLayerDecides(t *testing.T) {
	var v struct {
		Cases []struct {
			Name      string            `json:"name"`
			Document  string            `json:"document"`
			Host      *settingspb.Host  `json:"host"`
			Refused   string            `json:"refused"`
			Custom    map[string]string `json:"custom"`
			Effective map[string]string `json:"effective"`
		} `json:"cases"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sdk-settings", "custom.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 25 {
		t.Fatalf("%d cases: the file changed shape?", len(v.Cases))
	}
	layer := testLayer(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			host := goHost()
			if c.Host != nil {
				host = c.Host
			}
			d, err := layer.DecideAt(context.Background(), remote(c.Document), []byte("{}"), []byte("{}"), host,
				vectorNow, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.Refused != "" {
				if d.GetStatus().GetStatus() != failed_ || d.GetStatus().GetErrorMessage() != c.Refused {
					t.Fatalf("status %v, want FAILED %q", d.GetStatus(), c.Refused)
				}
				if d.GetApply() != nil {
					t.Fatal("a refused config hands nothing over")
				}
				return
			}
			if d.GetStatus().GetStatus() != applied {
				t.Fatalf("status %v", d.GetStatus())
			}
			controls := NewControls()
			var tld told
			controls.app.onChange(tld.f)
			controls.Apply(d.GetApply())
			if got := controls.app.snapshot(); !sameMap(got, c.Custom) {
				t.Fatalf("in force %v, want %v", got, c.Custom)
			}
			if len(c.Custom) == 0 {
				tld.want(t)
			} else {
				keys := make([]string, 0, len(c.Custom))
				for k := range c.Custom {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				tld.want(t, change{c.Custom, keys})
			}
			var effective map[string]string
			if err := json.Unmarshal(d.GetEffective().GetConfigMap().GetConfigMap()["elastic"].GetBody(), &effective); err != nil {
				t.Fatal(err)
			}
			if c.Effective != nil && !sameMap(effective, c.Effective) {
				t.Fatalf("effective %v, want %v", effective, c.Effective)
			}
		})
	}
}

func TestThePackageDeclaresCustom(t *testing.T) {
	if !slices.Contains(goHost().GetLive(), "custom") {
		t.Fatalf("live %v: custom undeclared is ignored, unchecked", goHost().GetLive())
	}
}

func TestAConfigPutsTheValuesInForceAndTheEffectiveConfigReportsThem(t *testing.T) {
	a := newTestAgent(t, t.TempDir())
	var tld told
	a.controls.app.onChange(func(settings map[string]string, changed []string) {
		// Last: the SDK's settings beside them already in force.
		if r := a.controls.Sampler.rate; r == nil || *r != 0.5 {
			t.Error("told before the SDK's settings were in force")
		}
		tld.f(settings, changed)
	})
	st := a.remoteConfig(context.Background(), remote(`{"sampling_rate":"0.5","custom":{"checkout.new_flow":"on","limit":25}}`))
	if st.GetStatus() != applied {
		t.Fatalf("status %v", st)
	}
	if a.controls.app.get("checkout.new_flow", "off") != "on" || a.controls.app.getInt("limit", 0) != 25 {
		t.Fatal("not in force")
	}
	if got, want := effectiveBody(a), `{"custom":"{\"checkout.new_flow\":\"on\",\"limit\":\"25\"}","sampling_rate":"0.5"}`; !sameJSON(t, got, json.RawMessage(want)) {
		t.Fatalf("effective %s, want %s", got, want)
	}
	tld.want(t, change{m{"checkout.new_flow": "on", "limit": "25"}, []string{"checkout.new_flow", "limit"}})
}

func TestTheSameValuesSentAgainOrAnotherWayTellNothing(t *testing.T) {
	a := newTestAgent(t, t.TempDir())
	var tld told
	a.controls.app.onChange(tld.f)
	a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1","b":"2"}}`))
	// New configs (other hashes) whose map is the same: in a string, keys in another order, a
	// number for its text, another SDK setting beside it.
	for _, body := range []string{`{"custom":"{\"b\":\"2\",\"a\":1}"}`, `{"custom":{"a":"1","b":"2"},"sampling_rate":"1"}`} {
		if st := a.remoteConfig(context.Background(), remote(body)); st.GetStatus() != applied {
			t.Fatalf("%s: %v", body, st)
		}
	}
	tld.want(t, change{m{"a": "1", "b": "2"}, []string{"a", "b"}})
}

func TestAWrongCustomEntryFailsTheConfigAndNothingChanges(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir)
	var tld told
	a.controls.app.onChange(tld.f)
	a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1"},"sampling_rate":"0.5"}`))
	for _, tc := range []struct{ body, why string }{
		{`{"custom":{"a":"2","b":["x"]},"sampling_rate":"1"}`, "custom.b: values must be"},
		{`{"custom":{"a b":"2"}}`, `custom: "a b" is not a key`},
		{`{"custom":{"a":"2","a":"3"}}`, "custom.a: sent more than once"},
		{`{"custom":{"a":"` + strings.Repeat("x", 4097) + `"}}`, "custom.a: 4097 bytes, at most 4096"},
		{`{"custom":7}`, "custom: must be a JSON object"},
	} {
		st := a.remoteConfig(context.Background(), remote(tc.body))
		if st.GetStatus() != failed_ || !strings.HasPrefix(st.GetErrorMessage(), tc.why) {
			t.Fatalf("%.60s: %v, want FAILED %q", tc.body, st, tc.why)
		}
	}
	if a.controls.app.get("a", "") != "1" || *a.controls.Sampler.rate != 0.5 {
		t.Fatal("a FAILED config changed what is in force")
	}
	if got := effectiveBody(a); !sameJSON(t, got, json.RawMessage(`{"custom":"{\"a\":\"1\"}","sampling_rate":"0.5"}`)) {
		t.Fatalf("effective %s", got)
	}
	tld.want(t, change{m{"a": "1"}, []string{"a"}})
	if _, err := os.Stat(filepath.Join(dir, "sdk-settings.json")); err == nil {
		t.Fatal("sdk-settings.json written: the client keeps the config applied")
	}
}

func TestAConfigWithoutCustomPutsEveryKeyBackToItsDefault(t *testing.T) {
	a := newTestAgent(t, t.TempDir())
	var tld told
	a.controls.app.onChange(tld.f)
	a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1"}}`))
	a.remoteConfig(context.Background(), remote(`{"sampling_rate":"1"}`))
	if a.controls.app.get("a", "default") != "default" {
		t.Fatal("custom left out: the default")
	}
	a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1"}}`))
	// A remote config with no settings entry at all.
	a.remoteConfig(context.Background(), &protobufs.AgentRemoteConfig{ConfigHash: []byte("none"), Config: &protobufs.AgentConfigMap{}})
	if a.controls.app.get("a", "default") != "default" {
		t.Fatal("no elastic entry: the default")
	}
	tld.want(t,
		change{m{"a": "1"}, []string{"a"}}, change{m{}, []string{"a"}},
		change{m{"a": "1"}, []string{"a"}}, change{m{}, []string{"a"}})
}

func TestACallbackThatPanicsDoesNotBreakTheAnswer(t *testing.T) {
	records(t)
	a := newTestAgent(t, t.TempDir())
	a.controls.app.onChange(func(map[string]string, []string) { panic(fmt.Errorf("nope")) })
	st := a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1"}}`))
	if st.GetStatus() != applied || a.controls.app.get("a", "") != "1" {
		t.Fatalf("status %v", st)
	}
	if got := effectiveBody(a); got != `{"custom":"{\"a\":\"1\"}"}` {
		t.Fatalf("effective %s", got)
	}
}

// The layer's Refresh (debug mode ends, on the timer) hands the settings over again: the same map
// is put in force again, which is no change, and the application still reads it.
func TestARefreshWithTheSameMapTellsNothing(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, rep := timedAgent(t, t.TempDir(), ft)
	var tld told
	a.controls.app.onChange(tld.f)
	st := a.remoteConfig(context.Background(),
		remote(`{"debug_until":"2026-10-14T12:10:00Z","sampling_rate":"0.1","custom":{"a":"1"}}`))
	if st.GetStatus() != applied {
		t.Fatalf("status %v", st)
	}
	ft.set(vectorNow + 300_000) // a refresh while debug mode lasts
	ft.set(vectorNow + 600_000) // and its end
	a.mu.Lock()
	rate, refreshAt := *a.controls.Sampler.rate, a.refreshAt
	a.mu.Unlock()
	if rate != 0.1 || refreshAt != 0 {
		t.Fatalf("debug mode didn't end on the timer: rate %v, refresh at %d", rate, refreshAt)
	}
	rep.mu.Lock()
	reported := len(rep.effective)
	rep.mu.Unlock()
	if reported == 0 {
		t.Fatal("the refresh reported nothing")
	}
	tld.want(t, change{m{"a": "1"}, []string{"a"}})
	if a.controls.app.get("a", "") != "1" {
		t.Fatal("the refresh put the application's settings back to the defaults")
	}
}

func TestValuesSurviveARestartWithAStateDirectory(t *testing.T) {
	dir := t.TempDir()
	if st := newTestAgent(t, dir).remoteConfig(context.Background(), remote(`{"custom":{"checkout.new_flow":"on"}}`)); st.GetStatus() != applied {
		t.Fatalf("%v", st)
	}
	keepApplied(t, dir, `{"custom":{"checkout.new_flow":"on"}}`) // as the client does, before APPLIED goes out
	again := newTestAgent(t, dir)                                // the next start: before any config arrives
	if again.controls.app.get("checkout.new_flow", "off") != "on" {
		t.Fatal("the saved values aren't in force at the next start")
	}
	if got := effectiveBody(again); got != `{"custom":"{\"checkout.new_flow\":\"on\"}"}` {
		t.Fatalf("effective %s", got)
	}
}

func TestWithoutAStateDirectoryARestartStartsFromTheDefaults(t *testing.T) {
	a := newTestAgent(t, "")
	if st := a.remoteConfig(context.Background(), remote(`{"custom":{"a":"1"}}`)); st.GetStatus() != applied {
		t.Fatalf("live: no state needed, %v", st)
	}
	if a.controls.app.get("a", "") != "1" {
		t.Fatal("not in force")
	}
	if newTestAgent(t, "").controls.app.get("a", "default") != "default" {
		t.Fatal("nothing saved: the defaults")
	}
}

// Through Setup, as an application uses it: the package's functions read the values saved before
// a restart from the start, and a function added before Setup is told of them.
func TestSetupPutsTheSavedValuesInForceBeforeItReturns(t *testing.T) {
	ownAppSettings(t)
	dir := t.TempDir()
	if st := newTestAgent(t, dir).remoteConfig(context.Background(), remote(`{"custom":{"example.feature":"on","limit":"9"}}`)); st.GetStatus() != applied {
		t.Fatalf("%v", st)
	}
	keepApplied(t, dir, `{"custom":{"example.feature":"on","limit":"9"}}`) // as the client does, before APPLIED goes out
	var tld told
	defer OnAppSettingsChange(tld.f)()
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Cleanup(func() { os.Unsetenv("OTEL_TRACES_EXPORTER"); os.Unsetenv("OTEL_LOGS_EXPORTER") })
	sdk, err := setup(context.Background(), env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_OPAMP_STATE_DIR": dir, "OTEL_METRICS_EXPORTER": "none"}))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	if sdk.Controls.app != theAppSettings {
		t.Fatal("Setup's controls don't put in force what AppSetting reads")
	}
	if AppSetting("example.feature", "off") != "on" || AppSettingInt("limit", 0) != 9 {
		t.Fatalf("in force %v", AppSettings())
	}
	tld.want(t, change{m{"example.feature": "on", "limit": "9"}, []string{"example.feature", "limit"}})
	// The server's config, as the client hands it over.
	sdk.agent.remoteConfig(context.Background(), remote(`{"custom":{"example.feature":"off"}}`))
	if AppSetting("example.feature", "") != "off" || AppSettingInt("limit", 0) != 0 {
		t.Fatalf("in force %v", AppSettings())
	}
}

// A Setup that fails after it has put the saved values in force takes them back: the application
// reads what it read before, and a function told of the saved values is told they are gone.
func TestAFailedSetupTakesTheSavedValuesBack(t *testing.T) {
	ownAppSettings(t)
	dir := t.TempDir()
	if st := newTestAgent(t, dir).remoteConfig(context.Background(), remote(`{"custom":{"example.feature":"on"}}`)); st.GetStatus() != applied {
		t.Fatalf("%v", st)
	}
	keepApplied(t, dir, `{"custom":{"example.feature":"on"}}`) // as the client does, before APPLIED goes out
	var tld told
	defer OnAppSettingsChange(tld.f)()
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Cleanup(func() { os.Unsetenv("OTEL_TRACES_EXPORTER"); os.Unsetenv("OTEL_LOGS_EXPORTER") })
	// The saved values are loaded, then the metrics fail.
	if _, err := setup(context.Background(), env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_OPAMP_STATE_DIR": dir, "OTEL_EXPORTER_OTLP_PROTOCOL": "carrier-pigeon"})); err == nil {
		t.Fatal("an unknown OTLP protocol should fail Setup")
	}
	if got := AppSettings(); len(got) != 0 {
		t.Fatalf("a failed Setup left %v in force", got)
	}
	tld.want(t,
		change{m{"example.feature": "on"}, []string{"example.feature"}},
		change{m{}, []string{"example.feature"}})
}

// Apply puts a document's SDK settings and its custom values in force together: a function told
// of the custom values sees that document's SDK settings, whatever another Apply is doing.
func TestApplyPutsEachDocumentInForceWhole(t *testing.T) {
	c := NewControls()
	off, on := false, true
	docs := []*settingspb.Settings{
		{SendTraces: &off, Custom: m{"traces": "off"}},
		{SendTraces: &on, Custom: m{"traces": "on"}},
	}
	var mixed atomic.Int64
	defer c.app.onChange(func(now map[string]string, _ []string) {
		if (now["traces"] == "on") != c.gates.Load().traces {
			mixed.Add(1)
		}
	})()
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				c.Apply(docs[(g+i)%2])
			}
		}()
	}
	wg.Wait()
	if n := mixed.Load(); n != 0 {
		t.Fatalf("%d callbacks saw one document's custom values with another's SDK settings", n)
	}
}

func TestNewControlsKeepsItsOwnApplicationSettings(t *testing.T) {
	ownAppSettings(t)
	NewControls().Apply(&settingspb.Settings{Custom: m{"a": "1"}})
	if AppSetting("a", "d") != "d" {
		t.Fatal("a Controls of the application's own changed the package's application settings")
	}
}
