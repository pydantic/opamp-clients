// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package match_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// The shared vectors (testdata/sdk-settings) for the matchers alone: each case's policy goes
// through the layer for a host that carries out every control the vectors set, and the matcher
// judges what the case gives.

const vectorNow = uint64(1_791_979_200_000) // 2026-10-14T12:00:00Z, the vectors' clock

var host = &settingspb.Host{Live: []string{"sampling_rate", "sampling_rules", "send_traces", "send_metrics",
	"send_logs", "send_logs_level", "send_logs_level_by_scope", "drop_attributes", "mask_attributes",
	"debug_until", "debug_sampling_rate", "debug_send_logs_level"}}

func settings(t *testing.T, doc json.RawMessage, now uint64) *settingspb.Settings {
	t.Helper()
	l, err := sdksettings.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	if now == 0 {
		now = vectorNow
	}
	loaded, err := l.LoadAt(context.Background(), doc, host, now)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GetWarning() != "" {
		t.Fatalf("the layer refused %s: %s", doc, loaded.GetWarning())
	}
	return loaded.GetSettings()
}

func vectors[T any](t *testing.T, name string) T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "testdata", "sdk-settings", name))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLevelsKeepTheLogVectors(t *testing.T) {
	v := vectors[struct {
		Cases []struct {
			Name     string          `json:"name"`
			Settings json.RawMessage `json:"settings"`
			Now      uint64          `json:"now_unix_ms"`
			Record   struct {
				Scope    string `json:"scope"`
				Severity int    `json:"severity"`
			} `json:"record"`
			Kept bool `json:"kept"`
		}
	}](t, "logs.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			s := settings(t, c.Settings, c.Now)
			kept := (s.SendLogs == nil || *s.SendLogs) && match.LevelsOf(s).Keeps(c.Record.Scope, c.Record.Severity)
			if kept != c.Kept {
				t.Fatalf("kept %v, want %v", kept, c.Kept)
			}
		})
	}
	if len(v.Cases) < 40 {
		t.Fatalf("%d cases: logs.json was cut short", len(v.Cases))
	}
}

func TestRedactionKeepsTheRedactVectors(t *testing.T) {
	v := vectors[struct {
		Cases []struct {
			Name       string                       `json:"name"`
			Settings   json.RawMessage              `json:"settings"`
			Attributes []map[string]json.RawMessage `json:"attributes"`
			Want       []map[string]json.RawMessage `json:"want"`
		}
	}](t, "redact.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			r := match.RedactionOf(settings(t, c.Settings, 0))
			var got []map[string]json.RawMessage
			for _, a := range c.Attributes {
				var key string
				if err := json.Unmarshal(a["key"], &key); err != nil {
					t.Fatal(err)
				}
				drop, mask := r.Key(key)
				switch {
				case drop:
				case mask:
					masked, _ := json.Marshal(match.Mask)
					got = append(got, map[string]json.RawMessage{"key": a["key"], "string": masked})
				default:
					got = append(got, a)
				}
			}
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(c.Want)
			if len(got) == 0 && len(c.Want) == 0 {
				return
			}
			if string(g) != string(w) {
				t.Fatalf("got %s, want %s", g, w)
			}
		})
	}
	if len(v.Cases) < 20 {
		t.Fatalf("%d cases: redact.json was cut short", len(v.Cases))
	}
}

// Glob: the cases the layer's docs give, by characters and ASCII case only.
func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, text string
		fold, want    bool
	}{
		{"*", "", false, true},
		{"", "", false, true},
		{"", "a", false, false},
		{"a*b*c", "aXbYbZc", false, true},
		{"a*b", "ab", false, true},
		{"*ab", "aab", false, true},
		{"a?", "ab", false, false},
		{"A*", "abc", false, false},
		{"A*", "abc", true, true},
		{"É", "é", true, false},
		{"é*😀", "éx😀", false, true},
	} {
		if got := match.Glob(c.pattern, c.text, c.fold); got != c.want {
			t.Errorf("Glob(%q, %q, %v) = %v, want %v", c.pattern, c.text, c.fold, got, c.want)
		}
	}
}
