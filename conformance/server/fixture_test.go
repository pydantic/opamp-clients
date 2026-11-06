package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func newFixtureServer(t *testing.T, sc *Scenario) (*server, *bytes.Buffer) {
	t.Helper()
	sc.SDKFixture = true
	f, err := newFixture(sc)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	start := time.Now()
	return &server{sc: sc, start: start, log: &eventLog{w: out, start: start}, lastSeq: -1,
		open: map[int]*wsConn{}, fixture: f}, out
}

// events are the log's events named `name`.
func events(t *testing.T, log *bytes.Buffer, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var e map[string]any
		if line != "" && json.Unmarshal([]byte(line), &e) == nil && e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

// exchange is one message from the agent, `token` its bearer token: the server's reply.
func exchange(s *server, msg *protobufs.AgentToServer, token string) *protobufs.ServerToAgent {
	n, faults := s.messageFaults()
	s.logA2S(msg, n, 1, "http", nil)
	s.advance(n)
	resp, _ := s.respond(msg, faults, n, "http://127.0.0.1:1/v1/opamp", token)
	return resp
}

func onMessage(n int) *int { return &n }

func TestFixtureOffersTheConfigFromItsMessageOnUntilTheAgentHasIt(t *testing.T) {
	s, log := newFixtureServer(t, &Scenario{Faults: []Fault{
		{OnMessage: onMessage(3), Action: "offer_config", ConfigKey: "p.json", ContentType: "application/json", Body: `{"b":2}`},
		{OnMessage: onMessage(2), Action: "offer_config", ConfigKey: "p.json", ContentType: "application/json", Body: `{"until":"{now+20s}"}`},
	}})
	uid := []byte("0123456789abcdef")
	msg := func() *protobufs.AgentToServer { return &protobufs.AgentToServer{InstanceUid: uid} }
	if r := exchange(s, msg(), ""); r.RemoteConfig != nil {
		t.Fatal("offered before its message")
	}
	r := exchange(s, msg(), "")
	body := string(r.GetRemoteConfig().GetConfig().GetConfigMap()["p.json"].GetBody())
	if !strings.HasPrefix(body, `{"until":"20`) || strings.Contains(body, "now+") {
		t.Fatalf("message 2: %q", body)
	}
	if got := events(t, log, "offer_filled"); len(got) != 1 || got[0]["on_message"] != 2.0 {
		t.Fatalf("offer_filled: %v", got)
	}
	r = exchange(s, msg(), "")
	if got := string(r.GetRemoteConfig().GetConfig().GetConfigMap()["p.json"].GetBody()); got != `{"b":2}` {
		t.Fatalf("message 3: %q", got)
	}
	sum := sha256.Sum256([]byte(`{"b":2}`))
	has := &protobufs.AgentToServer{InstanceUid: uid, RemoteConfigStatus: &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: sum[:], Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}}
	if r := exchange(s, has, ""); r.RemoteConfig != nil {
		t.Fatal("offered to an agent that has it")
	}
	if r := exchange(s, msg(), ""); r.RemoteConfig != nil {
		t.Fatal("offered again to an agent that said it has it")
	}
}

func TestFixtureGivesAKeyConfirmsItThenRotatesAndMoves(t *testing.T) {
	s, log := newFixtureServer(t, &Scenario{EnrollmentKey: "enrol", AfterPromotion: []string{"rotate", "move"}, MoveHost: "h"})
	s.fixture.movedPort = "9"
	if s.fixture.authorized("wrong") || !s.fixture.authorized("enrol") {
		t.Fatal("the enrollment key alone is taken at first")
	}
	uid := []byte("0123456789abcdef")
	msg := &protobufs.AgentToServer{InstanceUid: uid}
	if r := exchange(s, msg, "enrol"); r.ConnectionSettings != nil {
		t.Fatal("a key offered to an agent that doesn't take connection settings")
	}
	msg.Capabilities = takesKeys
	token := "enrol"
	var dests []string
	for step := 0; step < 3; step++ {
		r := exchange(s, msg, token)
		op := r.GetConnectionSettings().GetOpamp()
		if op == nil || op.HeartbeatIntervalSeconds != fixtureHeartbeatS {
			t.Fatalf("step %d: no key offered with the interval: %v", step, r.ConnectionSettings)
		}
		key := strings.TrimPrefix(op.GetHeaders().GetHeaders()[0].GetValue(), "Bearer ")
		dests = append(dests, op.DestinationEndpoint)
		if !s.fixture.authorized(key) {
			t.Fatalf("step %d: the offered key is refused", step)
		}
		// APPLIED on the old key: not confirmed until the agent uses the new one.
		msg.ConnectionSettingsStatus = &protobufs.ConnectionSettingsStatus{LastConnectionSettingsHash: r.ConnectionSettings.Hash,
			Status: protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED}
		exchange(s, msg, token)
		if got := len(events(t, log, "key_promoted")); got != step {
			t.Fatalf("step %d: confirmed before the agent used it (%d)", step, got)
		}
		old := token
		token = key
		exchange(s, msg, token)
		if got := len(events(t, log, "key_promoted")); got != step+1 {
			t.Fatalf("step %d: not confirmed (%d)", step, got)
		}
		if step > 0 && s.fixture.authorized(old) {
			t.Fatalf("step %d: the replaced key still taken", step)
		}
	}
	want := []string{"http://127.0.0.1:1/v1/opamp", "http://127.0.0.1:1/v1/opamp", "http://h:9/v1/opamp"}
	if !reflect.DeepEqual(dests, want) {
		t.Fatalf("endpoints %v, want %v", dests, want)
	}
	var done []any
	for _, e := range events(t, log, "operator") {
		done = append(done, e["action"])
	}
	if !reflect.DeepEqual(done, []any{"rotate", "move"}) {
		t.Fatalf("operator: %v", done)
	}
	if r := exchange(s, msg, token); r.ConnectionSettings != nil {
		t.Fatal("a key offered after the last action")
	}
}

func TestFixtureAsksForComponentsItWasNotSent(t *testing.T) {
	s, _ := newFixtureServer(t, &Scenario{})
	uid := []byte("0123456789abcdef")
	ask := uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
	hashOnly := &protobufs.AgentToServer{InstanceUid: uid, AvailableComponents: &protobufs.AvailableComponents{Hash: []byte{1}}}
	if r := exchange(s, hashOnly, ""); r.Flags&ask == 0 {
		t.Fatal("not asked for a set it doesn't have")
	}
	whole := &protobufs.AgentToServer{InstanceUid: uid, AvailableComponents: &protobufs.AvailableComponents{Hash: []byte{1},
		Components: map[string]*protobufs.ComponentDetails{"sdk": {}}}}
	exchange(s, whole, "")
	if r := exchange(s, hashOnly, ""); r.Flags&ask != 0 {
		t.Fatal("asked for a set it has")
	}
}

func str(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func kv(k, v string) *commonpb.KeyValue { return &commonpb.KeyValue{Key: k, Value: str(v)} }

func TestSinkNamesSpansByURLAndFindsProbes(t *testing.T) {
	traces, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{Name: "GET", EndTimeUnixNano: 2_500_000_000, Attributes: []*commonpb.KeyValue{kv("url.full", "http://h/ping")},
				Events: []*tracepb.Span_Event{
					{Name: probeName, Attributes: []*commonpb.KeyValue{kv("url.full", "[REDACTED]"), kv(probeNote, "the password")}},
					{Name: probeName},
					{Name: "exception", Attributes: []*commonpb.KeyValue{kv("exception.type", "E")}},
				},
				Links: []*tracepb.Span_Link{{}}},
			{Name: "probe"},
		}}}}}})
	fields, ok := sinkFields("traces", traces)
	if !ok {
		t.Fatal("didn't decode")
	}
	if !reflect.DeepEqual(fields["spans"], []string{"http://h/ping", "probe"}) || fields["count"] != 2 ||
		!reflect.DeepEqual(fields["ends_wall"], []float64{2.5, 0}) || !reflect.DeepEqual(fields["services"], []string{""}) {
		t.Fatalf("traces: %v", fields)
	}
	want := []map[string]any{{"in": "event", "url": "[REDACTED]", "note": "the password"}, {"in": "event", "url": nil},
		{"in": "link", "url": nil}}
	if !reflect.DeepEqual(fields["probes"], want) {
		t.Fatalf("probes: %v", fields["probes"])
	}
	logs, _ := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{Body: str("requested"), Attributes: []*commonpb.KeyValue{kv("url.full", "http://h/ping")}},
			{Body: str("example.probe {url.full}")},
			{Body: str("settings changed")},
		}}}}}})
	fields, _ = sinkFields("logs", logs)
	want = []map[string]any{{"in": "log", "url": "http://h/ping"}, {"in": "log", "url": nil}}
	if fields["count"] != 3 || !reflect.DeepEqual(fields["probes"], want) {
		t.Fatalf("logs: %v", fields)
	}
}

func TestFillTimes(t *testing.T) {
	now := time.UnixMilli(1_760_400_000_250)
	for _, c := range []struct {
		in, out string
		filled  bool
	}{
		{`{"debug_until": "{now+20s}"}`, `{"debug_until": "2025-10-14T00:00:21Z"}`, true},
		{"{now+x}", "{now+x}", false},
		{"{now+5}", "{now+5}", false},
		{"{now+18446744073709551615s}", "9999-12-31T23:59:59Z", true},
		{"{now+18446744073709551616s}", "{now+18446744073709551616s}", false},
		{"{now+9223372036854775807s}", "9999-12-31T23:59:59Z", true},
	} {
		if got, filled := fillTimes(c.in, now); got != c.out || filled != c.filled {
			t.Errorf("fillTimes(%q) = %q, %v; want %q, %v", c.in, got, filled, c.out, c.filled)
		}
	}
}
