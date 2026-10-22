// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package match_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	"debug_until", "debug_sampling_rate", "debug_send_logs_level", "scrub_defaults", "scrub_patterns"}}

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

// A vector's value (one of string, int, bool, double, array, map) as scrubbing leaves it. key is
// the attribute or member it is the value of; nil: a body or an array's item.
func scrubValue(s *match.Scrubbing, key *string, v map[string]any) map[string]any {
	out := map[string]any{}
	for k, x := range v {
		out[k] = x
	}
	if key != nil {
		switch what, with := s.Key(*key); what {
		case match.Keep:
			return out
		case match.Replace:
			return map[string]any{"key": *key, "string": with}
		}
	}
	switch {
	case v["string"] != nil:
		out["string"], _ = s.Text(v["string"].(string))
	case v["array"] != nil:
		var items []any
		for _, x := range v["array"].([]any) {
			if text, ok := x.(string); ok {
				x, _ = s.Text(text)
			}
			items = append(items, x)
		}
		out["array"] = items
	case v["map"] != nil:
		var members []any
		for _, m := range v["map"].([]any) {
			member := m.(map[string]any)
			k := member["key"].(string)
			members = append(members, scrubValue(s, &k, member))
		}
		out["map"] = members
	}
	return out
}

func TestScrubbingKeepsTheScrubVectors(t *testing.T) {
	v := vectors[struct {
		Cases []struct {
			Name       string           `json:"name"`
			Settings   json.RawMessage  `json:"settings"`
			Attributes []map[string]any `json:"attributes"`
			Want       []map[string]any `json:"want"`
			Body       map[string]any   `json:"body"`
			WantBody   map[string]any   `json:"want_body"`
		}
	}](t, "scrub.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			set := settings(t, c.Settings, 0)
			r, s := match.RedactionOf(set), match.ScrubbingOf(set)
			got := []map[string]any{}
			for _, a := range c.Attributes {
				key := a["key"].(string)
				switch drop, mask := r.Key(key); {
				case drop:
				case mask:
					got = append(got, map[string]any{"key": key, "string": match.Mask})
				default:
					got = append(got, scrubValue(s, &key, a))
				}
			}
			want := c.Want
			if want == nil {
				want = []map[string]any{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			if c.Body != nil {
				if body := scrubValue(s, nil, c.Body); !reflect.DeepEqual(body, c.WantBody) {
					t.Fatalf("body: got %v, want %v", body, c.WantBody)
				}
			}
		})
	}
	if len(v.Cases) < 30 {
		t.Fatalf("%d cases: scrub.json was cut short", len(v.Cases))
	}
}

// The default patterns at their edges, beyond the vectors.
func TestScrubbingText(t *testing.T) {
	s := &match.Scrubbing{Defaults: true}
	for _, c := range []struct{ text, want string }{
		{"the author wrote", "the author wrote"},
		{"the authors wrote", "the authors wrote"},
		{"authority figure", "[Scrubbed due to 'auth']"},
		{"my API-KEY here", "[Scrubbed due to 'API-KEY']"},
		{"apikey=1", "[Scrubbed due to 'apikey']"},
		{"tok pylf_v12_abc", "[Scrubbed due to 'pylf_v12_']"},
		{"tok pylf_v_abc", "tok pylf_v_abc"},
		{"a jwt here", "[Scrubbed due to 'jwt']"},
		{"my_jwt_x", "[Scrubbed due to '_jwt_']"},
		{"jwtoken", "jwtoken"},
		{"the Key: ſecret", "[Scrubbed due to 'ſecret']"},
		{"password", "password"},
		{"", ""},
	} {
		if got, _ := s.Text(c.text); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	if what, with := s.Key("db.statement"); what != match.Keep || with != "" {
		t.Errorf("a safe key: %v %q", what, with)
	}
	if what, with := s.Key("X-Session-Id"); what != match.Replace || with != "[Scrubbed due to 'Session']" {
		t.Errorf("a key a default matches: %v %q", what, with)
	}
	if what, _ := (&match.Scrubbing{}).Key("password"); what != match.Keep {
		t.Errorf("scrubbing off: %v", what)
	}
	p := &match.Scrubbing{Patterns: []string{"*iban*", "sk_live_*"}}
	if got, ok := p.Text("SK_LIVE_123"); !ok || got != "[Scrubbed due to 'sk_live_*']" {
		t.Errorf("a pattern on a value: %q %v", got, ok)
	}
	if got, ok := p.Text("an sk_live_1"); ok {
		t.Errorf("a pattern matches the whole value only: %q", got)
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
