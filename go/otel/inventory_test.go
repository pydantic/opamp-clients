// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	otelsdk "go.opentelemetry.io/otel/sdk"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// A module the test binary is built with, for a version from build info.
const (
	autoexportScope   = "go.opentelemetry.io/contrib/exporters/autoexport"
	autoexportVersion = "v0.71.0"
)

// listed is a component's sub-components, each by its metadata (key to value).
func listed(d *protobufs.ComponentDetails) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, sub := range d.GetSubComponentMap() {
		md := map[string]any{}
		for _, kv := range sub.GetMetadata() {
			switch v := kv.GetValue().GetValue().(type) {
			case *protobufs.AnyValue_StringValue:
				md[kv.GetKey()] = v.StringValue
			case *protobufs.AnyValue_BoolValue:
				md[kv.GetKey()] = v.BoolValue
			}
		}
		out[name] = md
	}
	return out
}

// exportersOf is the exporters component, signal to name to gated.
func exportersOf(c *protobufs.AvailableComponents) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for signal, d := range c.GetComponents()["exporters"].GetSubComponentMap() {
		out[signal] = map[string]bool{}
		for name, e := range listed(d) {
			out[signal][name] = e["gated"].(bool)
		}
	}
	return out
}

// componentsServer is an OpAMP server over HTTP that asks for the components in full in every
// answer, and keeps the full ones it is sent.
func componentsServer(t *testing.T) (*httptest.Server, func() *protobufs.AvailableComponents) {
	t.Helper()
	var mu sync.Mutex
	var last *protobufs.AvailableComponents
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m protobufs.AgentToServer
		if proto.Unmarshal(body, &m) != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		if c := m.GetAvailableComponents(); c != nil && len(c.GetComponents()) > 0 {
			if m.GetCapabilities()&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents) == 0 {
				t.Errorf("components sent without ReportsAvailableComponents")
			}
			mu.Lock()
			last = c
			mu.Unlock()
		}
		out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid(),
			Flags: uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, func() *protobufs.AvailableComponents {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// eventually waits for ok, failing with what after 10 s.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// A typical setup: OTLP traces, a Prometheus reader the server can't stop, console logs, and an
// instrumentation of each kind taken from the global providers once Setup has returned. The
// server gets them all, with a hash, and each name is one deactivate_instrumentations takes.
func TestTheInventoryOfATypicalSetupReachesTheServer(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp") // autoexport reads the process environment
	t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", "0")
	t.Setenv("OTEL_LOGS_EXPORTER", "console")
	withBuildInfo(t, application)
	srv, last := componentsServer(t)
	sdk, err := setup(context.Background(), os.Getenv, WithEndpoint(srv.URL+"/v1/opamp"), WithStateDir(t.TempDir()),
		WithHeartbeatInterval(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())

	otel.Tracer(otelhttp, trace.WithInstrumentationVersion("0.71.0"))
	otel.Meter(autoexportScope) // no version given: the module's, from build info
	global.GetLoggerProvider().Logger("checkout.db")
	// The SDK's own providers are the same ones: a scope taken from them is listed too.
	sdk.TracerProvider.Tracer("orders.http")
	sdk.MeterProvider.Meter("orders.meter")
	sdk.LoggerProvider.Logger("orders.db")

	want := map[string]map[string]any{"otelhttp": {"version": "0.71.0"}, "autoexport": {"version": autoexportVersion},
		"checkout.db": {}, "orders.http": {}, "orders.meter": {}, "orders.db": {}}
	var c *protobufs.AvailableComponents
	eventually(t, "the instrumentations taken after Setup never reached the server", func() bool {
		c = last()
		return c != nil && reflect.DeepEqual(listed(c.GetComponents()["instrumentations"]), want)
	})
	if len(c.GetHash()) == 0 {
		t.Fatal("no hash")
	}
	sdkMeta := map[string]any{}
	for _, kv := range c.GetComponents()["sdk"].GetMetadata() {
		sdkMeta[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if sdkMeta["integration.name"] != module || sdkMeta["integration.version"] != "v0.5.0" || sdkMeta["telemetry.sdk.language"] != "go" ||
		sdkMeta["telemetry.sdk.version"] != otelsdk.Version() || otelsdk.Version() == "" {
		t.Fatalf("sdk: %v", sdkMeta)
	}
	if got, want := exportersOf(c), map[string]map[string]bool{"traces": {"otlp": true}, "metrics": {"prometheus": false},
		"logs": {"console": true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exporters %v, want %v", got, want)
	}
	// The settings: those of the host Load and Decide are given, without send_metrics.
	settings := listed(c.GetComponents()["settings"])
	if _, ok := settings["send_metrics"]; ok || settings["send_traces"]["applies"] != "at once" ||
		settings["deactivate_instrumentations"]["applies"] != "at once" || settings["debug_until"]["applies"] != "at once" {
		t.Fatalf("settings %v", settings)
	}
	ref, err := testLayer(t).Components(context.Background(), &settingspb.Inventory{}, sdk.agent.server.host(), vectorNow)
	if err != nil || !proto.Equal(ref.GetComponents()["settings"], c.GetComponents()["settings"]) {
		t.Fatalf("settings are not the host's: %v %v", ref.GetComponents()["settings"], err)
	}

	// Each name listed switches its instrumentation off.
	var off []string
	for name := range want {
		off = append(off, name)
	}
	if st := sdk.agent.remoteConfig(context.Background(),
		remote(every("instrumentations", override(`"disabled":["`+strings.Join(off, `","`)+`"]`)))); st.GetStatus() != applied {
		t.Fatalf("status %v", st)
	}
	for _, scope := range []string{otelhttp, autoexportScope, "checkout.db"} {
		if !sdk.Controls.Off(scope) {
			t.Errorf("%s: listed, but its name doesn't switch it off", scope)
		}
	}
}

// startedAgent is an agent whose client is a fake, started as Setup starts it with the exporters
// given.
func startedAgent(t *testing.T, srv *server, exporters ...*settingspb.Exporter) (*agent, *fakeReporter) {
	t.Helper()
	ft := newFakeTime(vectorNow)
	a, err := newAgentAt(context.Background(), srv, NewControls(), "", ft.now, ft.after)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.stop(context.Background()) })
	r := &fakeReporter{}
	a.mu.Lock()
	a.reporter = r
	a.exporters = exporters
	a.mu.Unlock()
	return a, r
}

func (r *fakeReporter) lastComponents() (*protobufs.AvailableComponents, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.components) == 0 {
		return nil, 0
	}
	return r.components[len(r.components)-1], len(r.components)
}

// A new scope is reported, once; the same scope again isn't; a new version of it is, the versions
// joined; two scopes that end in the same name are one.
func TestANewInstrumentationIsReportedOnce(t *testing.T) {
	a, r := startedAgent(t, &server{endpoint: "http://127.0.0.1:1"})
	tp := a.controls.TracerProvider(otel.GetTracerProvider())
	instrumentations := func(n int) map[string]map[string]any {
		t.Helper()
		var c *protobufs.AvailableComponents
		eventually(t, "not reported", func() bool { _, got := r.lastComponents(); return got >= n })
		c, got := r.lastComponents()
		if got != n {
			t.Fatalf("reported %d times, want %d", got, n)
		}
		return listed(c.GetComponents()["instrumentations"])
	}
	tp.Tracer(otelhttp, trace.WithInstrumentationVersion("0.70.0"))
	if got := instrumentations(1); !reflect.DeepEqual(got, map[string]map[string]any{"otelhttp": {"version": "0.70.0"}}) {
		t.Fatalf("instrumentations %v", got)
	}
	tp.Tracer(otelhttp, trace.WithInstrumentationVersion("0.70.0"))
	a.controls.MeterProvider(otel.GetMeterProvider()).Meter(otelhttp, metric.WithInstrumentationVersion("0.70.0"))
	time.Sleep(50 * time.Millisecond)
	if _, got := r.lastComponents(); got != 1 {
		t.Fatalf("the same scope again: reported %d times", got)
	}
	tp.Tracer("example.com/vendored/otelhttp", trace.WithInstrumentationVersion("0.71.0"))
	if got := instrumentations(2); !reflect.DeepEqual(got, map[string]map[string]any{"otelhttp": {"version": "0.70.0,0.71.0"}}) {
		t.Fatalf("instrumentations %v", got)
	}
}

// No instrumentation, no exporter (each set to none): the four components are there, two empty.
func TestAnInventoryOfNothing(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	sdk, err := setup(context.Background(), env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_METRICS_EXPORTER": "none"}), WithStateDir("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	if len(sdk.exporters) != 0 {
		t.Fatalf("exporters %v", sdk.exporters)
	}
	sdk.agent.mu.Lock()
	c := sdk.agent.components(context.Background())
	sdk.agent.mu.Unlock()
	if len(c.GetComponents()) != 4 || len(listed(c.GetComponents()["instrumentations"])) != 0 ||
		len(c.GetComponents()["exporters"].GetSubComponentMap()) != 0 || len(listed(c.GetComponents()["settings"])) == 0 {
		t.Fatalf("components %v", c)
	}
	// Without a state directory every setting is still carried out at once, as the layer says.
	if listed(c.GetComponents()["settings"])["deactivate_instrumentations"]["applies"] != "at once" {
		t.Fatalf("settings %v", listed(c.GetComponents()["settings"]))
	}
}

// The metrics exporter is gated exactly when send_metrics is carried out: the inventory and the
// host agree, for each exporter Setup builds.
func TestTheMetricsExporterIsGatedWhenSendMetricsIsCarriedOut(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", "0")
	for _, tc := range []struct {
		env  map[string]string
		want map[string]map[string]bool
	}{
		{map[string]string{}, map[string]map[string]bool{"metrics": {"otlp": true}}},
		{map[string]string{"OTEL_METRICS_EXPORTER": "console"}, map[string]map[string]bool{"metrics": {"console": true}}},
		{map[string]string{"OTEL_METRICS_EXPORTER": "none"}, map[string]map[string]bool{}},
		{map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}, map[string]map[string]bool{"metrics": {"prometheus": false}}},
		{map[string]string{"OTEL_METRICS_PRODUCERS": "prometheus"}, map[string]map[string]bool{"metrics": {"otlp": false}}},
	} {
		for k, v := range tc.env { // autoexport reads the process environment too
			t.Setenv(k, v)
		}
		tc.env["OTEL_OPAMP_ENDPOINT"] = "http://127.0.0.1:1/v1/opamp"
		sdk, err := setup(context.Background(), env(tc.env), WithStateDir("none"))
		if err != nil {
			t.Fatal(err)
		}
		sdk.agent.mu.Lock()
		c := sdk.agent.components(context.Background())
		sdk.agent.mu.Unlock()
		_ = sdk.Shutdown(context.Background())
		os.Unsetenv("OTEL_METRICS_EXPORTER")
		os.Unsetenv("OTEL_METRICS_PRODUCERS")
		if got := exportersOf(c); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: exporters %v, want %v", tc.env, got, tc.want)
		}
		_, carried := listed(c.GetComponents()["settings"])["send_metrics"]
		if gated := tc.want["metrics"][metricsExporter(env(tc.env))]; carried != (gated || len(tc.want) == 0) {
			t.Errorf("%v: send_metrics carried out %v, the exporter gated %v", tc.env, carried, gated)
		}
	}
}

// Setup builds one exporter a signal; were two listed under one name, they would be one, gated
// only if both are.
func TestDuplicateExportersAreOne(t *testing.T) {
	a, _ := startedAgent(t, &server{endpoint: "http://127.0.0.1:1"},
		&settingspb.Exporter{Signal: "traces", Name: "otlp", Gated: true},
		&settingspb.Exporter{Signal: "traces", Name: "otlp", Gated: false},
		&settingspb.Exporter{Signal: "logs", Name: "otlp", Gated: true},
		&settingspb.Exporter{Signal: "logs", Name: "otlp", Gated: true})
	a.mu.Lock()
	c := a.components(context.Background())
	a.mu.Unlock()
	if got, want := exportersOf(c), map[string]map[string]bool{"traces": {"otlp": false}, "logs": {"otlp": true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exporters %v, want %v", got, want)
	}
}

// withBuildInfo gives the package an application's build info for the test (go test's own lists
// no dependencies): this package at v0.5.0, autoexport and otelhttp at theirs.
func withBuildInfo(t *testing.T, bi *debug.BuildInfo) {
	t.Helper()
	before := buildInfo
	buildInfo = func() *debug.BuildInfo { return bi }
	t.Cleanup(func() { buildInfo = before })
}

var application = &debug.BuildInfo{
	Main: debug.Module{Path: "example.com/checkout", Version: "(devel)"},
	Deps: []*debug.Module{
		{Path: module, Version: "v0.5.0"},
		{Path: "github.com/pydantic/opamp-clients/go", Version: "v0.0.0", Replace: &debug.Module{Path: "../opamp/go"}},
		{Path: "go.opentelemetry.io/contrib", Version: "v1.46.0"},
		{Path: autoexportScope, Version: autoexportVersion},
		{Path: otelhttp, Version: "v0.71.0"},
	},
}

// A version comes from build info only for a scope in a module the binary has a version of: the
// longest module path the scope is in.
func TestAVersionFromBuildInfo(t *testing.T) {
	withBuildInfo(t, application)
	for path, want := range map[string]string{
		module:                     "v0.5.0",
		autoexportScope:            autoexportVersion,
		autoexportScope + "/inner": autoexportVersion,
		autoexportScope + "er":     "v1.46.0", // not under autoexport: under contrib
		otelhttp:                   "v0.71.0",
		"checkout":                 "", // an application's own scope
		"example.com/checkout/db":  "", // the application's module, built from a checkout
		"github.com/pydantic/opamp-clients/go/statekey": "", // replaced by a directory
		"": "",
	} {
		if got := moduleVersion(path); got != want {
			t.Errorf("moduleVersion(%q) = %q, want %q", path, got, want)
		}
	}
	withBuildInfo(t, nil)
	if moduleVersion(module) != "" {
		t.Error("no build info: no version")
	}
}

// A scope taken after the agent stopped, or before it started, reports nothing; the start then
// lists every scope taken before it.
func TestScopesBeforeTheStartAreInTheFirstReport(t *testing.T) {
	srv, last := componentsServer(t)
	a, err := newAgent(context.Background(), &server{endpoint: srv.URL + "/v1/opamp", heartbeat: 50 * time.Millisecond},
		NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.controls.TracerProvider(otel.GetTracerProvider()).Tracer("checkout")
	a.start(context.Background(), resource.Empty())
	eventually(t, "the scope taken before the start never reached the server", func() bool {
		_, ok := listed(last().GetComponents()["instrumentations"])["checkout"]
		return ok
	})
	if err := a.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.controls.TracerProvider(otel.GetTracerProvider()).Tracer("late")
	a.scopesChanged() // after stop: nothing, and the closed layer isn't called
}

// An application that names a tracer, or its version, per request doesn't grow the inventory: past
// the most names a new one isn't kept, a name keeps its first versions sorted, and what isn't kept
// isn't reported as a change.
func TestTheScopesKeptAreBounded(t *testing.T) {
	var s seenScopes
	changes := 0
	f := func() { changes++ }
	s.changed.Store(&f)
	for n := range maxScopeNames + 10 {
		s.add(fmt.Sprintf("scope%05d", maxScopeNames+9-n), "") // the last by name first
	}
	if changes != maxScopeNames+10 {
		t.Fatalf("%d changes, want %d", changes, maxScopeNames+10)
	}
	for n := maxScopeVersions + 10; n > 0; n-- {
		s.add("scope00000", fmt.Sprintf("1.%03d", n))
	}
	got, _ := s.instrumentations()
	if len(got) != maxScopeNames-1+maxScopeVersions {
		t.Fatalf("%d listed, want %d", len(got), maxScopeNames-1+maxScopeVersions)
	}
	var versions []string
	for _, i := range got {
		if i.GetName() == "scope00000" {
			versions = append(versions, i.GetVersion())
		}
	}
	slices.Sort(versions)
	want := []string{""}
	for n := 1; n < maxScopeVersions; n++ {
		want = append(want, fmt.Sprintf("1.%03d", n))
	}
	if !reflect.DeepEqual(versions, want) {
		t.Fatalf("versions %v, want %v", versions, want)
	}
	before := changes
	s.add("scope00000", "9.0")                         // past the last kept
	s.add(fmt.Sprintf("scope%05d", maxScopeNames), "") // a name past the most
	if changes != before {
		t.Fatal("what isn't kept was reported")
	}
	s.add("scope00000", "0.1") // before the last kept: in its place
	if changes != before+1 {
		t.Fatal("a version kept wasn't reported")
	}
	s.add("aaa", "") // a name before the first: in place of the last
	if changes != before+2 {
		t.Fatal("a name kept wasn't reported")
	}
	got, _ = s.instrumentations()
	names := map[string]bool{}
	for _, i := range got {
		names[i.GetName()] = true
	}
	if len(names) != maxScopeNames || !names["aaa"] || names[fmt.Sprintf("scope%05d", maxScopeNames-1)] {
		t.Fatalf("%d names kept, aaa %v, the last %v", len(names), names["aaa"], names[fmt.Sprintf("scope%05d", maxScopeNames-1)])
	}
}

// A version with a comma, which the layer leaves out, takes none of a name's slots: the name is
// still listed, with the versions it gave that the layer lists.
func TestAVersionWithACommaTakesNoSlot(t *testing.T) {
	var s seenScopes
	changes := 0
	f := func() { changes++ }
	s.changed.Store(&f)
	s.add("odd", "1,2")
	if got, _ := s.instrumentations(); changes != 1 || len(got) != 1 || got[0].GetName() != "odd" || got[0].GetVersion() != "" {
		t.Fatalf("%d changes, listed %v: want odd, with no version", changes, got)
	}
	s.add("odd", "3,4")
	if changes != 1 {
		t.Fatal("a version with a comma was reported as a change")
	}
	for n := range maxScopeVersions {
		s.add("scope", fmt.Sprintf("0,%03d", n)) // before every version below, were it kept
		s.add("scope", fmt.Sprintf("1.%03d", n))
	}
	got, _ := s.instrumentations()
	var versions []string
	for _, i := range got {
		if i.GetName() == "scope" {
			versions = append(versions, i.GetVersion())
		}
	}
	slices.Sort(versions)
	if len(versions) != maxScopeVersions || versions[0] != "1.000" || versions[maxScopeVersions-1] != fmt.Sprintf("1.%03d", maxScopeVersions-1) {
		t.Fatalf("versions %v: want the %d without a comma", versions, maxScopeVersions)
	}
}
