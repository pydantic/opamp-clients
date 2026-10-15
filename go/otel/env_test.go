// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestServerFromEnv(t *testing.T) {
	if s := serverFromEnv(env(nil)); s != nil {
		t.Fatal("no endpoint: off")
	}
	s := serverFromEnv(env(map[string]string{
		"OTEL_OPAMP_ENDPOINT":           " https://opamp.example.com/v1/opamp ",
		"OTEL_OPAMP_HEADERS":            "Authorization=Bearer%20k1, X-Tenant = acme ,broken",
		"OTEL_OPAMP_STATE_DIR":          "/var/lib/checkout",
		"OTEL_OPAMP_HEARTBEAT_INTERVAL": "2m",
	}))
	if s.endpoint != "https://opamp.example.com/v1/opamp" || s.stateDir != "/var/lib/checkout" ||
		s.heartbeat != 2*time.Minute || s.header.Get("Authorization") != "Bearer k1" || s.header.Get("X-Tenant") != "acme" ||
		len(s.header) != 2 {
		t.Fatalf("%+v", s)
	}
	if s := serverFromEnv(env(map[string]string{"OTEL_OPAMP_ENDPOINT": "x", "OTEL_OPAMP_HEARTBEAT_INTERVAL": "soon"})); s.heartbeat != 0 {
		t.Fatal("a bad interval: the client's default")
	}
	// 307445735 minutes is more nanoseconds than an int64 holds: multiplied out, it wrapped to 26 s.
	if s := serverFromEnv(env(map[string]string{"OTEL_OPAMP_ENDPOINT": "x", "OTEL_OPAMP_HEARTBEAT_INTERVAL": "307445735m"})); s.heartbeat != 0 {
		t.Fatalf("an interval out of range: the client's default, not %v", s.heartbeat)
	}
}

// shown is what serverFromEnv made of one setting, as text: "off" without a server, "default"
// for the default state directory under home.
func shown(s *server, setting, home string) string {
	switch {
	case s == nil:
		return "off"
	case setting == "endpoint":
		return s.endpoint
	case setting == "headers":
		var kv []string
		for k, vs := range s.header {
			kv = append(kv, k+"="+strings.Join(vs, "|"))
		}
		sort.Strings(kv)
		return strings.Join(kv, ";")
	case setting == "state dir" && s.stateLock != nil && strings.HasPrefix(s.stateDir, home):
		return "default"
	case setting == "state dir":
		return s.stateDir
	default:
		return s.heartbeat.String()
	}
}

// Each option against its variable: neither is the default, either alone is that one, both is
// the option's; an option left blank is as if not given.
func TestEachOptionWinsOverItsVariable(t *testing.T) {
	settings := []struct {
		name, variable, inEnv        string
		inCode, blank                Option
		byDefault, fromEnv, fromCode string
	}{
		{"endpoint", "OTEL_OPAMP_ENDPOINT", " https://env.example.com/v1/opamp ",
			WithEndpoint(" wss://code.example.com/v1/opamp "), WithEndpoint("  "),
			"off", "https://env.example.com/v1/opamp", "wss://code.example.com/v1/opamp"},
		{"headers", "OTEL_OPAMP_HEADERS", "Authorization=Bearer%20env,X-Env=1",
			WithHeaders(map[string]string{"authorization": "Bearer code", "X-Tenant": "a%20b"}), WithHeaders(map[string]string{}),
			"", "Authorization=Bearer env;X-Env=1", "Authorization=Bearer code;X-Tenant=a%20b"},
		{"state dir", "OTEL_OPAMP_STATE_DIR", "/var/lib/env",
			WithStateDir("/var/lib/code"), WithStateDir(" "),
			"default", "/var/lib/env", "/var/lib/code"},
		{"heartbeat", "OTEL_OPAMP_HEARTBEAT_INTERVAL", "2m",
			WithHeartbeatInterval(5 * time.Second), WithHeartbeatInterval(0),
			"0s", "2m0s", "5s"},
	}
	for _, set := range settings {
		for _, c := range []struct {
			name      string
			env, code bool
			blank     bool
			want      string
		}{
			{"neither", false, false, false, set.byDefault},
			{"variable only", true, false, false, set.fromEnv},
			{"option only", false, true, false, set.fromCode},
			{"both: the option", true, true, false, set.fromCode},
			{"blank option and the variable: the variable", true, false, true, set.fromEnv},
			{"blank option alone: the default", false, false, true, set.byDefault},
		} {
			t.Run(set.name+"/"+c.name, func(t *testing.T) {
				home := t.TempDir()
				vars := stateHome(home)
				if set.name != "endpoint" {
					vars["OTEL_OPAMP_ENDPOINT"] = "https://base.example.com/v1/opamp"
				}
				if c.env {
					vars[set.variable] = set.inEnv
				}
				var opts []Option
				if c.code {
					opts = append(opts, set.inCode)
				}
				if c.blank {
					opts = append(opts, set.blank)
				}
				said := records(t)
				s := serverFromEnv(env(vars), opts...)
				defer released(s)
				if got := shown(s, set.name, home); got != c.want {
					t.Fatalf("got %q, want %q", got, c.want)
				}
				if len(*said) != 0 {
					t.Fatalf("nothing to warn about; said: %q", *said)
				}
			})
		}
	}
}

// The options, given at once, each win over its variable; the variables not given in code still
// apply; given twice, the last one given counts, a blank one counting for nothing.
func TestOptionsTogether(t *testing.T) {
	vars := env(map[string]string{
		"OTEL_OPAMP_ENDPOINT":           "https://env.example.com/v1/opamp",
		"OTEL_OPAMP_HEADERS":            "Authorization=Bearer%20env,X-Env=1",
		"OTEL_OPAMP_STATE_DIR":          "/var/lib/env",
		"OTEL_OPAMP_HEARTBEAT_INTERVAL": "2m",
	})
	s := serverFromEnv(vars, WithEndpoint("wss://code.example.com/v1/opamp"),
		WithHeaders(map[string]string{"authorization": "Bearer code"}), WithStateDir("/var/lib/code"),
		WithHeartbeatInterval(5*time.Second))
	if s.endpoint != "wss://code.example.com/v1/opamp" || s.stateDir != "/var/lib/code" || s.heartbeat != 5*time.Second ||
		s.header.Get("Authorization") != "Bearer code" || len(s.header) != 1 {
		t.Fatalf("code wins, headers replaced: %+v", s)
	}
	if s := serverFromEnv(vars, WithHeartbeatInterval(time.Second)); s.endpoint != "https://env.example.com/v1/opamp" ||
		s.stateDir != "/var/lib/env" || s.heartbeat != time.Second || s.header.Get("X-Env") != "1" {
		t.Fatalf("the rest from the variables: %+v", s)
	}
	s = serverFromEnv(vars, WithEndpoint("https://first.example.com"), WithEndpoint("https://second.example.com"), WithEndpoint(""),
		WithHeartbeatInterval(time.Second), WithHeartbeatInterval(3*time.Second), WithHeartbeatInterval(0),
		WithStateDir("/a"), WithStateDir("/b"), WithStateDir(""),
		WithHeaders(map[string]string{"X-A": "1"}), WithHeaders(map[string]string{"X-B": "2"}), WithHeaders(nil))
	if s.endpoint != "https://second.example.com" || s.heartbeat != 3*time.Second || s.stateDir != "/b" ||
		s.header.Get("X-B") != "2" || len(s.header) != 1 {
		t.Fatalf("the last given counts: %+v", s)
	}
}

func TestTheStateDirectoryInCodeOrNone(t *testing.T) {
	const chose = "service.instance.id=0199e3a0-0000-7000-8000-000000000007" // a UUID: the instance UID
	for _, c := range []struct {
		name string
		vars map[string]string
		opts []Option
		want string // "default": one of the user's own, locked
	}{
		{"none in code", nil, []Option{WithStateDir("none")}, ""},
		{"NONE in code, any case", nil, []Option{WithStateDir(" NONE ")}, ""},
		{"none in code over a directory in the variable", map[string]string{"OTEL_OPAMP_STATE_DIR": "/var/lib/env"},
			[]Option{WithStateDir("none")}, ""},
		{"a directory in code over none in the variable", map[string]string{"OTEL_OPAMP_STATE_DIR": "none"},
			[]Option{WithStateDir("/var/lib/code")}, "/var/lib/code"},
		{"none in the variable", map[string]string{"OTEL_OPAMP_STATE_DIR": "None"}, nil, ""},
		{"neither", nil, nil, "default"},
		{"neither, but a service.instance.id the application chose", map[string]string{"OTEL_RESOURCE_ATTRIBUTES": chose}, nil, ""},
		{"a service.instance.id the application chose, and a directory in code", map[string]string{"OTEL_RESOURCE_ATTRIBUTES": chose},
			[]Option{WithStateDir("/var/lib/code")}, "/var/lib/code"},
		{"the endpoint and the directory in code", map[string]string{"OTEL_OPAMP_ENDPOINT": ""},
			[]Option{WithEndpoint("https://code.example.com"), WithStateDir("/var/lib/code")}, "/var/lib/code"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			vars := with(with(stateHome(home), map[string]string{"OTEL_OPAMP_ENDPOINT": "https://base.example.com"}), c.vars)
			if vars["OTEL_OPAMP_ENDPOINT"] == "" {
				delete(vars, "OTEL_OPAMP_ENDPOINT")
			}
			s := serverFromEnv(env(vars), c.opts...)
			defer released(s)
			if got := shown(s, "state dir", home); got != c.want {
				t.Fatalf("state dir %q, want %q", got, c.want)
			}
			if c.want != "default" && s.stateLock != nil {
				t.Fatal("no default directory, so no lock held")
			}
		})
	}
}

// A setting that can't be used warns, is left out or is the default, and never stops the package.
func TestAnUnusableSettingWarnsAndIsTheDefault(t *testing.T) {
	for _, c := range []struct {
		name    string
		vars    map[string]string
		opts    []Option
		setting string
		want    string
		warning string
	}{
		{"an unreadable interval in the variable", map[string]string{"OTEL_OPAMP_HEARTBEAT_INTERVAL": "soon"}, nil,
			"heartbeat", "0s", "OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable"},
		{"a negative interval in the variable", map[string]string{"OTEL_OPAMP_HEARTBEAT_INTERVAL": "-5s"}, nil,
			"heartbeat", "0s", "OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable"},
		{"an interval in hours in the variable", map[string]string{"OTEL_OPAMP_HEARTBEAT_INTERVAL": "1h"}, nil,
			"heartbeat", "0s", "OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable"},
		{"a negative interval in code", nil, []Option{WithHeartbeatInterval(-time.Second)},
			"heartbeat", "0s", "WithHeartbeatInterval is negative"},
		{"a negative interval in code over a good variable", map[string]string{"OTEL_OPAMP_HEARTBEAT_INTERVAL": "2m"},
			[]Option{WithHeartbeatInterval(-time.Second)}, "heartbeat", "0s", "WithHeartbeatInterval is negative"},
		{"an unreadable interval in the variable under a good one in code", map[string]string{"OTEL_OPAMP_HEARTBEAT_INTERVAL": "soon"},
			[]Option{WithHeartbeatInterval(time.Second)}, "heartbeat", "1s", ""},
		{"a header name HTTP can't carry in the variable", map[string]string{"OTEL_OPAMP_HEADERS": "Bad Name=x,X-Ok=1"}, nil,
			"headers", "X-Ok=1", "OTEL_OPAMP_HEADERS has a header HTTP can't carry"},
		{"a header value HTTP can't carry in the variable", map[string]string{"OTEL_OPAMP_HEADERS": "X-Bad=a%0D%0Ab,X-Ok=1"}, nil,
			"headers", "X-Ok=1", "OTEL_OPAMP_HEADERS has a header HTTP can't carry"},
		{"a header value not percent-encoded in the variable", map[string]string{"OTEL_OPAMP_HEADERS": "X-Bad=100%,X-Ok=1"}, nil,
			"headers", "X-Ok=1", "isn't percent-encoded"},
		{"a header name HTTP can't carry in code", nil, []Option{WithHeaders(map[string]string{"Bad Name": "x", "X-Ok": "1"})},
			"headers", "X-Ok=1", "WithHeaders has a header HTTP can't carry"},
		{"a header value HTTP can't carry in code", nil, []Option{WithHeaders(map[string]string{"X-Bad": "a\r\nb", "X-Ok": "1"})},
			"headers", "X-Ok=1", "WithHeaders has a header HTTP can't carry"},
		{"an endpoint without a scheme in code", nil, []Option{WithEndpoint("opamp.example.com:4320/v1/opamp")},
			"endpoint", "http://opamp.example.com:4320/v1/opamp", `WithEndpoint "opamp.example.com:4320/v1/opamp" has no scheme`},
	} {
		t.Run(c.name, func(t *testing.T) {
			said := records(t)
			vars := with(map[string]string{"OTEL_OPAMP_ENDPOINT": "https://base.example.com", "OTEL_OPAMP_STATE_DIR": "none"}, c.vars)
			s := serverFromEnv(env(vars), c.opts...)
			if s == nil {
				t.Fatal("still on")
			}
			if got := shown(s, c.setting, ""); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			switch {
			case c.warning == "" && len(*said) != 0:
				t.Fatalf("nothing to warn about; said: %q", *said)
			case c.warning != "" && (len(*said) != 1 || !strings.HasPrefix((*said)[0], "WARN ") || !strings.Contains((*said)[0], c.warning)):
				t.Fatalf("want one warning about %q; said: %q", c.warning, *said)
			}
		})
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"1500": 1500 * time.Millisecond, "500ms": 500 * time.Millisecond,
		" 30s ": 30 * time.Second, "2m": 2 * time.Minute, "153722867m": 153722867 * time.Minute} {
		if got, err := duration(in); err != nil || got != want {
			t.Errorf("duration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// The largest that fits is 153722867m (time.Duration is int64 nanoseconds); past it, out of range.
	for _, in := range []string{"", "-1", "1h", "1.5s", "s", "153722868m", "307445735m", "9223372036855s",
		"9223372036855ms", "99999999999999999999"} {
		if _, err := duration(in); err == nil {
			t.Errorf("duration(%q) should fail", in)
		}
	}
}

func TestSamplerFromEnv(t *testing.T) {
	for vars, want := range map[[2]string]string{
		{"", ""}:                            "ParentBased{root:AlwaysOnSampler,",
		{"always_off", ""}:                  "AlwaysOffSampler",
		{"traceidratio", "0.25"}:            "TraceIDRatioBased{0.25}",
		{"traceidratio", "7"}:               "TraceIDRatioBased{1}",
		{"parentbased_traceidratio", "0.5"}: "ParentBased{root:TraceIDRatioBased{0.5},",
		{"parentbased_always_off", ""}:      "ParentBased{root:AlwaysOffSampler,",
		{"jaeger_remote", ""}:               "ParentBased{root:AlwaysOnSampler,",
	} {
		got := samplerFromEnv(env(map[string]string{"OTEL_TRACES_SAMPLER": vars[0], "OTEL_TRACES_SAMPLER_ARG": vars[1]})).Description()
		if len(got) < len(want) || got[:len(want)] != want {
			t.Errorf("%v: %s, want %s...", vars, got, want)
		}
	}
}
