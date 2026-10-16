// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"encoding/json"
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
	c.Apply(settingsOf(t, []byte(levelsPolicy("warn", "com.acme.db*=debug", "noisy.lib=off")), 0))
	if got, want := all(), "com.acme.db:5 com.acme.db:13 com.acme.dbpool:5 com.acme.dbpool:13 app:13"; got != want {
		t.Fatalf("levels per scope: %s, want %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(levelsPolicy("warn", "app=error")), 0))
	if got, want := all(), "com.acme.db:13 com.acme.dbpool:13 noisy.lib:13"; got != want {
		t.Fatalf("changed: %s, want %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(levelsPolicy("warn")), 0))
	if got, want := all(), "com.acme.db:13 com.acme.dbpool:13 noisy.lib:13 app:13"; got != want {
		t.Fatalf("levels per scope left out: %s, want the global level's %s", got, want)
	}
	c.Apply(settingsOf(t, []byte(v2()), 0))
	if got := all(); got != every {
		t.Fatalf("everything left out: %s, want every record", got)
	}
}

// levelsPolicy is a policy whose log_levels control is the given level and scope entries, each
// "pattern=level"; with neither, the control is local.
func levelsPolicy(level string, entries ...string) string {
	return v2("log_levels", logLevelsControl(level, entries...))
}

// levelsPolicyOff is levelsPolicy with no level, the given scope entries and the log export off.
func levelsPolicyOff(entries ...string) string {
	return v2("signal_exports", exports("", "", "off"), "log_levels", logLevelsControl("", entries...))
}

func logLevelsControl(level string, entries ...string) string {
	if level == "" && len(entries) == 0 {
		return `{"mode":"local"}`
	}
	var fields []string
	if level != "" {
		fields = append(fields, `"level":"`+level+`"`)
	}
	if len(entries) > 0 {
		var scopes []string
		for _, e := range entries {
			pattern, lv, _ := strings.Cut(e, "=")
			key, _ := json.Marshal(pattern)
			scopes = append(scopes, string(key)+`:"`+lv+`"`)
		}
		fields = append(fields, `"scopes":{`+strings.Join(scopes, ",")+`}`)
	}
	return override(strings.Join(fields, ","))
}

// The spec's edge cases, through the gate.
func TestScopeLevelEdgeCases(t *testing.T) {
	for _, c := range []struct {
		name, doc, scope string
		sev              log.Severity
		kept             bool
	}{
		{"exact beats prefix", levelsPolicy("", "a.b*=error", "a.b=debug"), "a.b", log.SeverityDebug, true},
		{"the longer prefix wins", levelsPolicy("", "a*=debug", "a.b*=error"), "a.b.c", log.SeverityWarn, false},
		{"the shorter prefix where the longer does not match", levelsPolicy("", "a*=debug", "a.b*=error"), "a.c", log.SeverityDebug, true},
		{"the empty scope: * matches it", levelsPolicy("", "*=error"), "", log.SeverityWarn, false},
		{"the empty scope: only *", levelsPolicy("", "a*=off"), "", log.SeverityWarn, true},
		{"severity 0 under fatal is kept", levelsPolicy("", "app=fatal"), "app", 0, true},
		{"severity 0 under off is dropped", levelsPolicy("", "app=off"), "app", 0, false},
		{"off drops fatal too", levelsPolicy("", "app=off"), "app", log.SeverityFatal4, false},
		{"a scope entry without send_logs_level", levelsPolicy("", "app=error"), "app", log.SeverityWarn, false},
		{"other scopes without send_logs_level: every record", levelsPolicy("", "app=error"), "other", log.SeverityTrace, true},
		{"a scope's level lower than the global one", levelsPolicy("error", "app=trace"), "app", log.SeverityTrace, true},
		{"send_logs false overrides every level", levelsPolicyOff("app=trace"), "app", log.SeverityFatal, false},
		{"scope names match case-sensitively", levelsPolicy("", "App=off"), "app", log.SeverityInfo, true},
		{"a prefix is by characters, not dotted segments", levelsPolicy("", "com.acme*=off"), "com.acmesoft", log.SeverityInfo, false},
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

// A host that cannot carry out send_logs_level cannot carry out the levels per scope either: the
// layer refuses a policy that overrides them, sets them aside in a saved policy (reported local),
// and the gate keeps every record.
func TestScopeLevelsAreRefusedWhereTheLevelIs(t *testing.T) {
	host := goHost()
	host.Live = slices.DeleteFunc(host.Live, func(k string) bool { return k == "send_logs_level" })
	doc := levelsPolicy("", "app=off")
	d, err := testLayer(t).DecideAt(context.Background(), remote(doc), nil, nil, host, vectorNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.GetStatus().GetStatus() != failed_ || d.GetApply() != nil ||
		d.GetStatus().GetErrorMessage() != "log_levels: scopes: this SDK cannot apply the requested override" {
		t.Fatalf("decided %v", d)
	}
	loaded, err := testLayer(t).LoadAt(context.Background(), []byte(doc), host, vectorNow)
	effective := string(loaded.GetEffective().GetConfigMap().GetConfigMap()[policyFile].GetBody())
	if err != nil || loaded.GetWarning() != "" || !strings.Contains(effective, `"log_levels":{"mode":"local"}`) {
		t.Fatalf("loaded %v %v: a saved policy's levels this host cannot apply should be set aside", loaded, err)
	}
	c := NewControls()
	c.Apply(loaded.GetSettings())
	sink := &recordSink{}
	emit(gatedLogs(c, sink), "app", log.SeverityInfo)
	if len(sink.take()) != 1 {
		t.Fatal("a refused level dropped a record")
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
		level := ""
		if r.IntN(2) == 0 {
			level = levels[r.IntN(6)]
		}
		doc := levelsPolicy(level, entries...)
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
