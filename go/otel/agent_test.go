// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// remote is a remote config with the policy body as its only file.
func remote(body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{ConfigHash: []byte(body), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{policyFile: {Body: []byte(body), ContentType: "application/json"}}}}
}

// effectiveBody is the policy that the agent's effective config reports, or a text that says what
// else the effective config holds.
func effectiveBody(a *agent) string {
	files := a.effective.GetConfigMap().GetConfigMap()
	if f, ok := files[policyFile]; ok && len(files) == 1 && f.GetContentType() == "application/json" {
		return string(f.GetBody())
	}
	return fmt.Sprintf("an effective config of %v", files)
}

func newTestAgent(t *testing.T, stateDir string) *agent {
	t.Helper()
	a, err := newAgent(context.Background(), &server{endpoint: "http://127.0.0.1:1", stateDir: stateDir}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.layer.Close(context.Background()) })
	return a
}

const (
	applied = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	failed_ = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
)

func TestInstrumentationSwitchesTakeEffectAtOnceAndAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir)
	body := every("instrumentations", override(`"disabled":["otelhttp"]`), "head_sampling", override(`"ratio":0.25`))
	st := a.remoteConfig(context.Background(), remote(body))
	if st.GetStatus() != applied || string(st.GetLastRemoteConfigHash()) != body {
		t.Fatalf("status %v", st)
	}
	if !a.controls.Off(otelhttp) || *a.controls.Sampler.rate != 0.25 {
		t.Fatal("both settings should be in force at once")
	}
	// In force now, so reported now: not pending a restart.
	if got := effectiveBody(a); !strings.Contains(got, `"instrumentations":{"disabled":["otelhttp"],"mode":"override"}`) {
		t.Fatalf("effective config %s", got)
	}
	// The client keeps the config applied, in its own state: nothing of the package's own.
	if _, err := os.Stat(filepath.Join(dir, "sdk-settings.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("sdk-settings.json written (%v)", err)
	}
	keepApplied(t, dir, body) // as the client does, before APPLIED goes out

	next := newTestAgent(t, dir)
	if !next.controls.Off(otelhttp) || next.uid != a.uid {
		t.Fatal("the next start: the switch still off, the same instance UID")
	}
}

func TestAnUnusableConfigChangesNothing(t *testing.T) {
	a := newTestAgent(t, t.TempDir())
	a.remoteConfig(context.Background(), remote(v1("signal_exports", exports("off", "", ""))))
	before := string(a.current)
	st := a.remoteConfig(context.Background(), remote(every("head_sampling", override(`"ratio":7`),
		"instrumentations", override(`"disabled":["otelhttp"]`))))
	if st.GetStatus() != failed_ || !strings.HasPrefix(st.GetErrorMessage(), "head_sampling:") {
		t.Fatalf("status %v", st)
	}
	if a.controls.Off(otelhttp) || a.controls.gates.Load().traces || string(a.current) != before {
		t.Fatal("all or nothing: nothing of a FAILED config is in force")
	}
}

// Every setting this package carries out takes effect at once, so none needs a state directory.
func TestWithoutAStateDirectoryEverySettingIsStillCarriedOut(t *testing.T) {
	a := newTestAgent(t, "")
	st := a.remoteConfig(context.Background(), remote(every("instrumentations", override(`"disabled":["otelhttp"]`))))
	if st.GetStatus() != applied || !a.controls.Off(otelhttp) {
		t.Fatalf("a live switch needs no saving: %v", st)
	}
}

func TestTheInstanceUIDIsKeptOrTakenFromTheResource(t *testing.T) {
	const given = "0190a3b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b"
	if u := identity("", "service.name=x, service.instance.id="+given); u.String() != given {
		t.Fatalf("no state dir: the resource's UUID, got %s", u)
	}
	if u := identity("", "service.instance.id=pod-7"); u.Version() != 7 {
		t.Fatalf("not a UUID: a new UUIDv7, got %s", u)
	}
	dir := t.TempDir()
	first := identity(dir, "service.instance.id="+given)
	if first.String() == given || identity(dir, "") != first {
		t.Fatal("with a state dir: its own UID, kept")
	}
	// Issue #218: a state directory nothing can be saved in keeps no UID, so the resource's UUID
	// names the instance at every start, as without one; not a new UID each start.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unsaved := filepath.Join(file, "state")
	for range 2 {
		if u := identity(unsaved, "service.instance.id="+given); u.String() != given {
			t.Fatalf("a state dir it can't save in: the resource's UUID, got %s", u)
		}
	}
	if u := identity(unsaved, ""); u.Version() != 7 {
		t.Fatalf("a state dir it can't save in, nothing given: a new UUIDv7, got %s", u)
	}
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestWithoutAnEndpointTheSDKIsUnmanaged(t *testing.T) {
	sdk, err := setup(context.Background(), env(map[string]string{
		"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none"}))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	if sdk.Controls != nil || sdk.agent != nil {
		t.Fatal("no endpoint: nothing under a server")
	}
	if _, ok := sdk.TracerProvider.(*sdktrace.TracerProvider); !ok || otel.GetTracerProvider() != sdk.TracerProvider {
		t.Fatal("the plain SDK, set globally")
	}
}

func TestAnEndpointInCodeManagesTheSDK(t *testing.T) {
	sdk, err := setup(context.Background(), env(map[string]string{
		"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none"}),
		WithEndpoint("http://127.0.0.1:1/v1/opamp"), WithStateDir("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	if sdk.Controls == nil || sdk.agent.server.endpoint != "http://127.0.0.1:1/v1/opamp" {
		t.Fatal("no OTEL_OPAMP_ENDPOINT, WithEndpoint: under the server")
	}
}

// What is given in code is what the client sends: to the endpoint given, with the headers given
// and none of OTEL_OPAMP_HEADERS', at the interval given; and unusable options hold nothing up.
func TestTheServerGivenInCodeIsTheOneReached(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none") // autoexport reads the process environment
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	type request struct{ auth, env string }
	requests := make(chan request, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- request{r.Header.Get("Authorization"), r.Header.Get("X-Env")}:
		default:
		}
		body, _ := io.ReadAll(r.Body)
		var m protobufs.AgentToServer
		_ = proto.Unmarshal(body, &m)
		out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid()})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	defer srv.Close()
	sdk, err := setup(context.Background(), env(map[string]string{
		"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp", "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20env,X-Env=1",
		"OTEL_OPAMP_HEARTBEAT_INTERVAL": "1h", "OTEL_METRICS_EXPORTER": "none"}),
		WithEndpoint(srv.URL+"/v1/opamp"), WithHeaders(map[string]string{"Authorization": "Bearer code", "Bad Name": "x"}),
		WithStateDir(t.TempDir()), WithHeartbeatInterval(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	for i := range 3 { // the first message, then heartbeats at the interval given
		select {
		case r := <-requests:
			if r != (request{"Bearer code", ""}) {
				t.Fatalf("message %d: the headers given in code alone, got %+v", i, r)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d: the server given in code not reached at the interval given", i)
		}
	}
}

func TestAnUnreachableServerLeavesTheSDKRunning(t *testing.T) {
	os.Setenv("OTEL_TRACES_EXPORTER", "none") // autoexport reads the process environment
	os.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Cleanup(func() { os.Unsetenv("OTEL_TRACES_EXPORTER"); os.Unsetenv("OTEL_LOGS_EXPORTER") })
	sdk, err := setup(context.Background(), env(map[string]string{
		"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp", "OTEL_METRICS_EXPORTER": "none",
		"OTEL_TRACES_SAMPLER": "always_off"}))
	if err != nil {
		t.Fatal(err)
	}
	if sdk.Controls == nil || sdk.agent.client == nil {
		t.Fatal("the client starts whether or not the server answers")
	}
	if !strings.Contains(sdk.Resource.String(), "service.instance.id="+sdk.agent.uid.String()) {
		t.Fatalf("the resource carries the instance UID: %s", sdk.Resource)
	}
	_, span := otel.Tracer("checkout").Start(context.Background(), "s")
	if span.IsRecording() {
		t.Fatal("the configured sampler (always_off) is in force")
	}
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = sdk.Shutdown(ctx)
	if ctx.Err() != nil {
		t.Fatal("shutting down waited on the unreachable server")
	}
}

func TestAFailedSetupLeavesTheGlobalsAlone(t *testing.T) {
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Cleanup(func() { os.Unsetenv("OTEL_TRACES_EXPORTER"); os.Unsetenv("OTEL_LOGS_EXPORTER") })
	tp, prop := tracenoop.NewTracerProvider(), propagation.Baggage{}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	// The traces are set up, then the metrics fail.
	_, err := setup(context.Background(), env(map[string]string{
		"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp", "OTEL_EXPORTER_OTLP_PROTOCOL": "carrier-pigeon"}))
	if err == nil {
		t.Fatal("an unknown OTLP protocol should fail Setup")
	}
	if otel.GetTracerProvider() != tp || otel.GetTextMapPropagator() != prop {
		t.Fatal("a failed Setup set the globals (to a provider it has shut down)")
	}
}

func TestABadHeartbeatIntervalIsTheDefault(t *testing.T) {
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Cleanup(func() { os.Unsetenv("OTEL_TRACES_EXPORTER"); os.Unsetenv("OTEL_LOGS_EXPORTER") })
	sdk, err := setup(context.Background(), env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp",
		"OTEL_OPAMP_HEARTBEAT_INTERVAL": "soon", "OTEL_METRICS_EXPORTER": "none"}))
	if err != nil {
		t.Fatalf("a bad interval must not hold the application up: %v", err)
	}
	defer sdk.Shutdown(context.Background())
	if sdk.Controls == nil || sdk.agent.server.heartbeat != 0 {
		t.Fatal("managed, at the client's default interval")
	}
}

// A server over HTTP that offers remote in its answer to the first message, and records the
// remote config statuses it is sent.
func offeringServer(t *testing.T, remote *protobufs.AgentRemoteConfig) (*httptest.Server, chan *protobufs.RemoteConfigStatus) {
	t.Helper()
	statuses := make(chan *protobufs.RemoteConfigStatus, 64)
	var messages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var m protobufs.AgentToServer
		if err != nil || proto.Unmarshal(body, &m) != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		if s := m.GetRemoteConfigStatus(); s != nil {
			select {
			case statuses <- s:
			default:
			}
		}
		reply := &protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid()}
		if messages.Add(1) == 1 {
			reply.RemoteConfig = remote
		}
		out, _ := proto.Marshal(reply)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, statuses
}

// The first notification (a remote config in the answer to the first message) is answered through
// the client it comes with: callbacks wait for Start to return, so no queue holds them until the
// client is there (with -race, a callback reading a.client, which start sets after Start
// returns, races).
// The server hears APPLIED once the client has saved the config with its state: a start after
// that comes back to it, with no file of the package's own.
func TestAConfigAppliedThroughTheServerIsWhereTheNextStartBegins(t *testing.T) {
	body := every("instrumentations", override(`"disabled":["otelhttp"]`), "head_sampling", override(`"ratio":0.25`))
	srv, statuses := offeringServer(t, remote(body))
	dir := t.TempDir()
	a, err := newAgent(context.Background(), &server{endpoint: srv.URL + "/v1/opamp", stateDir: dir,
		heartbeat: 100 * time.Millisecond}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.start(context.Background(), resource.Empty())
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case s := <-statuses:
			done = s.GetStatus() == applied && string(s.GetLastRemoteConfigHash()) == body
		case <-deadline:
			t.Fatal("never answered APPLIED")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sdk-settings.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("sdk-settings.json written (%v)", err)
	}
	next := newTestAgent(t, dir)
	if !next.controls.Off(otelhttp) || *next.controls.Sampler.rate != 0.25 {
		t.Fatal("the next start: the config applied should be in force")
	}
}

// The settings to start with are the config the client kept applied, else the file this package
// kept before (an earlier release's), which carries over until a config is applied.
func TestTheSettingsToStartWithAreTheConfigTheClientKeptAppliedElseTheFileKeptBefore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sdk-settings.json"), []byte(v1("head_sampling", override(`"ratio":0.25`))), 0o600); err != nil {
		t.Fatal(err)
	}
	rate := func() any {
		a := newTestAgent(t, dir)
		if r := a.controls.Sampler.rate; r != nil {
			return *r
		}
		return nil
	}
	if r := rate(); r != 0.25 {
		t.Fatalf("kept before: rate %v", r)
	}
	keepApplied(t, dir, v1("head_sampling", override(`"ratio":0.5`)))
	if r := rate(); r != 0.5 {
		t.Fatalf("kept applied by the client: rate %v", r)
	}
	keepApplied(t, dir, v1()) // a config applied with every control local: the SDK's own, not the old file's
	if r := rate(); r != nil {
		t.Fatalf("a config applied with every control local: rate %v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "opamp-state"), []byte{0xff, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := rate(); r != 0.25 {
		t.Fatalf("a client state that doesn't read: rate %v", r)
	}
}

func TestTheFirstNotificationIsAnsweredThroughItsClient(t *testing.T) {
	body := every("instrumentations", override(`"disabled":["otelhttp"]`))
	srv, statuses := offeringServer(t, remote(body))
	a, err := newAgent(context.Background(), &server{endpoint: srv.URL + "/v1/opamp", stateDir: t.TempDir(),
		heartbeat: 100 * time.Millisecond}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.start(context.Background(), resource.Empty())
	if a.client == nil {
		t.Fatal("the client did not start")
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case s := <-statuses:
			if s.GetStatus() == applied && string(s.GetLastRemoteConfigHash()) == body {
				if !a.controls.Off(otelhttp) {
					t.Fatal("applied, but not in force")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := a.stop(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatal("the remote config offered first was never answered APPLIED")
		}
	}
}

func TestAUIDTheServerAssignsIsTheNextStarts(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir)
	assigned := uuid.Must(uuid.NewV7())
	a.notified(nil, opamp.Notification{Kind: opamp.InstanceUIDChanged, InstanceUID: assigned})
	if next := newTestAgent(t, dir); next.uid != assigned {
		t.Fatalf("the next start is %s, want the server's %s", next.uid, assigned)
	}
}

// The description the agent sends at start names the policy format the layer reads and the
// controls this package carries out, beside the resource's attributes.
func TestTheDescriptionNamesThePolicyAndTheControls(t *testing.T) {
	descriptions := make(chan *protobufs.AgentDescription, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m protobufs.AgentToServer
		_ = proto.Unmarshal(body, &m)
		if d := m.GetAgentDescription(); d != nil {
			select {
			case descriptions <- d:
			default:
			}
		}
		out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid()})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	defer srv.Close()
	a, err := newAgent(context.Background(), &server{endpoint: srv.URL + "/v1/opamp", stateDir: t.TempDir(),
		heartbeat: time.Hour}, NewControls(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.start(context.Background(), resource.NewSchemaless(attribute.String("service.name", "checkout")))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.stop(ctx)
	}()
	var d *protobufs.AgentDescription
	select {
	case d = <-descriptions:
	case <-time.After(10 * time.Second):
		t.Fatal("no description sent")
	}
	attrs := map[string]string{}
	for _, kv := range d.GetNonIdentifyingAttributes() {
		if arr := kv.GetValue().GetArrayValue(); arr != nil {
			var each []string
			for _, v := range arr.GetValues() {
				each = append(each, v.GetStringValue())
			}
			attrs[kv.GetKey()] = strings.Join(each, ",")
		} else {
			attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	want := map[string]string{
		"logfire.opamp.config_format": "logfire-sdk-policy/v1",
		"logfire.opamp.supported_controls": "head_sampling,signal_exports.traces,signal_exports.metrics,signal_exports.logs," +
			"sampling_rules,trace_rate_limit,log_levels,log_rate_limit,attribute_redaction,debug,app_settings,instrumentations",
	}
	for k, w := range want {
		if attrs[k] != w {
			t.Errorf("%s: %q, want %q", k, attrs[k], w)
		}
	}
}

// The description carries the resource's attributes typed as they are, arrays as arrays.
func TestTheDescriptionKeepsTheResourcesTypes(t *testing.T) {
	res := resource.NewSchemaless(attribute.String("s", "x"), attribute.Bool("b", true), attribute.Int64("i", 7),
		attribute.Float64("f", 0.5), attribute.StringSlice("ss", []string{"a", "b"}), attribute.BoolSlice("bs", []bool{true}),
		attribute.Int64Slice("is", []int64{1, 2}), attribute.Float64Slice("fs", []float64{0.25}))
	got := map[string]string{}
	for _, kv := range keyValues(res) {
		got[kv.Key] = prototext.MarshalOptions{}.Format(kv.Value)
	}
	want := map[string]string{
		"s": `string_value:"x"`, "b": `bool_value:true`, "i": `int_value:7`, "f": `double_value:0.5`,
		"ss": `array_value:{values:{string_value:"a"} values:{string_value:"b"}}`,
		"bs": `array_value:{values:{bool_value:true}}`,
		"is": `array_value:{values:{int_value:1} values:{int_value:2}}`,
		"fs": `array_value:{values:{double_value:0.25}}`,
	}
	for k, w := range want {
		if strings.Join(strings.Fields(got[k]), " ") != w {
			t.Errorf("%s: %s, want %s", k, got[k], w)
		}
	}
}

// records is what is logged through slog's default logger while the test runs, as "LEVEL msg".
func records(t *testing.T) *[]string {
	t.Helper()
	var said []string
	var mu sync.Mutex
	before := slog.Default()
	slog.SetDefault(slog.New(handlerFunc(func(r slog.Record) {
		mu.Lock()
		defer mu.Unlock()
		said = append(said, r.Level.String()+" "+r.Message)
	})))
	t.Cleanup(func() { slog.SetDefault(before) })
	return &said
}

type handlerFunc func(slog.Record)

func (h handlerFunc) Enabled(context.Context, slog.Level) bool      { return true }
func (h handlerFunc) Handle(_ context.Context, r slog.Record) error { h(r); return nil }
func (h handlerFunc) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h handlerFunc) WithGroup(string) slog.Handler                 { return h }

func TestConnectingFailingAndRecoveringAreEachSaidOnce(t *testing.T) {
	said := records(t)
	uid := uuid.UUID{15: 7}
	r := &reachability{endpoint: where("https://user:secret@opamp.example.com/v1/opamp?token=x#frag"), service: "checkout", uid: uid}
	for _, n := range []opamp.Notification{
		{Kind: opamp.ConnectFailed, Status: 401, Detail: "HTTP 401"},
		{Kind: opamp.ConnectFailed, Status: 401, Detail: "HTTP 401"},
		{Kind: opamp.Connected},
		{Kind: opamp.Connected},
		{Kind: opamp.ConnectFailed, Detail: "connection refused"},
		{Kind: opamp.Connected},
	} {
		r.notified(n)
	}
	want := []string{
		"WARN opamp: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's credentials: " +
			"check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background; the SDK keeps its current settings meanwhile",
		"DEBUG opamp: still can't reach https://opamp.example.com/v1/opamp: HTTP 401",
		"INFO opamp: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance " + uid.String(),
		"WARN opamp: can't reach https://opamp.example.com/v1/opamp (connection refused): check OTEL_OPAMP_ENDPOINT and that " +
			"the server is up. Retrying in the background; the SDK keeps its current settings meanwhile",
		"INFO opamp: connected to https://opamp.example.com/v1/opamp again",
	}
	if strings.Join(*said, "\n") != strings.Join(want, "\n") {
		t.Fatalf("said:\n%s", strings.Join(*said, "\n"))
	}
}

func TestTheWarningNamesTheOptionThatGaveTheServer(t *testing.T) {
	for _, c := range []struct {
		opts                  []Option
		vars                  map[string]string
		endpointFrom, headers string
	}{
		{nil, map[string]string{"OTEL_OPAMP_ENDPOINT": "http://h/v1/opamp", "OTEL_OPAMP_HEADERS": "a=b"},
			"OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS"},
		// Given in code, each wins over its variable, and the warning names the option.
		{[]Option{WithEndpoint("http://h/v1/opamp"), WithHeaders(map[string]string{"Authorization": "Bearer k"})},
			map[string]string{"OTEL_OPAMP_ENDPOINT": "http://other/v1/opamp", "OTEL_OPAMP_HEADERS": "a=b"},
			"WithEndpoint", "WithHeaders"},
		{[]Option{WithEndpoint("http://h/v1/opamp")}, map[string]string{"OTEL_OPAMP_HEADERS": "a=b"},
			"WithEndpoint", "OTEL_OPAMP_HEADERS"},
	} {
		s := serverFromEnv(env(c.vars), c.opts...)
		said := records(t)
		r := &reachability{endpoint: where(s.endpoint), endpointFrom: s.endpointFrom, headersFrom: s.headersFrom}
		r.notified(opamp.Notification{Kind: opamp.ConnectFailed, Status: 401})
		r.notified(opamp.Notification{Kind: opamp.Connected})
		r.notified(opamp.Notification{Kind: opamp.ConnectFailed, Status: 404})
		r.notified(opamp.Notification{Kind: opamp.Connected})
		r.notified(opamp.Notification{Kind: opamp.ConnectFailed, Detail: "connection refused"})
		want := []string{"check " + c.headers + " (Authorization=Bearer <token>)",
			"check " + c.endpointFrom + " (it usually ends in /v1/opamp)", "check " + c.endpointFrom + " and that the server is up"}
		if len(*said) != 5 || !strings.Contains((*said)[0], want[0]) || !strings.Contains((*said)[2], want[1]) ||
			!strings.Contains((*said)[4], want[2]) {
			t.Errorf("%v: said %q", c.endpointFrom, *said)
		}
	}
}

func TestWithoutAServiceNameTheInstanceAloneIsNamed(t *testing.T) {
	said := records(t)
	r := &reachability{endpoint: "http://127.0.0.1:4320/v1/opamp"}
	r.notified(opamp.Notification{Kind: opamp.ConnectFailed, Status: 404, Detail: ""})
	r.notified(opamp.Notification{Kind: opamp.Connected})
	if len(*said) != 2 || !strings.Contains((*said)[0], "(HTTP 404): nothing serves OpAMP at that path") ||
		(*said)[1] != "INFO opamp: connected to http://127.0.0.1:4320/v1/opamp; this SDK is managed, instance "+uuid.Nil.String() {
		t.Fatalf("said: %q", *said)
	}
}

func TestTheEndpointIsLoggedWithoutCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:secret@opamp.example.com:8443/v1/opamp?token=x#f": "https://opamp.example.com:8443/v1/opamp",
		"wss://[::1]:4320/v1/opamp":                                     "wss://[::1]:4320/v1/opamp",
		"user:secret@opamp.example.com:4320/v1/opamp?token=x":           "opamp.example.com:4320/v1/opamp",
		"https://user:secret@opamp.example.com:notaport/v1/opamp?k=v":   "https://opamp.example.com:notaport/v1/opamp",
		"https://[::1/v1/opamp?k=v":                                     "https://[::1/v1/opamp",
		"https://opamp.example.com":                                     "https://opamp.example.com",
	} {
		if got := where(in); got != want {
			t.Errorf("where(%q) = %q, want %q", in, got, want)
		}
	}
	if hint(500, "", "") != "the server answered with an error" {
		t.Error("another status: the server's error")
	}
}

func TestAnEndpointWithoutASchemeIsWarnedAbout(t *testing.T) {
	said := records(t)
	s := serverFromEnv(env(map[string]string{"OTEL_OPAMP_ENDPOINT": "user:secret@opamp.example.com:4320/v1/opamp"}))
	if s == nil {
		t.Fatal("still on")
	}
	// Taken as http://, as the warning says: the client refuses an endpoint without a scheme.
	if s.endpoint != "http://user:secret@opamp.example.com:4320/v1/opamp" {
		t.Fatalf("endpoint %q", s.endpoint)
	}
	want := `WARN opamp: OTEL_OPAMP_ENDPOINT "opamp.example.com:4320/v1/opamp" has no scheme, so it is taken as http://; ` +
		"name it in full, as https://host/v1/opamp or wss://host/v1/opamp"
	if len(*said) != 1 || (*said)[0] != want {
		t.Fatalf("said: %q", *said)
	}
	serverFromEnv(env(map[string]string{"OTEL_OPAMP_ENDPOINT": "https://opamp.example.com/v1/opamp"}))
	if len(*said) != 1 {
		t.Fatalf("a full endpoint: no warning; said: %q", *said)
	}
}

// The layer carries out only what the host declares: this package names only settings
// (the layer refuses a host naming anything else) and every one it carries out.
func TestTheHostDeclaresEverySettingThisPackageCarriesOut(t *testing.T) {
	h := goHost()
	if _, err := testLayer(t).LoadAt(context.Background(), nil, h, vectorNow); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, k := range h.GetLive() {
		seen[k] = true
	}
	if len(seen) != 17 || !seen["send_metrics"] || !seen["deactivate_instrumentations"] || !seen["custom"] ||
		len(h.GetNextStart()) != 0 {
		t.Fatalf("live %v, next start %v", h.GetLive(), h.GetNextStart())
	}
	for _, k := range (&server{metricsUngated: true}).host().GetLive() {
		if k == "send_metrics" {
			t.Fatal("send_metrics declared beside a metrics reader of autoexport's own")
		}
	}
}

// A metrics reader of autoexport's own is not gated. So a policy that switches the metrics export
// off is refused, and the server does not see metrics stopped while they go on.
func TestSendMetricsIsRefusedWhenTheMetricsReaderIsAutoexportsOwn(t *testing.T) {
	for _, tc := range []struct {
		env   map[string]string
		gated bool
	}{
		{map[string]string{}, true},
		{map[string]string{"OTEL_METRICS_EXPORTER": "console"}, true},
		{map[string]string{"OTEL_METRICS_EXPORTER": "none"}, true},
		{map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}, false},
		{map[string]string{"OTEL_METRICS_PRODUCERS": "prometheus"}, false},
	} {
		getenv := func(k string) string { return tc.env[k] }
		if metricsGated(getenv) != tc.gated {
			t.Fatalf("%v: gated %v, want %v", tc.env, !tc.gated, tc.gated)
		}
		srv := &server{endpoint: "http://127.0.0.1:1", metricsUngated: !tc.gated}
		a, err := newAgent(context.Background(), srv, NewControls(), "")
		if err != nil {
			t.Fatal(err)
		}
		doc := v1("signal_exports", exports("off", "off", ""))
		st := a.remoteConfig(context.Background(), remote(doc))
		want, eff := applied, doc
		if !tc.gated {
			want, eff = failed_, v1()
			if st.GetErrorMessage() != "signal_exports.metrics: this SDK cannot apply the requested override" {
				t.Fatalf("%v: refused with %q", tc.env, st.GetErrorMessage())
			}
		}
		if st.GetStatus() != want || effectiveBody(a) != full(t, eff) || a.controls.gates.Load().metrics == tc.gated ||
			a.controls.gates.Load().traces != !tc.gated {
			t.Fatalf("%v: status %v, effective %s, sending metrics %v", tc.env, st, effectiveBody(a), a.controls.gates.Load().metrics)
		}
		_ = a.layer.Close(context.Background())
	}
}

// After the server moved the agent to another endpoint, what it says names the one it uses now
// (as the client gives it, logged without credentials), not the one configured.
func TestWhatIsSaidNamesTheEndpointInUse(t *testing.T) {
	said := records(t)
	inUse := "" // none yet: the configured one
	r := &reachability{endpoint: where("https://a.example.com/v1/opamp"), inUse: func() string { return inUse }}
	r.notified(opamp.Notification{Kind: opamp.Connected})
	inUse = "https://user:secret@b.example.com/v1/opamp?token=x"
	r.notified(opamp.Notification{Kind: opamp.ConnectFailed, Detail: "connection refused"})
	r.notified(opamp.Notification{Kind: opamp.Connected})
	if len(*said) != 3 || !strings.HasPrefix((*said)[0], "INFO opamp: connected to https://a.example.com/v1/opamp;") ||
		!strings.HasPrefix((*said)[1], "WARN opamp: can't reach https://b.example.com/v1/opamp (connection refused)") ||
		(*said)[2] != "INFO opamp: connected to https://b.example.com/v1/opamp again" {
		t.Fatalf("said: %q", *said)
	}
}
