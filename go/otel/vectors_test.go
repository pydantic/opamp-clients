// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/pydantic/opamp-clients/go/opamp/sdk"
	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// The shared vectors (testdata/sdk-settings), run through this package's own code:
// each case's settings document goes through the layer as this package's host declares it, the
// settings it hands over are applied to Controls, and the sampler or the gate decides.

const vectorNow = uint64(1_791_979_200_000) // 2026-10-14T12:00:00Z, the vectors' clock

var (
	layerOnce sync.Once
	theLayer  *sdk.Layer
	layerErr  error
)

// testLayer is one layer for the package's tests (the layer keeps nothing between calls).
func testLayer(t testing.TB) *sdk.Layer {
	t.Helper()
	layerOnce.Do(func() { theLayer, layerErr = sdk.New(context.Background()) })
	if layerErr != nil {
		t.Fatal(layerErr)
	}
	return theLayer
}

// goHost is the host this package declares (agent.go).
func goHost() *settingspb.Host { return (&server{}).host() }

// settingsOf is what the layer hands this package for a settings document at now (0: the
// vectors' clock), as at a start with the document saved.
func settingsOf(t testing.TB, doc []byte, now uint64) *settingspb.Settings {
	t.Helper()
	if now == 0 {
		now = vectorNow
	}
	loaded, err := testLayer(t).LoadAt(context.Background(), doc, goHost(), now)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GetWarning() != "" {
		t.Fatalf("the layer refused %s: %s", doc, loaded.GetWarning())
	}
	if set := append(loaded.GetSettings().GetIgnored(), loaded.GetSettings().GetUnsupported()...); len(set) > 0 {
		t.Fatalf("this package's host sets %v of %s aside: it should carry out every one", set, doc)
	}
	return loaded.GetSettings()
}

func readVectors[T any](t testing.TB, name string) T {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "sdk-settings", name)
	// OPAMP_SETTINGS_VECTORS: a directory of generated cases in place of testdata's, where it has the file.
	if other := os.Getenv("OPAMP_SETTINGS_VECTORS"); other != "" {
		if _, err := os.Stat(filepath.Join(other, name)); err == nil {
			path = filepath.Join(other, name)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type logCase struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Now      uint64          `json:"now_unix_ms"`
	Record   struct {
		Scope    string `json:"scope"`
		Severity int    `json:"severity"`
	} `json:"record"`
	Kept bool `json:"kept"`
}

// recordSink keeps what the gate exported.
type recordSink struct {
	mu  sync.Mutex
	got []sdklog.Record
	err error // what Export answers
}

func (s *recordSink) Export(_ context.Context, records []sdklog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		s.got = append(s.got, r.Clone())
	}
	return s.err
}

func (s *recordSink) ForceFlush(context.Context) error { return nil }
func (s *recordSink) Shutdown(context.Context) error   { return nil }

func (s *recordSink) take() []sdklog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.got
	s.got = nil
	return got
}

// A log record's way through the gate: send_logs, send_logs_level, send_logs_level_by_scope,
// debug mode's level (logs.json).
func TestTheLogVectors(t *testing.T) {
	v := readVectors[struct{ Cases []logCase }](t, "logs.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			controls.Apply(settingsOf(t, c.Settings, c.Now))
			sink := &recordSink{}
			lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(controls.Logs(sink))))
			var r log.Record
			r.SetSeverity(log.Severity(c.Record.Severity))
			lp.Logger(c.Record.Scope).Emit(context.Background(), r)
			if kept := len(sink.take()) == 1; kept != c.Kept {
				t.Fatalf("kept %v, want %v", kept, c.Kept)
			}
		})
	}
	if len(v.Cases) < 40 {
		t.Fatalf("%d cases: logs.json was cut short", len(v.Cases))
	}
}
