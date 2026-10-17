// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/log"
	logembedded "go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/metric"
	metricembedded "go.opentelemetry.io/otel/metric/embedded"
	otelsdk "go.opentelemetry.io/otel/sdk"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// The inventory (docs/go-otel.md#inventory): what this instance runs, as its available
// components. Go instrumentations are libraries the application wires in, which make themselves
// known only when they take a tracer, a meter or a logger from the providers Setup set; the
// switching providers keep each scope they hand one to, the same scopes deactivate_instrumentations
// switches off, and the agent reports the list again when a new one comes.

// module is this package's module, as published.
const module = "github.com/pydantic/opamp-clients/go/otel"

// The most scope names kept: past the layer's own limit (1024 instrumentations, the first by
// name), so those are still the ones listed, and bounded, for an application that names a tracer
// per request. And the most versions kept for a name: the layer's (16), the first sorted, the same
// ones it lists.
const (
	maxScopeNames    = 4 * 1024
	maxScopeVersions = 16
)

// seenScopes are the instrumentation scopes the providers have handed a tracer, meter or logger to,
// each with the versions it gave (WithInstrumentationVersion; "": none). Safe for concurrent use.
type seenScopes struct {
	mu      sync.RWMutex
	seen    map[string]map[string]struct{} // scope name: its versions, at most maxScopeVersions
	gen     atomic.Uint64                  // bumped at each change
	changed atomic.Pointer[func()]
}

// add keeps a scope; the first time it is kept with this version, changed is called. Past
// maxScopeNames only a name before the last kept is, in place of the last, and past
// maxScopeVersions the same for a name's versions.
func (s *seenScopes) add(name, version string) {
	s.mu.RLock()
	_, known := s.seen[name][version]
	s.mu.RUnlock()
	if known || !s.keep(name, version) {
		return
	}
	s.gen.Add(1)
	if f := s.changed.Load(); f != nil {
		(*f)()
	}
}

// keep adds version to name's versions, within the limits; whether it was kept and was not before.
// A version with a comma is not kept, as the layer leaves it out (it would read as several in the
// joined list), so it takes none of the name's slots; the name is kept all the same.
func (s *seenScopes) keep(name, version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions, ok := s.seen[name]
	if !ok {
		if s.seen == nil {
			s.seen = map[string]map[string]struct{}{}
		}
		if len(s.seen) >= maxScopeNames {
			// The first by name, whatever came first: every instance of a service lists the same.
			last := name
			for n := range s.seen {
				last = max(last, n)
			}
			if last == name {
				return false
			}
			delete(s.seen, last)
		}
		versions = map[string]struct{}{}
		s.seen[name] = versions
	}
	if strings.Contains(version, ",") {
		return !ok
	}
	if _, ok := versions[version]; ok {
		return false
	}
	versions[version] = struct{}{}
	if len(versions) > maxScopeVersions {
		last := ""
		for v := range versions {
			last = max(last, v)
		}
		delete(versions, last)
		return last != version
	}
	return true
}

// instrumentations are the scopes as the inventory lists them, and the generation they are of:
// each under the name deactivate_instrumentations takes (Off), the last path element of its scope
// name, with the version it gave, else the version of the Go module it is in (build info).
func (s *seenScopes) instrumentations() ([]*settingspb.Instrumentation, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	gen := s.gen.Load()
	var out []*settingspb.Instrumentation
	for name, versions := range s.seen {
		if len(versions) == 0 { // only versions with a comma: listed with none
			out = append(out, &settingspb.Instrumentation{Name: name[strings.LastIndexByte(name, '/')+1:]})
		}
		for version := range versions {
			if version == "" {
				version = moduleVersion(name)
			}
			out = append(out, &settingspb.Instrumentation{Name: name[strings.LastIndexByte(name, '/')+1:], Version: version})
		}
	}
	return out, gen
}

// buildInfo is the binary's build info (nil: none); a test's own in tests (go test builds its
// binary with no dependencies listed).
var buildInfo = sync.OnceValue(func() *debug.BuildInfo {
	bi, _ := debug.ReadBuildInfo()
	return bi
})

// moduleVersion is the version of the Go module the package at path is in, from the binary's
// build info: "" when none is (an application's own scope, a name that isn't a package path) or
// the module has no version (built from a checkout, or replaced by a directory).
func moduleVersion(path string) string {
	bi := buildInfo()
	if bi == nil {
		return ""
	}
	version, longest := "", 0
	for _, m := range append([]*debug.Module{&bi.Main}, bi.Deps...) {
		if len(m.Path) <= longest || (path != m.Path && !strings.HasPrefix(path, m.Path+"/")) {
			continue
		}
		longest, version = len(m.Path), m.Version
		if m.Replace != nil {
			version = m.Replace.Version
		}
	}
	if version == "(devel)" {
		return ""
	}
	return version
}

// MeterProvider hands out the meters of mp, keeping each scope it hands one to for the inventory.
func (c *Controls) MeterProvider(mp metric.MeterProvider) metric.MeterProvider {
	return &meterProvider{mp: mp, c: c}
}

type meterProvider struct {
	metricembedded.MeterProvider
	mp metric.MeterProvider
	c  *Controls
}

func (p *meterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	p.c.scopes.add(name, metric.NewMeterConfig(opts...).InstrumentationVersion())
	return p.mp.Meter(name, opts...)
}

// LoggerProvider hands out the loggers of lp, keeping each scope it hands one to for the
// inventory: a log bridge's, by the name the application gives it.
func (c *Controls) LoggerProvider(lp log.LoggerProvider) log.LoggerProvider {
	return &loggerProvider{lp: lp, c: c}
}

type loggerProvider struct {
	logembedded.LoggerProvider
	lp log.LoggerProvider
	c  *Controls
}

func (p *loggerProvider) Logger(name string, opts ...log.LoggerOption) log.Logger {
	p.c.scopes.add(name, log.NewLoggerConfig(opts...).InstrumentationVersion())
	return p.lp.Logger(name, opts...)
}

// exporterName is the name OTEL_*_EXPORTER gives the exporter autoexport built for a signal
// (otlp when unset). autoexport reads the process's environment, so this does too.
func exporterName(variable string) string {
	if name := os.Getenv(variable); name != "" {
		return name
	}
	return "otlp"
}

// inventory is what this instance runs: this package and its version, the SDK's, every scope
// that has taken a tracer, meter or logger (switched off or not), and the exporters Setup built,
// and the generation of scopes it lists.
func (a *agent) inventory() (*settingspb.Inventory, uint64) {
	instrumentations, gen := a.controls.scopes.instrumentations()
	return &settingspb.Inventory{
		Integration:        module,
		IntegrationVersion: moduleVersion(module),
		SdkLanguage:        "go",
		SdkVersion:         otelsdk.Version(),
		Instrumentations:   instrumentations,
		Exporters:          a.exporters,
	}, gen
}

// components is the inventory as the layer builds it, with the settings the host carries out
// (the same host and clock as Load and Decide); nil when it can't. Under a.mu.
func (a *agent) components(ctx context.Context) *protobufs.AvailableComponents {
	inventory, gen := a.inventory()
	c, err := a.layer.Components(ctx, inventory, a.host(), unixMs(a.now()))
	if err != nil {
		slog.Warn("opamp: the inventory could not be built", "error", err)
		return nil
	}
	a.listed = gen
	return c
}

// scopesChanged reports the inventory again, when a scope has come since it was last listed
// (from its own goroutine: an instrumentation taking a tracer isn't held up).
func (a *agent) scopesChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || a.reporter == nil || a.controls.scopes.gen.Load() == a.listed {
		return
	}
	if c := a.components(context.Background()); c != nil {
		a.reporter.SetAvailableComponents(c)
	}
}
