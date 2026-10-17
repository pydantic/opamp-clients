// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/net/http/httpguts"
)

// server is where the OpAMP server is, from Setup's options and OTEL_OPAMP_*.
type server struct {
	endpoint  string
	header    http.Header
	stateDir  string        // "": none
	stateLock *os.File      // the default state directory's lock, held while the process runs; nil for a named one
	heartbeat time.Duration // 0: the client's default
	// metricsUngated: the metrics reader is autoexport's own (a pull exporter, or producers), so
	// send_metrics isn't declared to the settings layer: set aside rather than answered done.
	metricsUngated bool
	// endpointFrom and headersFrom name what gave each, the option or the variable, for the
	// warning when the server can't be reached.
	endpointFrom, headersFrom string
}

// Option gives Setup one of the OTEL_OPAMP_* settings in code; it wins over the variable.
// An option left blank (an empty string or map, a zero interval) is as if not given: its variable
// still applies, so a flag or setting of the application's own that is left unset passes through.
type Option func(*options)

type options struct {
	endpoint, stateDir string
	header             http.Header   // nil: not given
	heartbeat          time.Duration // 0: not given
}

// WithEndpoint is OTEL_OPAMP_ENDPOINT; given, the package is on without the variable.
func WithEndpoint(endpoint string) Option {
	return func(o *options) {
		if strings.TrimSpace(endpoint) != "" {
			o.endpoint = endpoint
		}
	}
}

// WithHeaders is OTEL_OPAMP_HEADERS, values as they are (not percent-encoded); it replaces the
// variable's headers, not adds to them.
func WithHeaders(headers map[string]string) Option {
	return func(o *options) {
		if len(headers) == 0 {
			return
		}
		o.header = http.Header{}
		for k, v := range headers {
			addHeader(o.header, "WithHeaders", k, v)
		}
	}
}

// WithStateDir is OTEL_OPAMP_STATE_DIR: a directory, or "none".
func WithStateDir(dir string) Option {
	return func(o *options) {
		if strings.TrimSpace(dir) != "" {
			o.stateDir = dir
		}
	}
}

// WithHeartbeatInterval is OTEL_OPAMP_HEARTBEAT_INTERVAL; a negative one warns and is the default.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(o *options) {
		if d != 0 {
			o.heartbeat = d
		}
	}
}

// serverFromEnv reads OTEL_OPAMP_*, each overridden by the option that gives it; nil without an
// endpoint (the package is off).
// Without a state directory it is one of the user's own (defaultStateDir); "none" turns that off.
// A heartbeat interval it can't read is the client's default: it warns, and holds nothing up.
func serverFromEnv(getenv func(string) string, opts ...Option) *server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	endpoint, name := strings.TrimSpace(o.endpoint), "WithEndpoint"
	if endpoint == "" {
		endpoint, name = strings.TrimSpace(getenv("OTEL_OPAMP_ENDPOINT")), "OTEL_OPAMP_ENDPOINT"
	}
	if endpoint == "" {
		return nil
	}
	if !strings.Contains(endpoint, "://") {
		slog.Warn(fmt.Sprintf("opamp: %s %q has no scheme, so it is taken as http://; "+
			"name it in full, as https://host/v1/opamp or wss://host/v1/opamp", name, where(endpoint)))
		endpoint = "http://" + endpoint // as the warning says: the client refuses it without one
	}
	s := &server{endpoint: endpoint, header: o.header, endpointFrom: name, headersFrom: "WithHeaders"}
	if s.header == nil {
		s.header, s.headersFrom = headers(getenv("OTEL_OPAMP_HEADERS")), "OTEL_OPAMP_HEADERS"
	}
	dir := strings.TrimSpace(o.stateDir)
	if dir == "" {
		dir = strings.TrimSpace(getenv("OTEL_OPAMP_STATE_DIR"))
	}
	switch {
	case strings.EqualFold(dir, "none"):
	case dir != "":
		s.stateDir = dir
	case !choseUID(getenv("OTEL_RESOURCE_ATTRIBUTES")):
		// A service.instance.id the application chose stays its choice: no default to override it.
		// One that isn't a UUID can't be the instance UID: it is replaced, by a UID kept here.
		s.stateDir, s.stateLock = defaultStateDir(getenv, os.Args)
	}
	switch v := strings.TrimSpace(getenv("OTEL_OPAMP_HEARTBEAT_INTERVAL")); {
	case o.heartbeat < 0:
		slog.Warn("opamp: WithHeartbeatInterval is negative, using the default", "interval", o.heartbeat)
	case o.heartbeat > 0:
		s.heartbeat = o.heartbeat
	case v != "":
		if d, err := duration(v); err != nil {
			slog.Warn("opamp: OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable, using the default", "error", err)
		} else {
			s.heartbeat = d
		}
	}
	return s
}

// choseUID: OTEL_RESOURCE_ATTRIBUTES gives a service.instance.id that is a UUID, the instance UID
// the application chose.
func choseUID(resourceAttrs string) bool {
	_, ok := chosenUID(resourceAttrs)
	return ok
}

// chosenUID is the service.instance.id OTEL_RESOURCE_ATTRIBUTES gives, if it is a UUID.
func chosenUID(resourceAttrs string) (uuid.UUID, bool) {
	for _, kv := range strings.Split(resourceAttrs, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if strings.TrimSpace(k) == instanceID {
			if u, err := uuid.Parse(strings.TrimSpace(v)); err == nil {
				return u, true
			}
		}
	}
	return uuid.UUID{}, false
}

// headers parses `name=value,...`, values percent-decoded, as OTEL_EXPORTER_OTLP_HEADERS.
func headers(spec string) http.Header {
	h := http.Header{}
	for _, pair := range strings.Split(spec, ",") {
		name, value, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		if v, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
			addHeader(h, "OTEL_OPAMP_HEADERS", name, v)
		} else {
			slog.Warn("opamp: OTEL_OPAMP_HEADERS has a value that isn't percent-encoded, leaving it out", "header", name)
		}
	}
	return h
}

// addHeader adds one header, or, when HTTP can't carry it, warns and leaves it out: a header no
// request could be sent with would stop the client reaching the server at all.
func addHeader(h http.Header, from, name, value string) {
	name = strings.TrimSpace(name)
	if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
		slog.Warn(fmt.Sprintf("opamp: %s has a header HTTP can't carry, leaving it out", from), "header", name)
		return
	}
	h.Add(name, value)
}

var durationRE = regexp.MustCompile(`^\s*(\d+)\s*(ms|s|m)?\s*$`)

// duration parses `1500` (milliseconds, as the SDK's variables), or `500ms`, `30s`, `2m`.
func duration(v string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("not a duration: %q", v)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a duration: %q", v)
	}
	unit := map[string]time.Duration{"": time.Millisecond, "ms": time.Millisecond, "s": time.Second, "m": time.Minute}[m[2]]
	// Multiplied out, a longer one would wrap around int64 nanoseconds to some short interval.
	if n > int64(math.MaxInt64/unit) {
		return 0, fmt.Errorf("out of range (at most %v): %q", time.Duration(math.MaxInt64), v)
	}
	return time.Duration(n) * unit, nil
}

// samplerFromEnv is the sampler OTEL_TRACES_SAMPLER names, as the SDK builds it when given none
// (the SDK's own function is unexported): the specification's values, and an argument out of
// range is 1.
func samplerFromEnv(getenv func(string) string) sdktrace.Sampler {
	ratio := func() sdktrace.Sampler {
		r, err := strconv.ParseFloat(strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER_ARG")), 64)
		if err != nil || r < 0 || r > 1 {
			r = 1
		}
		return sdktrace.TraceIDRatioBased(r)
	}
	switch strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER"))) {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "traceidratio":
		return ratio()
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(ratio())
	default:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
}
