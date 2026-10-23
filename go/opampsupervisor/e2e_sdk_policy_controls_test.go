// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Every control the logfirepolicy processor carries out, end to end through the Supervisor, on
// traces, metrics and logs a collector receives over OTLP: the description lists exactly those
// controls; log_levels scopes and attribute_redaction (on span, span event, span link and log
// record attributes, never metrics') apply; each signal_exports switch stops its signal and only
// it; and a policy that overrides head_sampling or scrubbing is FAILED naming it, the collector
// going on with the policy it ran.
//
// Each step's collector listens on a port of its own, so telemetry sent to it is the new
// collector's alone, and writes to files of its own.
func TestSupervisorCollectorCarriesOutEachPolicyControl(t *testing.T) {
	var status atomic.Pointer[protobufs.RemoteConfigStatus]
	var description atomic.Pointer[protobufs.AgentDescription]
	server := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if m.RemoteConfigStatus != nil {
				status.Store(m.RemoteConfigStatus)
			}
			if m.AgentDescription != nil {
				description.Store(m.AgentDescription)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	s, _ := newSupervisor(t, "sdk_policy", map[string]string{"url": server.addr, "storage_dir": t.TempDir()})
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	waitForSupervisorConnection(server.supervisorConnected, true)

	// The controls the server may override: the processor's, and since #77 not head_sampling.
	var controls []string
	require.Eventually(t, func() bool {
		for _, kv := range description.Load().GetNonIdentifyingAttributes() {
			if kv.GetKey() == "logfire.opamp.supported_controls" {
				controls = controls[:0]
				for _, v := range kv.GetValue().GetArrayValue().GetValues() {
					controls = append(controls, v.GetStringValue())
				}
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "the description never listed the supported controls")
	require.Equal(t, []string{"signal_exports.traces", "signal_exports.metrics", "signal_exports.logs", "log_levels",
		"attribute_redaction"}, controls)

	dir := t.TempDir()
	type step struct {
		port int
		out  map[string]string // signal -> the file its exporter writes
	}
	collectorFor := func(name string) (step, string) {
		st := step{port: freePort(t), out: map[string]string{}}
		var exporters, pipelines strings.Builder
		for _, signal := range []string{"traces", "metrics", "logs"} {
			st.out[signal] = filepath.Join(dir, name+"-"+signal+".json")
			fmt.Fprintf(&exporters, "  file/%s:\n    path: %q\n    flush_interval: 100ms\n", signal, st.out[signal])
			fmt.Fprintf(&pipelines, "    %s: {receivers: [otlp], exporters: [file/%s]}\n", signal, signal)
		}
		return st, fmt.Sprintf("receivers:\n  otlp:\n    protocols:\n      http:\n        endpoint: 127.0.0.1:%d\n"+
			"exporters:\n%sservice:\n  pipelines:\n%s", st.port, exporters.String(), pipelines.String())
	}
	send := func(collector, policy string) []byte {
		files := map[string]*protobufs.AgentConfigObject{
			"collector.yaml":          {Body: []byte(collector)},
			"logfire-sdk-policy.json": {Body: []byte(policy), ContentType: "application/json"},
		}
		h := sha256.Sum256([]byte(collector + policy))
		server.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
			Config: &protobufs.AgentConfigMap{ConfigMap: files}, ConfigHash: h[:]}})
		return h[:]
	}
	waitStatus := func(hash []byte, want protobufs.RemoteConfigStatuses) *protobufs.RemoteConfigStatus {
		var got *protobufs.RemoteConfigStatus
		require.Eventually(t, func() bool {
			got = status.Load()
			return got != nil && bytes.Equal(got.LastRemoteConfigHash, hash) && got.Status == want
		}, 60*time.Second, 100*time.Millisecond, "never %s: %v", want, status.Load())
		return got
	}
	policy := func(controls ...string) string {
		given := map[string]string{
			"signal_exports": `{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}}`,
		}
		for i := 0; i+1 < len(controls); i += 2 {
			given[controls[i]] = controls[i+1]
		}
		b := `{"schema":"logfire-sdk-policy/v1","head_sampling":` + cmp.Or(given["head_sampling"], `{"mode":"local"}`) +
			`,"signal_exports":` + given["signal_exports"]
		for _, c := range []string{"log_levels", "attribute_redaction", "scrubbing"} {
			if v, ok := given[c]; ok {
				b += `,"` + c + `":` + v
			}
		}
		return b + "}"
	}

	// One round of telemetry, marked with `mark` (a resource attribute), posted to the step's
	// collector until it takes it (it may still be starting).
	post := func(st step, mark string) {
		res := func(r pcommon.Resource) { r.Attributes().PutStr("e2e.mark", mark) }
		secrets := func(m pcommon.Map) {
			m.PutStr("secret", "s")
			m.PutStr("api_token", "t")
			m.PutStr("user", "u")
		}
		td := ptrace.NewTraces()
		rs := td.ResourceSpans().AppendEmpty()
		res(rs.Resource())
		span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
		span.SetName("checkout")
		span.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
		span.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, 8})
		now := pcommon.NewTimestampFromTime(time.Now())
		span.SetStartTimestamp(now)
		span.SetEndTimestamp(now)
		secrets(span.Attributes())
		ev := span.Events().AppendEmpty()
		ev.SetName("probe")
		secrets(ev.Attributes())
		link := span.Links().AppendEmpty()
		link.SetTraceID(pcommon.TraceID{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
		link.SetSpanID(pcommon.SpanID{8, 7, 6, 5, 4, 3, 2, 1})
		secrets(link.Attributes())

		md := pmetric.NewMetrics()
		rm := md.ResourceMetrics().AppendEmpty()
		res(rm.Resource())
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("requests")
		dp := m.SetEmptySum().DataPoints().AppendEmpty()
		dp.SetIntValue(1)
		dp.SetTimestamp(now)
		dp.Attributes().PutStr("api_token", "t")

		ld := plog.NewLogs()
		rl := ld.ResourceLogs().AppendEmpty()
		res(rl.Resource())
		for _, scope := range []string{"app", "app.noisy"} {
			sl := rl.ScopeLogs().AppendEmpty()
			sl.Scope().SetName(scope)
			lr := sl.LogRecords().AppendEmpty()
			lr.SetSeverityNumber(plog.SeverityNumberInfo)
			lr.Body().SetStr("from " + scope)
			secrets(lr.Attributes())
		}

		bodies := map[string]func() ([]byte, error){
			"traces":  func() ([]byte, error) { return (&ptrace.JSONMarshaler{}).MarshalTraces(td) },
			"metrics": func() ([]byte, error) { return (&pmetric.JSONMarshaler{}).MarshalMetrics(md) },
			"logs":    func() ([]byte, error) { return (&plog.JSONMarshaler{}).MarshalLogs(ld) },
		}
		for signal, body := range bodies {
			b, err := body()
			require.NoError(t, err)
			url := fmt.Sprintf("http://127.0.0.1:%d/v1/%s", st.port, signal)
			require.Eventually(t, func() bool {
				resp, err := http.Post(url, "application/json", bytes.NewReader(b))
				if err != nil {
					return false
				}
				resp.Body.Close()
				return resp.StatusCode == http.StatusOK
			}, 60*time.Second, 200*time.Millisecond, "the collector never took the %s", signal)
		}
	}

	// What a step's exporters wrote, of the round marked `mark`.
	type seen struct {
		spans   []ptrace.Span
		points  []pmetric.NumberDataPoint
		records map[string][]plog.LogRecord // by scope
	}
	read := func(st step, mark string) seen {
		got := seen{records: map[string][]plog.LogRecord{}}
		marked := func(r pcommon.Resource) bool {
			v, ok := r.Attributes().Get("e2e.mark")
			return ok && v.Str() == mark
		}
		lines := func(signal string) []string {
			b, err := os.ReadFile(st.out[signal])
			if err != nil {
				return nil
			}
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		for _, line := range lines("traces") {
			td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces([]byte(line))
			if err != nil {
				continue
			}
			for i := range td.ResourceSpans().Len() {
				rs := td.ResourceSpans().At(i)
				for j := range rs.ScopeSpans().Len() {
					for k := range rs.ScopeSpans().At(j).Spans().Len() {
						if marked(rs.Resource()) {
							got.spans = append(got.spans, rs.ScopeSpans().At(j).Spans().At(k))
						}
					}
				}
			}
		}
		for _, line := range lines("metrics") {
			md, err := (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics([]byte(line))
			if err != nil {
				continue
			}
			for i := range md.ResourceMetrics().Len() {
				rm := md.ResourceMetrics().At(i)
				for j := range rm.ScopeMetrics().Len() {
					for k := range rm.ScopeMetrics().At(j).Metrics().Len() {
						dps := rm.ScopeMetrics().At(j).Metrics().At(k).Sum().DataPoints()
						for p := range dps.Len() {
							if marked(rm.Resource()) {
								got.points = append(got.points, dps.At(p))
							}
						}
					}
				}
			}
		}
		for _, line := range lines("logs") {
			ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs([]byte(line))
			if err != nil {
				continue
			}
			for i := range ld.ResourceLogs().Len() {
				rl := ld.ResourceLogs().At(i)
				for j := range rl.ScopeLogs().Len() {
					sl := rl.ScopeLogs().At(j)
					for k := range sl.LogRecords().Len() {
						if marked(rl.Resource()) {
							got.records[sl.Scope().Name()] = append(got.records[sl.Scope().Name()], sl.LogRecords().At(k))
						}
					}
				}
			}
		}
		return got
	}
	// waitFor reads a step's files until `ready` holds of what arrived of the round `mark`.
	waitFor := func(st step, mark, what string, ready func(seen) bool) seen {
		var got seen
		require.Eventually(t, func() bool {
			got = read(st, mark)
			return ready(got)
		}, 60*time.Second, 200*time.Millisecond, "never: %s", what)
		return got
	}
	// settled: a second round, sent after the first arrived, has arrived too, so anything the
	// collector was going to export of the first has been written (one exporter per signal,
	// each in order).
	settled := func(st step, mark string, ready func(seen) bool) seen {
		post(st, mark+"-after")
		waitFor(st, mark+"-after", "the second round of "+mark, ready)
		return read(st, mark)
	}
	redacted := func(what string, m pcommon.Map) {
		raw := m.AsRaw()
		require.NotContains(t, raw, "secret", "%s: secret dropped", what)
		require.Equal(t, "[REDACTED]", raw["api_token"], "%s: api_token masked", what)
		require.Equal(t, "u", raw["user"], "%s: user kept", what)
	}

	// 1. log_levels scopes (app.noisy off) and attribute_redaction (drop secret, mask *token*).
	one, yaml := collectorFor("one")
	waitStatus(send(yaml, policy(
		"log_levels", `{"mode":"override","scopes":{"app.noisy":"off"}}`,
		"attribute_redaction", `{"mode":"override","drop":["secret"],"mask":["*TOKEN*"]}`)),
		protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
	post(one, "one")
	all := func(g seen) bool { return len(g.spans) > 0 && len(g.points) > 0 && len(g.records["app"]) > 0 }
	waitFor(one, "one", "step one's spans, points and app's records", all)
	got := settled(one, "one", all)
	require.Len(t, got.spans, 1)
	redacted("the span", got.spans[0].Attributes())
	require.Equal(t, 1, got.spans[0].Events().Len())
	redacted("the span event", got.spans[0].Events().At(0).Attributes())
	require.Equal(t, 1, got.spans[0].Links().Len())
	redacted("the span link", got.spans[0].Links().At(0).Attributes())
	require.Len(t, got.records["app"], 1)
	redacted("the log record", got.records["app"][0].Attributes())
	require.Empty(t, got.records["app.noisy"], "log_levels scopes: app.noisy off")
	require.Len(t, got.points, 1)
	require.Equal(t, "t", got.points[0].Attributes().AsRaw()["api_token"], "metrics are never redacted")

	// 2. signal_exports traces and logs off: metrics alone go on.
	two, yaml := collectorFor("two")
	waitStatus(send(yaml, policy("signal_exports",
		`{"traces":{"mode":"override","enabled":false},"metrics":{"mode":"local"},"logs":{"mode":"override","enabled":false}}`)),
		protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
	post(two, "two")
	points := func(g seen) bool { return len(g.points) > 0 }
	waitFor(two, "two", "step two's points", points)
	got = settled(two, "two", points)
	require.Empty(t, got.spans, "signal_exports.traces off")
	require.Empty(t, got.records["app"], "signal_exports.logs off")
	require.Empty(t, got.records["app.noisy"], "signal_exports.logs off")
	require.Len(t, got.points, 1, "metrics go on")

	// 3. signal_exports metrics off, traces on (an override of true), logs local: the policy
	// before is gone, so the noisy scope's records come again and nothing is redacted.
	three, yaml := collectorFor("three")
	applied := policy("signal_exports",
		`{"traces":{"mode":"override","enabled":true},"metrics":{"mode":"override","enabled":false},"logs":{"mode":"local"}}`)
	waitStatus(send(yaml, applied), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
	post(three, "three")
	tracesAndLogs := func(g seen) bool {
		return len(g.spans) > 0 && len(g.records["app"]) > 0 && len(g.records["app.noisy"]) > 0
	}
	waitFor(three, "three", "step three's spans and both scopes' records", tracesAndLogs)
	got = settled(three, "three", tracesAndLogs)
	require.Empty(t, got.points, "signal_exports.metrics off")
	require.Equal(t, "s", got.spans[0].Attributes().AsRaw()["secret"], "redaction local again")

	// 4. Controls a collector doesn't carry out: FAILED, naming the control, and the collector
	// goes on with step three's policy.
	for _, c := range []struct{ control, value string }{
		{"head_sampling", `{"mode":"override","ratio":0.5}`},
		{"scrubbing", `{"mode":"override","defaults":true}`},
	} {
		refused := waitStatus(send(yaml, policy(c.control, c.value)), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
		require.True(t, strings.HasPrefix(refused.ErrorMessage, c.control+": "), "%s: %q", c.control, refused.ErrorMessage)
		require.Contains(t, refused.ErrorMessage, "cannot apply the requested override")
	}
	post(three, "four")
	waitFor(three, "four", "spans and records after the refusals", tracesAndLogs)
	got = settled(three, "four", tracesAndLogs)
	require.Empty(t, got.points, "still step three's policy: metrics off")
	require.True(t, slices.ContainsFunc(got.spans, func(s ptrace.Span) bool { return s.Name() == "checkout" }))
}
