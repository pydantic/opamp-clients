// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// host is what this package carries out, declared to the settings layer (docs/sdk-settings.md,
// "The host"). The layer refuses a policy that overrides a setting not named here, and the
// description lists only the controls named. The host names every live setting in every configuration
// Setup builds (the sampler wraps whichever one OTEL_TRACES_SAMPLER names, and every span and log
// exporter it builds goes through a gate), send_metrics only where the metrics reader is built
// around a gated exporter, the instrumentation switches at once too (a switched-off
// instrumentation's tracer starts no span from then on), so nothing waits for the next start. A
// global provider the application sets in place of Setup's narrows it (agent.host).
func (s *server) host() *settingspb.Host {
	live := append([]string{"deactivate_instrumentations", "deactivate_all_instrumentations"}, carried...)
	if !s.metricsUngated {
		live = append(live, "send_metrics")
	}
	return &settingspb.Host{Live: live}
}

// carried are the live settings this package carries out whatever it built: those of its sampler,
// its span and log gates, and debug mode, which the layer folds into them.
var carried = []string{
	"sampling_rate",
	"sampling_rules",
	"sampling_traces_per_second",
	"send_traces",
	"send_logs",
	"send_logs_level",
	"send_logs_level_by_scope",
	"send_logs_per_second",
	"drop_attributes",
	"mask_attributes",
	"scrub_defaults",
	"scrub_patterns",
	"debug_until",
	"debug_sampling_rate",
	"debug_send_logs_level",
	// The application's own settings, the policy's app_settings control (docs/app-settings.md),
	// which it reads with AppSetting.
	"custom",
}

// agent puts the SDK under the server: one per Setup.
type agent struct {
	server   *server
	controls *Controls
	layer    *sdksettings.Layer
	uid      uuid.UUID
	atStart  *settingspb.Loaded

	mu        sync.Mutex // everything below it but client and reach
	current   []byte
	effective *protobufs.EffectiveConfig
	// The timer: one, re-armed for the earlier of when the layer said to ask again (refreshAt,
	// the end of debug mode) and the next export health report. gen is bumped at each arming, so
	// a timer that went off before a newer arming is nothing.
	refreshAt uint64    // milliseconds since the Unix epoch; 0: nothing to ask
	deadline  time.Time // refreshAt on the monotonic clock: a wall clock moved since doesn't move it
	// Export health, once the client runs: the settings in force (a send_* false is "off"), the
	// last report the layer answered, when the next is due, and the time the agent started.
	settings    *settingspb.Settings
	health      *settingspb.HealthReport
	nextHealth  time.Time // zero: none
	healthEvery time.Duration
	started     uint64 // nanoseconds since the Unix epoch
	gen         uint64
	timer       stopper
	stopped     bool
	reporter    reporter // the client, once there is one: where a timer's changes are reported

	// now and after are the clock and the timer (a test's own in tests).
	now   func() time.Time
	after func(time.Duration, func()) stopper

	client *opamp.Client
	reach  *reachability // set in start; used on the client's goroutine only

	// The inventory: the exporters Setup built (set before start), and the generation of scopes
	// last listed (under mu).
	exporters []*settingspb.Exporter
	listed    uint64
	// relist: the host narrowed before there was a client to tell; start sends the inventory.
	relist bool

	// The global providers Setup set (set before start), which of them the application has
	// replaced since (under mu), the global providers now (a test's own in tests), and what
	// stops watching them.
	ours      providers
	replaced  replaced
	globals   func() providers
	watchStop chan struct{}
}

// stopper is a timer that can be stopped (*time.Timer).
type stopper interface{ Stop() bool }

// reporter is what a timer's changes go to: the client.
type reporter interface {
	SetEffectiveConfig(*protobufs.EffectiveConfig)
	SetHealth(*protobufs.ComponentHealth)
	SetAvailableComponents(*protobufs.AvailableComponents)
}

func afterFunc(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

// unixMs is t in milliseconds since the Unix epoch, as the layer takes the time (0 before it).
func unixMs(t time.Time) uint64 {
	if ms := t.UnixMilli(); ms > 0 {
		return uint64(ms)
	}
	return 0
}

// newAgent loads the saved settings and puts them in force on controls, and names the instance.
// Before the SDK is built.
func newAgent(ctx context.Context, s *server, controls *Controls, resourceAttrs string) (*agent, error) {
	return newAgentAt(ctx, s, controls, resourceAttrs, time.Now, afterFunc)
}

// newAgentAt is newAgent on a clock and a timer of the caller's.
func newAgentAt(ctx context.Context, s *server, controls *Controls, resourceAttrs string, now func() time.Time,
	after func(time.Duration, func()) stopper) (*agent, error) {
	layer, err := sdksettings.New(ctx)
	if err != nil {
		return nil, err
	}
	a := &agent{server: s, controls: controls, layer: layer, now: now, after: after, healthEvery: 30 * time.Second,
		globals: globalProviders}
	changed := func() { go a.scopesChanged() }
	controls.scopes.changed.Store(&changed)
	var saved, coreState []byte
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
			slog.Warn("opamp: the state directory is unusable", "error", err)
		}
		// The settings to start with are the last config applied, which the client keeps in its
		// state (opamp-state); sdk-settings.json is what this package kept itself before, read
		// only while the client's state holds no config applied. Read as the client reads it at
		// start: after a save a client stopped in this process left writing (a config applied
		// just before a Shutdown) has landed, or this start would begin from the config before.
		coreState, err = opamp.ReadState(filepath.Join(s.stateDir, "opamp-state"))
		if err != nil {
			slog.Warn("opamp: the client's saved state unreadable, using the SDK's own settings", "error", err)
		}
		saved, err = os.ReadFile(filepath.Join(s.stateDir, "sdk-settings.json"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("opamp: saved SDK settings unreadable, using the SDK's own", "error", err)
		}
	}
	start := a.now()
	loaded, err := layer.LoadFrom(ctx, coreState, saved, s.host(), unixMs(start))
	if err != nil {
		_ = layer.Close(ctx)
		return nil, err
	}
	if loaded.GetWarning() != "" {
		slog.Warn("opamp: " + loaded.GetWarning())
	}
	a.atStart, a.current, a.effective = loaded, loaded.GetCurrent(), loaded.GetEffective()
	controls.Apply(loaded.GetSettings())
	a.settings = loaded.GetSettings()
	a.uid = identity(s.stateDir, resourceAttrs, start)
	a.mu.Lock()
	a.refreshAfter(loaded.GetRefreshAtUnixMs(), start)
	a.arm()
	a.mu.Unlock()
	return a, nil
}

// identity is the instance UID, as the core's Setup decides it: the one saved in the state
// directory; else a new one, saved there; else, where none can be kept (no state directory, or one
// it can't be saved in), a UUID service.instance.id given in OTEL_RESOURCE_ATTRIBUTES, the same at
// every start; else a new one. What is here is the I/O: the file read and written, the entropy, the
// clock.
func identity(stateDir, resourceAttrs string, now time.Time) uuid.UUID {
	ask := &opamp.SetupIdentity{StateDir: stateDir != "", ResourceAttributes: resourceAttrs,
		Entropy: make([]byte, 16), UnixMs: uint64(now.UnixMilli())}
	path := filepath.Join(stateDir, opamp.SetupUIDFile)
	if stateDir != "" {
		if b, err := os.ReadFile(path); err == nil {
			ask.Saved = string(b)
		}
	}
	id, err := func() (*opamp.SetupIdentified, error) {
		if _, err := rand.Read(ask.Entropy); err != nil {
			return nil, err
		}
		return opamp.IdentifySetup(context.Background(), ask)
	}()
	if err != nil {
		slog.Error("opamp: the core could not choose the instance UID; this start is a new instance", "error", err)
		return uuid.Must(uuid.NewRandom())
	}
	uid := id.GetInstanceUid()
	if id.GetSave() {
		if err := write(path, []byte(id.GetText())); err != nil {
			slog.Warn("opamp: can't save the instance UID", "error", err)
			uid = id.GetUnsavedUid()
		}
	}
	u, err := uuid.FromBytes(uid)
	if err != nil {
		return uuid.Must(uuid.NewRandom())
	}
	return u
}

const instanceID = "service.instance.id"

func write(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// untraced is an HTTP client of the agent's own. The default client sends through
// http.DefaultTransport, which an application may wrap in otelhttp; the agent's requests aren't
// the application's.
func untraced() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// No connect or handshake timeouts: how long anything may take is the core's.
		DialContext:           (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

// start joins the fleet, once the SDK is built. A client that can't start leaves the SDK
// running unmanaged.
func (a *agent) start(ctx context.Context, res *resource.Resource) {
	description, err := a.layer.Describe(ctx, keyValues(res), a.server.host(), unixMs(a.now()))
	if err != nil {
		slog.Error("opamp: the client did not start; the SDK runs unmanaged", "error", err)
		return
	}
	cfg := opamp.Config{
		Endpoint:          a.server.endpoint,
		InstanceUID:       a.uid,
		// What the integration does and reports, declared: the client takes no status field
		// whose bit isn't.
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents),
		HeartbeatInterval: a.server.heartbeat,
		Header:            a.server.header,
		// A key of its own, offered by the server: taken with a state directory (cfg.StateFile,
		// below) and kept there; without one the client neither declares nor takes it.
		AcceptConnectionSettings: true,
		// Platform mode, asked for by name (an HTTPClient selects it too): the agent's requests go
		// through net/http, on a transport of its own that no instrumentation wraps.
		Mode:       opamp.ModePlatform,
		HTTPClient: untraced(),
	}
	if a.server.stateDir != "" {
		cfg.StateFile = filepath.Join(a.server.stateDir, "opamp-state")
	}
	service, _ := res.Set().Value("service.name")
	a.reach = &reachability{endpoint: a.server.shown, service: service.AsString(), uid: a.uid,
		hints: a.server.hints}
	a.mu.Lock()
	effective := a.effective
	health, _ := a.healthReport(ctx, a.now()) // the first report: always sent
	components := a.components(ctx)
	a.mu.Unlock()
	// Notifications come on the client's goroutine, one at a time, and none before Start has
	// returned (docs/sdk-contract.md); each is answered through the client it comes with, which
	// queues the answers without waiting.
	client, err := opamp.Start(ctx, cfg, opamp.Status{
		Description:         description,
		Health:              health,
		EffectiveConfig:     effective,
		AvailableComponents: components,
	}, a.notified)
	if err != nil {
		slog.Error("opamp: the client did not start; the SDK runs unmanaged", "error", err)
		return
	}
	a.client = client
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reporter == nil {
		a.reporter = client
	}
	if a.effective != effective { // a timer changed it since
		client.SetEffectiveConfig(a.effective)
	}
	if a.controls.scopes.gen.Load() != a.listed || a.relist { // an instrumentation came since, or the host narrowed
		if c := a.components(ctx); c != nil {
			a.reporter.SetAvailableComponents(c)
			a.relist = false
		}
	}
	a.nextHealth = a.now().Add(a.healthEvery)
	a.arm()
	if a.ours != (providers{}) && !a.stopped {
		a.watchStop = make(chan struct{})
		go a.watch(a.watchStop, watchEvery)
	}
}

// host is what this package carries out now: the server's host (server.host), less what a global
// provider the application set in place of Setup's takes from it. Under a.mu.
func (a *agent) host() *settingspb.Host { return a.replaced.without(a.server.host()) }

// healthReport is the agent's health: the application's (healthy, as it runs), with export
// health under component_health_map["exporters"] as the layer builds it from the gates' counts,
// and whether the layer says to send it now. Under a.mu.
func (a *agent) healthReport(ctx context.Context, now time.Time) (*protobufs.ComponentHealth, bool) {
	if a.started == 0 {
		a.started = uint64(now.UnixNano())
	}
	h := &protobufs.ComponentHealth{Healthy: true, StartTimeUnixNano: a.started}
	r, err := a.layer.Health(ctx, a.controls.Counts(), a.health, unixMs(now), a.settings)
	if err != nil {
		slog.Warn("opamp: export health could not be built", "error", err)
		return h, false
	}
	a.health = r
	if r.GetExporters() != nil {
		h.ComponentHealthMap = map[string]*protobufs.ComponentHealth{"exporters": r.GetExporters()}
	}
	return h, r.GetSend()
}

// reportHealth sends export health when the layer says to (a status changed, or the counters
// moved and a minute has passed), and sets when to look again. Under a.mu.
func (a *agent) reportHealth(now time.Time) {
	h, send := a.healthReport(context.Background(), now)
	if send && a.reporter != nil {
		a.reporter.SetHealth(h)
	}
	a.nextHealth = now.Add(a.healthEvery)
}

// stop says goodbye (agent_disconnect), and waits for the client to stop, the notification being
// handled with it (until ctx is done); then the SDK may flush.
func (a *agent) stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopped = true
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.watchStop != nil {
		close(a.watchStop)
		a.watchStop = nil
	}
	a.mu.Unlock()
	var errs []error
	if a.client != nil {
		errs = append(errs, a.client.Stop(ctx))
	}
	return errors.Join(append(errs, a.layer.Close(ctx))...)
}

// notified handles a notification on the client's goroutine, answering through c.
func (a *agent) notified(c *opamp.Client, n opamp.Notification) {
	if a.reach != nil {
		a.reach.inUse = c.Endpoint
		a.reach.notified(n)
	}
	switch n.Kind {
	case opamp.RemoteConfig:
		status := a.remoteConfig(context.Background(), n.RemoteConfig)
		// Under the lock, so a timer's report can't overtake this one.
		a.mu.Lock()
		if a.reporter == nil {
			a.reporter = c
		}
		c.SetEffectiveConfig(a.effective)
		c.SetRemoteConfigStatus(status)
		a.mu.Unlock()
	case opamp.InstanceUIDChanged:
		a.instanceUIDChanged(n.InstanceUID)
	case opamp.Stopped:
		if n.Err != nil {
			slog.Error("opamp: the client stopped; the SDK runs unmanaged", "error", n.Err)
		}
	}
}

// reachability says when the client connects, when it can't (once a run of failures, with what
// to check), and when it's back: setup that works or doesn't is never silent.
type reachability struct {
	endpoint string // the configured one, as it is safe to log
	// inUse is the endpoint the client uses now, as the client gives it (nil or "": the
	// configured one): after the server moved the agent, the one it moved it to.
	inUse   func() string
	service string // "": none
	uid     uuid.UUID
	ever    bool // connected once
	failing bool // since the last connect
	// What to check when the server can't be reached (the core's Setup), naming what gave the
	// endpoint and the headers.
	hints *opamp.SetupHints
}

// at is the endpoint the client uses now, as it is safe to log.
func (r *reachability) at() string {
	if r.inUse != nil {
		if e := r.inUse(); e != "" {
			return shownEndpoint(e, r.endpoint)
		}
	}
	return r.endpoint
}

func (r *reachability) notified(n opamp.Notification) {
	endpoint := r.at()
	switch n.Kind {
	case opamp.Connected:
		if !r.ever {
			who := ""
			if r.service != "" {
				who = " as " + r.service
			}
			slog.Info(fmt.Sprintf("opamp: connected to %s; this SDK is managed%s, instance %s", endpoint, who, r.uid))
		} else if r.failing {
			slog.Info("opamp: connected to " + endpoint + " again")
		}
		r.ever, r.failing = true, false
	case opamp.ConnectFailed:
		if !r.failing {
			why := n.Detail
			if why == "" {
				why = fmt.Sprintf("HTTP %d", n.Status)
			}
			slog.Warn(fmt.Sprintf("opamp: can't reach %s (%s): %s. Retrying in the background; the SDK keeps its current settings meanwhile",
				endpoint, why, r.hint(n.Status)))
		} else {
			slog.Debug(fmt.Sprintf("opamp: still can't reach %s: %s", endpoint, n.Detail))
		}
		r.failing = true
	}
}

// shownEndpoint is an endpoint as it is safe to log (no user info, query or fragment), by the
// core's rule; otherwise where the core can't be asked.
func shownEndpoint(endpoint, otherwise string) string {
	s, err := opamp.StartSetup(context.Background(), &opamp.SetupStart{Code: &opamp.SetupInCode{Endpoint: &endpoint}})
	if err != nil || s.GetShownEndpoint() == "" {
		return otherwise
	}
	return s.GetShownEndpoint()
}

// hint is what to check when the server can't be reached, by the HTTP status (0: none).
func (r *reachability) hint(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return r.hints.GetRefused()
	case http.StatusNotFound:
		return r.hints.GetNotFound()
	case 0:
		return r.hints.GetUnreachable()
	}
	return r.hints.GetError()
}

// instanceUIDChanged keeps the UID the server assigned for the next start, which puts it in the
// telemetry too (service.instance.id); the client uses it from now on.
func (a *agent) instanceUIDChanged(uid uuid.UUID) {
	if a.server.stateDir == "" {
		slog.Warn("opamp: the server assigned a new instance UID; without a state directory the next start won't keep it",
			"instance_uid", uid.String())
		return
	}
	if err := write(filepath.Join(a.server.stateDir, opamp.SetupUIDFile), []byte(uid.String())); err != nil {
		slog.Warn("opamp: can't save the instance UID the server assigned", "error", err)
		return
	}
	slog.Info("opamp: the server assigned a new instance UID; in the telemetry from the next start",
		"instance_uid", uid.String())
}

// remoteConfig carries out the layer's decision on a remote config, and gives the answer.
func (a *agent) remoteConfig(ctx context.Context, config *protobufs.AgentRemoteConfig) *protobufs.RemoteConfigStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	// With a state directory the client keeps the config this applies, in its own state, and tells
	// the server APPLIED once that is saved: nothing for this package to save. Without one, nothing
	// is kept, and a next-start change is refused.
	var save func([]byte) error
	if a.server.stateDir != "" {
		save = func([]byte) error { return nil }
	}
	// A provider replaced since the last look is seen before the decision, so the answer is for
	// what this package carries out now.
	if a.noticeReplaced() {
		a.narrowed(now)
	}
	d, err := a.layer.DecideAt(ctx, config, a.current, a.atStart.GetCurrent(), a.host(), unixMs(now), save)
	if err != nil {
		return failed(config, err.Error())
	}
	if s := d.GetApply(); s != nil {
		a.controls.Apply(s)
		a.settings = s
	}
	if len(d.GetPendingRestart()) > 0 {
		slog.Info("opamp: " + strings.Join(d.GetPendingRestart(), ", ") + " changed; in force when the application restarts")
	}
	a.current, a.effective = d.GetCurrent(), d.GetEffective()
	if d.GetApply() == nil && a.refreshAt != 0 && unixMs(now) >= a.refreshAt {
		// FAILED, so nothing changes, but what is reported is now's: once the wall clock has
		// passed the end of debug mode (a machine that slept, before the timer went off), it ends
		// here, as the report says.
		a.refresh(now)
	} else {
		// A FAILED config leaves the one in force, and its time to ask again.
		a.refreshAfter(d.GetRefreshAtUnixMs(), now)
	}
	a.arm()
	return d.GetStatus()
}

// refreshAfter keeps when to ask the layer again, decided at now: refreshAt (0: never), and the
// same on the monotonic clock. Under a.mu.
func (a *agent) refreshAfter(refreshAt uint64, now time.Time) {
	a.refreshAt = refreshAt
	if refreshAt != 0 {
		wait := time.Duration(0)
		if ms := unixMs(now); refreshAt > ms {
			wait = time.Duration(refreshAt-ms) * time.Millisecond
		}
		a.deadline = now.Add(wait)
	}
}

// arm sets the one timer for the earliest thing due, in place of any earlier one. Under a.mu.
func (a *agent) arm() {
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.gen++
	if a.stopped {
		return
	}
	var due time.Time
	if a.refreshAt != 0 {
		due = a.deadline
	}
	if !a.nextHealth.IsZero() && (due.IsZero() || a.nextHealth.Before(due)) {
		due = a.nextHealth
	}
	if due.IsZero() {
		return
	}
	gen := a.gen
	a.timer = a.after(max(due.Sub(a.now()), 0), func() { a.fired(gen) })
}

// fired is the timer going off: what is due is done, and the timer armed again. A timer armed
// before a newer one (a decision came between) is nothing.
func (a *agent) fired(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen || a.stopped {
		return
	}
	now := a.now()
	if a.refreshAt != 0 && !now.Before(a.deadline) {
		a.refresh(now)
	}
	if !a.nextHealth.IsZero() && !now.Before(a.nextHealth) {
		a.reportHealth(now)
	}
	a.arm()
}

// refresh asks the layer what is in force now that refreshAt has come (debug mode ends), puts it
// in force, and reports the effective config, with or without the server. The monotonic deadline
// decides that it has come: the layer is told at least refreshAt, should the wall clock have
// moved back since. Under a.mu.
func (a *agent) refresh(now time.Time) {
	a.decideAgain(now, max(unixMs(now), a.refreshAt))
}

// decideAgain asks the layer what is in force at ms under the host as it is now, puts it in
// force, and reports the effective config. Under a.mu.
func (a *agent) decideAgain(now time.Time, ms uint64) {
	r, err := a.layer.Refresh(context.Background(), a.current, a.atStart.GetCurrent(), a.host(), ms)
	if err != nil {
		slog.Warn("opamp: the settings in force could not be refreshed; asking again in a minute", "error", err)
		a.refreshAfter(ms+60_000, now)
		return
	}
	if r.GetWarning() != "" {
		slog.Warn("opamp: " + r.GetWarning())
	}
	a.controls.Apply(r.GetSettings())
	a.settings = r.GetSettings()
	a.effective = r.GetEffective()
	a.refreshAfter(r.GetRefreshAtUnixMs(), now)
	if a.reporter != nil {
		a.reporter.SetEffectiveConfig(a.effective)
	}
}

func failed(config *protobufs.AgentRemoteConfig, msg string) *protobufs.RemoteConfigStatus {
	return &protobufs.RemoteConfigStatus{LastRemoteConfigHash: config.GetConfigHash(), ErrorMessage: msg,
		Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED}
}

// keyValues is the resource's attributes, typed as OpAMP's KeyValue, arrays as arrays.
func keyValues(res *resource.Resource) []*protobufs.KeyValue {
	var out []*protobufs.KeyValue
	for _, kv := range res.Attributes() {
		out = append(out, &protobufs.KeyValue{Key: string(kv.Key), Value: anyValue(kv.Value)})
	}
	return out
}

func anyValue(v attribute.Value) *protobufs.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &protobufs.AnyValue{Value: &protobufs.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &protobufs.AnyValue{Value: &protobufs.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.BOOLSLICE:
		return array(v.AsBoolSlice(), attribute.BoolValue)
	case attribute.INT64SLICE:
		return array(v.AsInt64Slice(), attribute.Int64Value)
	case attribute.FLOAT64SLICE:
		return array(v.AsFloat64Slice(), attribute.Float64Value)
	case attribute.STRINGSLICE:
		return array(v.AsStringSlice(), attribute.StringValue)
	default:
		return &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v.Emit()}}
	}
}

func array[T any](elems []T, value func(T) attribute.Value) *protobufs.AnyValue {
	a := &protobufs.ArrayValue{}
	for _, e := range elems {
		a.Values = append(a.Values, anyValue(value(e)))
	}
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_ArrayValue{ArrayValue: a}}
}
