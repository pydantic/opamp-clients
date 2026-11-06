// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

// The SDK example fixture (examples/integrations.py): the server the example applications run
// against, and the sink their telemetry goes to. A scenario turns it on with "sdk_fixture". It
// differs from a conformance scenario in four ways:
//
//   - Offers are the config the server wants from then on, not one reply: each offer_config fault
//     makes its body the config from the message it names (counted over every agent's messages)
//     on, offered in every response while the hash the agent last reported differs (as
//     RemoteConfig). A body may name a time relative to when it is first offered, {now+20s}: it
//     is sent as that RFC 3339 time (UTC, whole seconds, rounded up), and the log says when it was
//     filled in (offer_filled).
//   - enrollment_key makes the server require that key, or one it gave, as a bearer token (any
//     other is refused with 401, logged as request_rejected and no a2s). An agent that takes
//     OpAMP connection settings and reports their status is given a key of its own at the
//     endpoint it reached, with a heartbeat interval of a second (the one the examples are set
//     up with), and the server confirms it (key_promoted) once the agent has said APPLIED and
//     uses it. after_promotion lists what the operator does each time a key is confirmed, in
//     order: rotate (a new key at the same endpoint) or move (a new key at the server's second
//     listener, named by move_host, 127.0.0.1 unless given), each logged as operator. With a
//     move, each connection either listener accepts is logged (connection, listener "first" or
//     "second").
//   - The sink has a port of its own (telemetry_addr): every OTLP export is logged with how many
//     items it carried, when each span ended (ends_wall, on the wall clock) and its probes (see
//     probesIn), and every other request as a stray_request (the applications' own requests).
//   - Messages are logged with what the agent says of itself: its description, health, available
//     components and effective config (its JSON files, parsed).

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// fixtureHeartbeatS is the heartbeat interval offered with an agent's key: the one the example
// applications are set up with. An agent keeps its key's interval across restarts once it has
// APPLIED it, so the server's offer is what its next start checks in by.
const fixtureHeartbeatS = 1

// takesKeys: the capabilities an agent declares to be given a key of its own.
const takesKeys = uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings |
	protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus)

// fixtureOffer is a config the server wants from a message on.
type fixtureOffer struct {
	message     int
	key         string
	contentType string
	body        string
}

// fixtureAgent is what the server holds of one agent's keys.
type fixtureAgent struct {
	// current: its confirmed key ("" before the first); offered: the key it was offered and has
	// not yet been confirmed on, with the endpoint and hash of the offer.
	current, offered string
	dest             string
	hash             []byte
	// applied: the agent said APPLIED for the offer; answered: it said APPLIED or FAILED.
	applied, answered bool
	// components: the hash of the available components the agent sent whole.
	components []byte
}

type fixture struct {
	mu sync.Mutex
	// The offers not made yet, in order, and the one in force (its body filled in) with its hash.
	pending []fixtureOffer
	config  *fixtureOffer
	hash    []byte
	// Keys (enrollment_key).
	enrollment string
	actions    []string
	moveHost   string
	movedPort  string
	tokens     map[string]string // a key the server gave -> the agent's UID (hex)
	agents     map[string]*fixtureAgent
	// moved: the second listener (with a move).
	moved net.Listener
}

func newFixture(sc *Scenario) (*fixture, error) {
	f := &fixture{enrollment: sc.EnrollmentKey, actions: sc.AfterPromotion, moveHost: sc.MoveHost,
		tokens: map[string]string{}, agents: map[string]*fixtureAgent{}}
	if f.moveHost == "" {
		f.moveHost = "127.0.0.1"
	}
	for _, a := range f.actions {
		if a != "rotate" && a != "move" {
			return nil, fmt.Errorf("after_promotion: %q is not rotate or move", a)
		}
	}
	for i, fault := range sc.Faults {
		if fault.Action != "offer_config" || fault.OnMessage == nil {
			return nil, fmt.Errorf("fault %d: the SDK fixture only takes offer_config on a message", i)
		}
		if fault.ConfigKey == "" || fault.ContentType == "" {
			return nil, fmt.Errorf("fault %d: offer_config needs config_key and content_type", i)
		}
		f.pending = append(f.pending, fixtureOffer{message: *fault.OnMessage, key: fault.ConfigKey,
			contentType: fault.ContentType, body: fault.Body})
	}
	sortOffers(f.pending)
	sc.Faults = nil
	return f, nil
}

func sortOffers(offers []fixtureOffer) {
	for i := 1; i < len(offers); i++ {
		for j := i; j > 0 && offers[j].message < offers[j-1].message; j-- {
			offers[j], offers[j-1] = offers[j-1], offers[j]
		}
	}
}

func (f *fixture) moves() bool {
	for _, a := range f.actions {
		if a == "move" {
			return true
		}
	}
	return false
}

// fixtureListeners starts the sink on a port of its own and, with a move, the second listener,
// and names both in `ready`: the listener to serve OpAMP on, connections logged with a move.
func (s *server) fixtureListeners(ln net.Listener, ready map[string]any) (net.Listener, error) {
	f := s.fixture
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	sink, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	s.serveSink(sink)
	ready["telemetry_addr"] = sink.Addr().String()
	ready["moved_addr"] = nil
	if !f.moves() {
		return ln, nil
	}
	moved, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	_, f.movedPort, _ = net.SplitHostPort(moved.Addr().String())
	f.moved = connListener{moved, "second", s.log}
	ready["moved_addr"] = moved.Addr().String()
	return connListener{ln, "first", s.log}, nil
}

// bearer is the request's bearer token.
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// authorized: without an enrollment key, every request; with one, a request with that key or a
// key the server gave.
func (f *fixture) authorized(token string) bool {
	if f.enrollment == "" {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, given := f.tokens[token]
	return token == f.enrollment || given
}

// refuse answers a request without the key, as the reference server does: 401, logged as
// request_rejected (the message is not taken in, so not logged as a2s).
func (s *server) refuse(w http.ResponseWriter, r *http.Request, upgrade bool) {
	w.WriteHeader(http.StatusUnauthorized)
	s.log.emit("request_rejected", map[string]any{"status": http.StatusUnauthorized, "upgrade": upgrade})
}

// advance makes the config of each offer whose message has come the one in force.
func (s *server) advance(n int) {
	f := s.fixture
	f.mu.Lock()
	var filled []int
	for len(f.pending) > 0 && n >= f.pending[0].message {
		o := f.pending[0]
		f.pending = f.pending[1:]
		body, did := fillTimes(o.body, time.Now())
		o.body = body
		sum := sha256.Sum256([]byte(body))
		f.config, f.hash = &o, sum[:]
		if did {
			filled = append(filled, o.message)
		}
	}
	f.mu.Unlock()
	for _, m := range filled {
		s.log.emit("offer_filled", map[string]any{"on_message": m})
	}
}

// fixtureRespond adds to `resp` the config in force and the agent's key, where it has one to be
// given. `token` is the request's bearer token, `url` the endpoint it reached.
func (s *server) fixtureRespond(msg *protobufs.AgentToServer, token, url string, resp *protobufs.ServerToAgent,
	info map[string]any) {
	f := s.fixture
	if msg.AgentDisconnect != nil {
		return
	}
	f.mu.Lock()
	if f.config != nil {
		reported := []byte(nil)
		if st := msg.RemoteConfigStatus; st != nil {
			reported = st.LastRemoteConfigHash
		}
		s.mu.Lock()
		if reported != nil {
			s.reportedConfig = reported
		}
		differs := !bytes.Equal(s.reportedConfig, f.hash)
		s.mu.Unlock()
		if differs {
			resp.RemoteConfig = &protobufs.AgentRemoteConfig{
				Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
					f.config.key: {Body: []byte(f.config.body), ContentType: f.config.contentType}}},
				ConfigHash: f.hash,
			}
			info["offer_config"] = hex.EncodeToString(f.hash)
		}
	}
	uid := hex.EncodeToString(msg.InstanceUid)
	a := f.agents[uid]
	if a == nil {
		a = &fixtureAgent{}
		f.agents[uid] = a
	}
	// Components: an agent sends the hash of its set, and the set only when asked for it.
	if c := msg.AvailableComponents; c != nil && len(c.Hash) > 0 {
		if len(c.Components) > 0 {
			a.components = c.Hash
		} else if !bytes.Equal(a.components, c.Hash) {
			resp.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
			info["ask_components"] = true
		}
	}
	if f.enrollment == "" {
		f.mu.Unlock()
		return
	}
	var events [][2]any
	if st := msg.ConnectionSettingsStatus; st != nil && a.offered != "" && bytes.Equal(st.LastConnectionSettingsHash, a.hash) {
		switch st.Status {
		case protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED:
			a.applied, a.answered = true, true
		case protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED:
			a.answered = true
		}
	}
	if a.offered != "" && a.applied && token == a.offered {
		// Confirmed: the old key goes, and the operator does what comes next.
		if a.current != "" {
			delete(f.tokens, a.current)
		}
		a.current, a.offered, a.applied, a.answered = a.offered, "", false, false
		events = append(events, [2]any{"key_promoted", map[string]any{"uid": uid}})
		if len(f.actions) > 0 {
			action := f.actions[0]
			f.actions = f.actions[1:]
			dest := url
			if action == "move" {
				scheme, _, _ := strings.Cut(url, "://")
				dest = scheme + "://" + net.JoinHostPort(f.moveHost, f.movedPort) + "/v1/opamp"
			}
			f.offer(a, uid, dest)
			events = append(events, [2]any{"operator", map[string]any{"action": action, "agents": 1}})
		}
	}
	if a.current == "" && a.offered == "" && token == f.enrollment && msg.Capabilities&takesKeys == takesKeys {
		f.offer(a, uid, url)
	}
	if a.offered != "" && !a.answered {
		resp.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
			Hash: a.hash,
			Opamp: &protobufs.OpAMPConnectionSettings{
				DestinationEndpoint: a.dest,
				Headers: &protobufs.Headers{Headers: []*protobufs.Header{
					{Key: "Authorization", Value: "Bearer " + a.offered}}},
				HeartbeatIntervalSeconds: fixtureHeartbeatS,
			},
		}
		info["offer_key"] = true
		info["heartbeat_offer"] = fixtureHeartbeatS
		info["settings_hash"] = hex.EncodeToString(a.hash)
	}
	f.mu.Unlock()
	for _, e := range events {
		s.log.emit(e[0].(string), e[1].(map[string]any))
	}
}

// offer gives agent `a` a new key at `dest` (f.mu held).
func (f *fixture) offer(a *fixtureAgent, uid, dest string) {
	raw := make([]byte, 16)
	rand.Read(raw)
	key := "key-" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key + " " + dest))
	a.offered, a.dest, a.hash, a.applied, a.answered = key, dest, sum[:], false, false
	f.tokens[key] = uid
}

// connListener logs each connection it accepts (connection, with its listener's name).
type connListener struct {
	net.Listener
	name string
	log  *eventLog
}

func (l connListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.log.emit("connection", map[string]any{"listener": l.name})
	}
	return c, err
}

// serveSink serves the sink on `ln`: OTLP exports, and every other request a stray_request.
func (s *server) serveSink(ln net.Listener) {
	mux := http.NewServeMux()
	for _, signal := range []string{"traces", "metrics", "logs"} {
		mux.HandleFunc("/v1/"+signal, func(w http.ResponseWriter, r *http.Request) {
			s.sinkExport(signal, w, r)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.emit("stray_request", map[string]any{"path": r.URL.Path, "method": r.Method})
		w.WriteHeader(http.StatusNotFound)
	})
	go (&http.Server{Handler: mux}).Serve(ln)
}

func (s *server) sinkExport(signal string, w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err == nil {
		if fields, ok := sinkFields(signal, body); ok {
			s.log.emit("otlp", fields)
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// readBody is a request's body, gunzipped where it says it is gzipped.
func readBody(r *http.Request) ([]byte, error) {
	var in io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		in = z
	}
	return io.ReadAll(in)
}

// sinkFields is what the log says of an export: its signal, services, how many spans, metrics or
// log records it carried, each span's name (its URL, where it has one) and end (wall clock), and
// its probes. False when it doesn't decode.
func sinkFields(signal string, body []byte) (map[string]any, bool) {
	services := []string{}
	addService := func(attrs []*commonpb.KeyValue) {
		name := ""
		for _, kv := range attrs {
			if kv.Key == "service.name" {
				if v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
					name = v.StringValue
					break
				}
			}
		}
		for _, s := range services {
			if s == name {
				return
			}
		}
		services = append(services, name)
	}
	spans, ends, probes := []string{}, []float64{}, []map[string]any{}
	count := 0
	switch signal {
	case "traces":
		var req coltracepb.ExportTraceServiceRequest
		if proto.Unmarshal(body, &req) != nil {
			return nil, false
		}
		for _, rs := range req.ResourceSpans {
			addService(rs.GetResource().GetAttributes())
			for _, ss := range rs.ScopeSpans {
				count += len(ss.Spans)
				for _, sp := range ss.Spans {
					name := sp.Name
					if u, ok := urlOf(sp.Attributes); ok && u != nil {
						name = *u
					}
					spans = append(spans, name)
					ends = append(ends, float64(sp.EndTimeUnixNano)/1e9)
					for _, ev := range sp.Events {
						if u, ok := urlOf(ev.Attributes); ok || ev.Name == probeName {
							probes = append(probes, probe("event", u, ev.Attributes))
						}
					}
					for _, l := range sp.Links {
						u, _ := urlOf(l.Attributes)
						probes = append(probes, probe("link", u, l.Attributes))
					}
				}
			}
		}
	case "metrics":
		var req colmetricspb.ExportMetricsServiceRequest
		if proto.Unmarshal(body, &req) != nil {
			return nil, false
		}
		for _, rm := range req.ResourceMetrics {
			addService(rm.GetResource().GetAttributes())
			for _, sm := range rm.ScopeMetrics {
				count += len(sm.Metrics)
			}
		}
	default:
		var req collogspb.ExportLogsServiceRequest
		if proto.Unmarshal(body, &req) != nil {
			return nil, false
		}
		for _, rl := range req.ResourceLogs {
			addService(rl.GetResource().GetAttributes())
			for _, sl := range rl.ScopeLogs {
				count += len(sl.LogRecords)
				for _, rec := range sl.LogRecords {
					u, ok := urlOf(rec.Attributes)
					if ok || strings.Contains(rec.GetBody().GetStringValue(), probeName) {
						probes = append(probes, probe("log", u, rec.Attributes))
					}
				}
			}
		}
	}
	fields := map[string]any{"signal": signal, "services": services, "spans": spans, "ends_wall": ends, "count": count}
	if len(probes) > 0 {
		fields["probes"] = probes
	}
	return fields, true
}

// probeName: what the example applications name their probe (a span event, and a log record's
// body), which carries a URL (url.full) where it can be redacted.
const probeName = "example.probe"

// probeNote: the attribute the probe carries beside the URL, a string scrubbing's default
// patterns replace (it says password).
const probeNote = "example.note"

// urlOf is the URL among `attrs` (url.full or http.url): nil when the key is there with a value
// that isn't a string; false when neither key is.
func urlOf(attrs []*commonpb.KeyValue) (*string, bool) {
	return stringAttr(attrs, func(k string) bool { return k == "url.full" || k == "http.url" })
}

func stringAttr(attrs []*commonpb.KeyValue, match func(string) bool) (*string, bool) {
	for _, kv := range attrs {
		if match(kv.Key) {
			if v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
				return &v.StringValue, true
			}
			return nil, true
		}
	}
	return nil, false
}

// probe: where it was (event, link, log) and its URL, null once dropped; and its note, where the
// attributes have one.
func probe(place string, url *string, attrs []*commonpb.KeyValue) map[string]any {
	out := map[string]any{"in": place, "url": nil}
	if url != nil {
		out["url"] = *url
	}
	if note, ok := stringAttr(attrs, func(k string) bool { return k == probeNote }); ok {
		if note != nil {
			out["note"] = *note
		} else {
			out["note"] = nil
		}
	}
	return out
}

// described adds to an a2s line what the agent says of itself: its description, health,
// available components (when sent in full) and effective config (its JSON files, parsed).
func described(msg *protobufs.AgentToServer, ev map[string]any) {
	if d := msg.AgentDescription; d != nil {
		ev["description"] = map[string]any{"identifying": attrsJSON(d.IdentifyingAttributes),
			"non_identifying": attrsJSON(d.NonIdentifyingAttributes)}
	}
	if h := msg.Health; h != nil {
		ev["health"] = healthJSON(h)
	}
	if c := msg.AvailableComponents; c != nil && len(c.Components) > 0 {
		ev["components"] = componentsJSON(c.Components)
	}
	if e := effectiveJSON(msg.EffectiveConfig); e != nil {
		ev["effective"] = e
	}
}

// effectiveLogged: the most bytes of one JSON file of an effective config the log gives.
const effectiveLogged = 16 << 10

func effectiveJSON(e *protobufs.EffectiveConfig) map[string]any {
	files := map[string]any{}
	for name, f := range e.GetConfigMap().GetConfigMap() {
		if f.ContentType != "application/json" || len(f.Body) > effectiveLogged {
			continue
		}
		var v any
		if json.Unmarshal(f.Body, &v) == nil {
			files[name] = v
		}
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

func healthJSON(h *protobufs.ComponentHealth) map[string]any {
	out := map[string]any{"healthy": h.Healthy, "status": h.Status, "last_error": h.LastError}
	if len(h.ComponentHealthMap) > 0 {
		children := map[string]any{}
		for k, c := range h.ComponentHealthMap {
			children[k] = healthJSON(c)
		}
		out["components"] = children
	}
	if len(h.Attributes) > 0 {
		out["attributes"] = attrsJSON(h.Attributes)
	}
	return out
}

func componentsJSON(m map[string]*protobufs.ComponentDetails) map[string]any {
	out := map[string]any{}
	for name, d := range m {
		v := map[string]any{}
		if len(d.Metadata) > 0 {
			v["metadata"] = attrsJSON(d.Metadata)
		}
		if len(d.SubComponentMap) > 0 {
			v["components"] = componentsJSON(d.SubComponentMap)
		}
		out[name] = v
	}
	return out
}

func attrsJSON(kvs []*protobufs.KeyValue) map[string]any {
	out := map[string]any{}
	for _, kv := range kvs {
		out[kv.Key] = anyJSON(kv.Value)
	}
	return out
}

func anyJSON(v *protobufs.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *protobufs.AnyValue_StringValue:
		return x.StringValue
	case *protobufs.AnyValue_BoolValue:
		return x.BoolValue
	case *protobufs.AnyValue_IntValue:
		return x.IntValue
	case *protobufs.AnyValue_DoubleValue:
		return x.DoubleValue
	case *protobufs.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *protobufs.AnyValue_ArrayValue:
		out := []any{}
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyJSON(e))
		}
		return out
	case *protobufs.AnyValue_KvlistValue:
		return attrsJSON(x.KvlistValue.GetValues())
	}
	return nil
}

// fillTimes is `body` with each {now+Ns} replaced by the RFC 3339 time N seconds after `now`
// (rounded up to a whole second), and whether there was one.
func fillTimes(body string, now time.Time) (string, bool) {
	const open = "{now+"
	var out strings.Builder
	rest, filled := body, false
	for {
		before, after, found := strings.Cut(rest, open)
		out.WriteString(before)
		if !found {
			break
		}
		digits := 0
		for digits < len(after) && after[digits] >= '0' && after[digits] <= '9' {
			digits++
		}
		seconds, err := strconv.ParseUint(after[:digits], 10, 64)
		tail, ok := strings.CutPrefix(after[digits:], "s}")
		if err != nil || !ok {
			out.WriteString(open)
			rest = after
			continue
		}
		// Past 9999-12-31T23:59:59Z, that time: a four-digit year is all RFC 3339 has.
		const last = 253_402_300_799
		whole := now.Unix()
		if now.Nanosecond() > 0 {
			whole++
		}
		at := int64(last)
		if seconds < uint64(last-whole) {
			at = whole + int64(seconds)
		}
		out.WriteString(time.Unix(at, 0).UTC().Format("2006-01-02T15:04:05Z"))
		rest, filled = tail, true
	}
	return out.String(), filled
}
