// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// gatedLogs is a logger provider whose only exporter is c's gate around sink.
func gatedLogs(c *Controls, sink sdklog.Exporter) *sdklog.LoggerProvider {
	return sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(c.Logs(sink))))
}

func emit(lp *sdklog.LoggerProvider, scope string, sev log.Severity) {
	var r log.Record
	r.SetSeverity(sev)
	r.SetBody(attribute.StringValue(scope))
	lp.Logger(scope).Emit(context.Background(), r)
}

// exported lists the records the sink got since the last call, as scope:severity.
func exported(s *recordSink) string {
	var out []string
	for _, r := range s.take() {
		out = append(out, fmt.Sprintf("%s:%d", r.InstrumentationScope().Name, r.Severity()))
	}
	return strings.Join(out, " ")
}

// The levels per scope change while records are made, and leaving them out restores the global
// level at once: the same logger provider, the same loggers.
func TestScopeLevelsChangeLiveAndRevert(t *testing.T) {
	c := NewControls()
	sink := &recordSink{}
	lp := gatedLogs(c, sink)
	all := func() string {
		for _, scope := range []string{"com.acme.db", "com.acme.dbpool", "noisy.lib", "app"} {
			emit(lp, scope, log.SeverityDebug)
			emit(lp, scope, log.SeverityWarn)
		}
		return exported(sink)
	}
	every := "com.acme.db:5 com.acme.db:13 com.acme.dbpool:5 com.acme.dbpool:13 noisy.lib:5 noisy.lib:13 app:5 app:13"
	if got := all(); got != every {
		t.Fatalf("nothing set: %s", got)
	}
	c.Apply(settingsOf(t, []byte(`{"send_logs_level":"warn","send_logs_level_by_scope":"com.acme.db*=debug, noisy.lib=off"}`), 0))
	if got, want := all(), "com.acme.db:5 com.acme.db:13 com.acme.dbpool:5 com.acme.dbpool:13 app:13"; got != want {
		t.Fatalf("levels per scope: %s, want %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(`{"send_logs_level":"warn","send_logs_level_by_scope":"app=error"}`), 0))
	if got, want := all(), "com.acme.db:13 com.acme.dbpool:13 noisy.lib:13"; got != want {
		t.Fatalf("changed: %s, want %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(`{"send_logs_level":"warn"}`), 0))
	if got, want := all(), "com.acme.db:13 com.acme.dbpool:13 noisy.lib:13 app:13"; got != want {
		t.Fatalf("levels per scope left out: %s, want the global level's %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(`{}`), 0))
	if got := all(); got != every {
		t.Fatalf("everything left out: %s, want every record", got)
	}
}

// The spec's edge cases, through the gate.
func TestScopeLevelEdgeCases(t *testing.T) {
	for _, c := range []struct {
		name, doc, scope string
		sev              log.Severity
		kept             bool
	}{
		{"exact beats prefix", `{"send_logs_level_by_scope":"a.b*=error,a.b=debug"}`, "a.b", log.SeverityDebug, true},
		{"the longer prefix wins", `{"send_logs_level_by_scope":"a*=debug,a.b*=error"}`, "a.b.c", log.SeverityWarn, false},
		{"the shorter prefix where the longer doesn't match", `{"send_logs_level_by_scope":"a*=debug,a.b*=error"}`, "a.c", log.SeverityDebug, true},
		{"the empty scope: * matches it", `{"send_logs_level_by_scope":"*=error"}`, "", log.SeverityWarn, false},
		{"the empty scope: only *", `{"send_logs_level_by_scope":"a*=off"}`, "", log.SeverityWarn, true},
		{"severity 0 under fatal is kept", `{"send_logs_level_by_scope":"app=fatal"}`, "app", 0, true},
		{"severity 0 under off is dropped", `{"send_logs_level_by_scope":"app=off"}`, "app", 0, false},
		{"off drops fatal too", `{"send_logs_level_by_scope":"app=off"}`, "app", log.SeverityFatal4, false},
		{"a scope entry without send_logs_level", `{"send_logs_level_by_scope":"app=error"}`, "app", log.SeverityWarn, false},
		{"other scopes without send_logs_level: every record", `{"send_logs_level_by_scope":"app=error"}`, "other", log.SeverityTrace, true},
		{"a scope's level lower than the global one", `{"send_logs_level":"error","send_logs_level_by_scope":"app=trace"}`, "app", log.SeverityTrace, true},
		{"send_logs false overrides every level", `{"send_logs":"false","send_logs_level_by_scope":"app=trace"}`, "app", log.SeverityFatal, false},
		{"scope names match case-sensitively", `{"send_logs_level_by_scope":"App=off"}`, "app", log.SeverityInfo, true},
		{"a prefix is by characters, not dotted segments", `{"send_logs_level_by_scope":"com.acme*=off"}`, "com.acmesoft", log.SeverityInfo, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			controls := NewControls()
			controls.Apply(settingsOf(t, []byte(c.doc), 0))
			sink := &recordSink{}
			emit(gatedLogs(controls, sink), c.scope, c.sev)
			if kept := len(sink.take()) == 1; kept != c.kept {
				t.Fatalf("kept %v, want %v", kept, c.kept)
			}
		})
	}
}

// A host that can't carry out send_logs_level can't carry out the levels per scope either: the
// layer ignores both, and the gate keeps every record.
func TestScopeLevelsAreIgnoredWhereTheLevelIs(t *testing.T) {
	host := goHost()
	host.Live = slices.DeleteFunc(host.Live, func(k string) bool { return k == "send_logs_level" })
	loaded, err := testLayer(t).LoadAt(context.Background(), []byte(`{"send_logs_level_by_scope":"app=off"}`), host, vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(loaded.GetSettings().GetUnsupported()) != "[send_logs_level_by_scope]" {
		t.Fatalf("unsupported %v", loaded.GetSettings().GetUnsupported())
	}
	c := NewControls()
	c.Apply(loaded.GetSettings())
	sink := &recordSink{}
	emit(gatedLogs(c, sink), "app", log.SeverityInfo)
	if len(sink.take()) != 1 {
		t.Fatal("an ignored level dropped a record")
	}
}

// The gate's scope matching against the layer's (Evaluate), on generated scopes and entries.
func TestScopeLevelsMatchTheLayers(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	word := func() string {
		const letters = "ab.é"
		n := r.IntN(4)
		var b strings.Builder
		for range n {
			b.WriteRune([]rune(letters)[r.IntN(4)])
		}
		return b.String()
	}
	levels := []string{"trace", "debug", "info", "warn", "error", "fatal", "off"}
	for i := 0; i < 300; i++ {
		seen := map[string]bool{}
		var entries []string
		for range 1 + r.IntN(4) {
			p := word()
			if r.IntN(2) == 0 {
				p += "*"
			}
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			entries = append(entries, p+"="+levels[r.IntN(len(levels))])
		}
		doc := fmt.Sprintf(`{"send_logs_level_by_scope":%q}`, strings.Join(entries, ","))
		if r.IntN(2) == 0 {
			doc = fmt.Sprintf(`{"send_logs_level":%q,"send_logs_level_by_scope":%q}`, levels[r.IntN(6)], strings.Join(entries, ","))
		}
		if len(entries) == 0 {
			doc = `{}`
		}
		scope, sev := word(), r.IntN(25)
		want, err := testLayer(t).Evaluate(context.Background(), &settingspb.Evaluate{Settings: []byte(doc), Host: goHost(),
			NowUnixMs: vectorNow, Input: &settingspb.Evaluate_Log{Log: &settingspb.LogRecord{Scope: scope, Severity: uint32(sev)}}})
		if err != nil || want.GetRefused() != "" {
			t.Fatalf("%s: %v %v", doc, want, err)
		}
		c := NewControls()
		c.Apply(settingsOf(t, []byte(doc), 0))
		sink := &recordSink{}
		emit(gatedLogs(c, sink), scope, log.Severity(sev))
		if kept := len(sink.take()) == 1; kept != want.GetKept() {
			t.Fatalf("%s, scope %q severity %d: the gate kept %v, the layer %v", doc, scope, sev, kept, want.GetKept())
		}
	}
}
