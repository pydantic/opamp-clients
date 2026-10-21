// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// An application traced with otelhttp, set up with opampotel.Setup: an HTTP request every
// 200 ms to the URL it is given, and a log line for each, until SIGTERM or Ctrl-C. It traces
// (and measures) every request the process makes through the default transport, as many
// applications do; the OpAMP client's own requests still aren't traced. Its log lines go through
// otelslog to the logger provider Setup installs.
//
// The OpAMP server comes from OTEL_OPAMP_*, or is given in code from the application's own
// flags and variable, which win over OTEL_OPAMP_*; a flag left out falls back to its variable:
//
//	CHECKOUT_OPAMP_KEY=<enrollment key> ./example -opamp https://opamp.example.com/v1/opamp \
//	    -state-dir /var/lib/checkout/opamp -heartbeat 30s https://example.com/
//
// With EXAMPLE_SETTINGS=1 it also shows the settings that match telemetry (try.py's
// check_settings): each tick it also requests /health next to the URL it is given (a sampling
// rule on the span name, "GET /health", can leave those out), and a second bridge logger, "noisy", writes three debug lines (a
// level per scope can silence it). Its spans carry url.full, which redaction can drop or mask.
//
// It also reads a setting of its own the server sends (docs/app-settings.md): a feature flag,
// example.feature. While the server says "on", each tick also requests /feature; and each time
// its settings change it requests /changed, once. Without a server, or until the server says, the
// flag is its default, "off".
//
// With EXAMPLE_PROBE=1 (integrations.py's run 3) each tick also probes redaction: a span of its own,
// example.probe, with the URL it requests (url.full) on an event and on a link to the tick
// before's probe, and a log line example.probe with that URL. It probes before or after the request,
// at random: under a rate limit of one a second, the first record after the token comes takes it,
// and in a fixed order the probe might never be that one.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opampotel"
)

var (
	endpoint  = flag.String("opamp", "", "the OpAMP server's endpoint (default: OTEL_OPAMP_ENDPOINT)")
	stateDir  = flag.String("state-dir", "", "where to keep state across restarts, or none (default: OTEL_OPAMP_STATE_DIR)")
	heartbeat = flag.Duration("heartbeat", 0, "how often to check in (default: OTEL_OPAMP_HEARTBEAT_INTERVAL)")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Client spans named by method and path ("GET /ping"): otelhttp's transport adds its request
	// attributes (url.full) after the span starts, so a sampling rule sees only the name and kind.
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))

	// Blank options are as if not given, so the flags can be passed as they are.
	opts := []opampotel.Option{
		opampotel.WithEndpoint(*endpoint),
		opampotel.WithStateDir(*stateDir),
		opampotel.WithHeartbeatInterval(*heartbeat),
	}
	if key := os.Getenv("CHECKOUT_OPAMP_KEY"); key != "" { // a secret: not in argv
		opts = append(opts, opampotel.WithHeaders(map[string]string{"Authorization": "Bearer " + key}))
	}
	sdk, err := opampotel.Setup(ctx, opts...)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sdk.Shutdown(shutdown); err != nil {
			log.Print(err)
		}
	}()
	if names := os.Getenv("EXAMPLE_SETUP"); names != "" { // try.py's truth checks (setups.go): none otherwise
		stop := applySetups(ctx, names)
		defer func() { _ = stop(context.Background()) }()
	}

	logger, noisy := otelslog.NewLogger("app"), otelslog.NewLogger("noisy")
	settings := os.Getenv("EXAMPLE_SETTINGS") == "1"
	probing := os.Getenv("EXAMPLE_PROBE") == "1"
	var lastProbe trace.SpanContext
	base := strings.TrimSuffix(flag.Arg(0), "/ping")
	health := base + "/health"
	// Told on the OpAMP client's goroutine: keep it quick, and do the work in the application's
	// own loop. Buffered so the client never waits; a change told while the buffer is full would
	// be dropped, which a tick every 200 ms never lets happen here.
	changes := make(chan []string, 64)
	defer opampotel.OnAppSettingsChange(func(_ map[string]string, changed []string) {
		select {
		case changes <- changed:
		default:
		}
	})()
	for tick := time.Tick(200 * time.Millisecond); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			early := rand.IntN(2) == 1
			if probing && early {
				lastProbe = probe(ctx, logger, flag.Arg(0), lastProbe)
			}
			get(ctx, flag.Arg(0))
			logger.InfoContext(ctx, "requested", "url", flag.Arg(0))
			if probing && !early {
				lastProbe = probe(ctx, logger, flag.Arg(0), lastProbe)
			}
			if opampotel.AppSetting("example.feature", "off") == "on" {
				get(ctx, base+"/feature")
			}
			for told := true; told; {
				select {
				case changed := <-changes:
					logger.InfoContext(ctx, "settings changed", "keys", strings.Join(changed, ", "))
					get(ctx, base+"/changed")
				default:
					told = false
				}
			}
			if settings {
				get(ctx, health)
				for range 3 {
					noisy.DebugContext(ctx, "noisy detail", "url", health)
				}
			}
		}
	}
}

// probe makes a span of its own with url on an event and on a link to last (none while last is
// the zero value), and a log line with url: what redaction may drop or mask. It returns the span's
// context, for the next probe to link to.
func probe(ctx context.Context, logger *slog.Logger, url string, last trace.SpanContext) trace.SpanContext {
	attrs := []attribute.KeyValue{attribute.String("url.full", url)}
	var opts []trace.SpanStartOption
	if last.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: last, Attributes: attrs}))
	}
	_, span := otel.Tracer("app").Start(ctx, "example.probe", opts...)
	span.AddEvent("example.probe", trace.WithAttributes(attrs...))
	span.End()
	logger.InfoContext(ctx, "example.probe", "url.full", url)
	return span.SpanContext()
}

func get(ctx context.Context, url string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
	}
}
