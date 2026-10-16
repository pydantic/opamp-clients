// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// server is where the OpAMP server is, as the core's Setup decides it from Setup's options and
// OTEL_OPAMP_*.
type server struct {
	endpoint  string
	header    http.Header
	stateDir  string        // "": none
	stateLock *os.File      // the default state directory's lock, held while the process runs; nil for a named one
	heartbeat time.Duration // 0: the client's default
	// metricsUngated: the metrics reader is autoexport's own (a pull exporter, or producers), so
	// send_metrics is not declared to the settings layer. A policy that overrides the metrics
	// export is then refused, not answered as done.
	metricsUngated bool
	// shown is the endpoint as it is safe to log; hints, what to check when the server can't be
	// reached, naming what gave the endpoint and the headers (the option or the variable).
	shown string
	hints *opamp.SetupHints
}

// Option gives Setup one of the OTEL_OPAMP_* settings in code; it wins over the variable.
// An option left blank (an empty string or map, a zero interval) is as if not given: its variable
// still applies, so a flag or setting of the application's own that is left unset passes through.
type Option func(*options)

type options struct {
	endpoint, stateDir string
	header             map[string]string // nil: not given
	heartbeat          time.Duration     // 0: not given
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
		if len(headers) != 0 {
			o.header = headers
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

// setupVariables are the variables the core's Setup reads.
var setupVariables = []string{"OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS", "OTEL_OPAMP_STATE_DIR",
	"OTEL_OPAMP_HEARTBEAT_INTERVAL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "HOME", "XDG_STATE_HOME",
	"LOCALAPPDATA"}

// setupOS is this operating system, as Setup names it.
func setupOS() opamp.SetupOs {
	switch runtime.GOOS {
	case "windows":
		return opamp.SetupOsWindows
	case "darwin", "ios":
		return opamp.SetupOsMacOS
	}
	return opamp.SetupOsUnix
}

// serverFromEnv is where the server is, from OTEL_OPAMP_*, each overridden by the option that
// gives it; nil without an endpoint (the package is off). The rules are the core's (its Setup
// call), the same in every language; what is here is the I/O: reading the variables, taking a
// slot of the default state directory, and saying the warnings. A setting it can't use is never
// an error: the default, with a warning.
func serverFromEnv(getenv func(string) string, opts ...Option) *server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	code := &opamp.SetupInCode{EndpointName: "WithEndpoint", HeadersName: "WithHeaders",
		HeartbeatName: "WithHeartbeatInterval"}
	if o.endpoint != "" {
		code.Endpoint = &o.endpoint
	}
	if o.stateDir != "" {
		code.StateDir = &o.stateDir
	}
	if o.header != nil {
		code.HeadersGiven = true
		names := make([]string, 0, len(o.header))
		for name := range o.header {
			names = append(names, name)
		}
		sort.Strings(names) // a map has no order of its own: the same every start
		for _, name := range names {
			code.Headers = append(code.Headers, &opamp.Header{Name: name, Value: o.header[name]})
		}
	}
	if o.heartbeat != 0 {
		ms := o.heartbeat.Milliseconds()
		code.HeartbeatMs = &ms
	}
	start := &opamp.SetupStart{Os: setupOS(), Program: programName(os.Args), Code: code}
	for _, name := range setupVariables {
		if v := getenv(name); v != "" {
			start.Env = append(start.Env, &opamp.Variable{Name: name, Value: v})
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		start.Cwd = cwd
	}
	s, err := opamp.StartSetup(context.Background(), start)
	if err != nil {
		slog.Error("opamp: the core could not read the settings; the SDK runs unmanaged", "error", err)
		return nil
	}
	for _, w := range s.GetWarnings() {
		slog.Warn("opamp: " + w)
	}
	if s.GetOff() {
		return nil
	}
	srv := &server{endpoint: s.GetEndpoint(), header: http.Header{}, stateDir: s.GetStateDir(),
		heartbeat: time.Duration(s.GetHeartbeatIntervalMs()) * time.Millisecond, shown: s.GetShownEndpoint(),
		hints: s.GetHints()}
	for _, h := range s.GetHeaders() {
		srv.header.Add(h.GetName(), h.GetValue())
	}
	if srv.stateDir == "" {
		srv.stateDir, srv.stateLock = slot(s.GetDefaultSlots(), s.GetAllSlotsTaken())
	}
	return srv
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
