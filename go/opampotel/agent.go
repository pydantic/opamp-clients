// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
// description lists only the controls named. The host names every live setting, whatever Setup
// built: the sampler wraps whichever one OTEL_TRACES_SAMPLER names, and every exporter it can
// reach goes through a gate. What the gates can't reach (autoexport's own metrics reader, a
// global provider the application sets in place of Setup's) is reported ungoverned, never a
// reason to declare less (contracts' "What a policy governs"). The instrumentation switches are
// at once too, so nothing waits for the next start.
func (s *server) host() *settingspb.Host {
	return &settingspb.Host{Live: append([]string{"deactivate_instrumentations", "deactivate_all_instrumentations",
		"send_metrics"}, carried...)}
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

// agent puts the SDK under the server: one per Setup. What it does around the settings is the
// layer's agent flow (docs/sdk-settings.md, "The agent flow"): it tells the flow what happened
// and carries out what the flow says, in order.
type agent struct {
	server   *server
	controls *Controls
	layer    *sdksettings.Layer
	uid      uuid.UUID
	base     time.Time // the monotonic clock's zero

	mu        sync.Mutex // everything below it but client
	flow      []byte     // the flow's state, as the layer last gave it
	effective *protobufs.EffectiveConfig
	settings  *settingspb.Settings // the settings last put in force
	gen       uint64               // bumped at each arming: a timer armed before a newer one is nothing
	timer     stopper
	stopped   bool
	reporter  reporter // the client, once there is one

	// now and after are the clock and the timer (a test's own in tests).
	now   func() time.Time
	after func(time.Duration, func()) stopper

	client *opamp.Client

	// The inventory: the exporters Setup built as found (set before start), those the layer
	// decided are listed (decide), the warnings said, and the generation of scopes last listed
	// (under mu).
	found     []*settingspb.FoundExporter
	exporters []*settingspb.Exporter
	warned    map[string]bool
	listed    uint64
	// relist: the exporters changed before there was a client to tell; start sends the inventory.
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

// reporter is where the flow's answers go: the client.
type reporter interface {
	SetEffectiveConfig(*protobufs.EffectiveConfig)
	SetRemoteConfigStatus(*protobufs.RemoteConfigStatus)
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
	a := &agent{server: s, controls: controls, layer: layer, now: now, after: after, globals: globalProviders}
	changed := func() { go a.scopesChanged() }
	controls.scopes.changed.Store(&changed)
	var coreState []byte
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
			slog.Warn("opamp: the state directory is unusable", "error", err)
		}
		// The settings to start with are the last config applied, which the client keeps in its
		// state (opamp-state). Read as the client reads it at start: after a save a client stopped
		// in this process left writing (a config applied just before a Shutdown) has landed, or
		// this start would begin from the config before.
		coreState, err = opamp.ReadState(filepath.Join(s.stateDir, "opamp-state"))
		if err != nil {
			slog.Warn("opamp: the client's saved state unreadable, using the SDK's own settings", "error", err)
		}
	}
	a.base = a.now()
	a.uid = identity(s.stateDir, resourceAttrs, a.base)
	h := s.hints
	out, err := layer.Start(ctx, &settingspb.AgentStart{
		Host:        s.host(),
		StateDir:    s.stateDir != "",
		CoreState:   coreState,
		Endpoint:    s.endpoint,
		InstanceUid: a.uid[:],
		Hints: &settingspb.AgentHints{Refused: h.GetRefused(), NotFound: h.GetNotFound(),
			Unreachable: h.GetUnreachable(), Error: h.GetError()},
		NowUnixMs: unixMs(a.base),
		NowMonoMs: 0,
	})
	if err != nil {
		_ = layer.Close(ctx)
		return nil, err
	}
	a.mu.Lock()
	a.carryOut(out)
	a.mu.Unlock()
	return a, nil
}

// step tells the flow what happened, and carries out what it says: the remote config status and
// the health it gave, if any. Under a.mu.
func (a *agent) step(input func(*settingspb.AgentStep)) (*protobufs.RemoteConfigStatus, *protobufs.ComponentHealth) {
	now := a.now()
	st := &settingspb.AgentStep{Agent: a.flow, NowUnixMs: unixMs(now), NowMonoMs: a.monoMs(now)}
	input(st)
	out, err := a.layer.Step(context.Background(), st)
	if err != nil {
		slog.Warn("opamp: the settings layer failed", "error", err)
		return nil, nil
	}
	return a.carryOut(out)
}

// monoMs is now on the monotonic clock, in milliseconds since the agent started.
func (a *agent) monoMs(now time.Time) uint64 {
	if d := now.Sub(a.base); d > 0 {
		return uint64(d.Milliseconds())
	}
	return 0
}

// carryOut carries out the flow's effects in order, and arms the timer for its next wake. Under
// a.mu.
func (a *agent) carryOut(out *settingspb.AgentOutput) (status *protobufs.RemoteConfigStatus, health *protobufs.ComponentHealth) {
	a.flow, a.effective = out.GetAgent(), out.GetEffective()
	for _, e := range out.GetEffects() {
		switch e := e.GetEffect().(type) {
		case *settingspb.AgentEffect_Apply:
			a.controls.Apply(e.Apply)
			a.settings = e.Apply
		case *settingspb.AgentEffect_Effective:
			if a.reporter != nil {
				a.reporter.SetEffectiveConfig(e.Effective)
			}
		case *settingspb.AgentEffect_Status:
			status = e.Status
			if a.reporter != nil {
				a.reporter.SetRemoteConfigStatus(e.Status)
			}
		case *settingspb.AgentEffect_Health:
			health = e.Health
			if a.reporter != nil {
				a.reporter.SetHealth(e.Health)
			}
		case *settingspb.AgentEffect_SaveUid:
			// The client uses the UID from now on; the next start puts it in the telemetry too.
			if err := write(filepath.Join(a.server.stateDir, opamp.SetupUIDFile), []byte(e.SaveUid)); err != nil {
				a.step(func(s *settingspb.AgentStep) {
					s.Input = &settingspb.AgentStep_UidNotSaved{UidNotSaved: err.Error()}
				})
			}
		case *settingspb.AgentEffect_Log:
			say(e.Log)
		}
	}
	a.arm(out.GetWakeAtMonoMs())
	return status, health
}

// say logs a line of the flow's, prefixed as this package's own.
func say(l *settingspb.AgentLog) {
	msg := "opamp: " + l.GetText()
	switch l.GetLevel() {
	case settingspb.AgentLog_DEBUG:
		slog.Debug(msg)
	case settingspb.AgentLog_INFO, settingspb.AgentLog_NOTICE:
		slog.Info(msg)
	case settingspb.AgentLog_WARN:
		slog.Warn(msg)
	default:
		slog.Error(msg)
	}
}

// arm sets the one timer for wakeMs on the monotonic clock (0: none), in place of any earlier one.
// Under a.mu.
func (a *agent) arm(wakeMs uint64) {
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.gen++
	if a.stopped || wakeMs == 0 {
		return
	}
	gen := a.gen
	wait := time.Duration(0)
	if now := a.monoMs(a.now()); wakeMs > now {
		wait = time.Duration(wakeMs-now) * time.Millisecond
	}
	a.timer = a.after(wait, func() { a.fired(gen) })
}

// fired is the timer going off: the flow asks the layer again when debug mode ends, and builds
// export health when it is due. A timer armed before a newer one is nothing.
func (a *agent) fired(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen || a.stopped {
		return
	}
	counts := a.controls.Counts()
	a.step(func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_Timer{Timer: counts} })
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
		Endpoint:    a.server.endpoint,
		InstanceUID: a.uid,
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
		// Platform mode, asked for by name (an HTTPClient needs it): the agent's requests go
		// through net/http, on a transport of its own that no instrumentation wraps.
		Mode:       opamp.ModePlatform,
		HTTPClient: untraced(),
	}
	if a.server.stateDir != "" {
		cfg.StateFile = filepath.Join(a.server.stateDir, "opamp-state")
	}
	service, _ := res.Set().Value("service.name")
	a.mu.Lock()
	health := a.built(service.AsString())
	effective := a.effective
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
	if a.controls.scopes.gen.Load() != a.listed || a.relist { // an instrumentation came since, or a provider was replaced
		if c := a.components(ctx); c != nil {
			a.reporter.SetAvailableComponents(c)
			a.relist = false
		}
	}
	if a.ours != (providers{}) && !a.stopped {
		a.watchStop = make(chan struct{})
		go a.watch(a.watchStop, watchEvery)
	}
}

// built tells the flow the SDK is built, and the exporters: the first export health, for the
// client's first status. Under a.mu.
func (a *agent) built(service string) *protobufs.ComponentHealth {
	exporters := a.exporters
	a.step(func(s *settingspb.AgentStep) {
		s.Input = &settingspb.AgentStep_Built{Built: &settingspb.AgentBuilt{Service: service, Exporters: exporters}}
	})
	if _, health := a.exportersNow(); health != nil {
		return health
	}
	return &protobufs.ComponentHealth{Healthy: true}
}

// exportersNow tells the flow the exporters listed and the gates' counts now: export health is
// built at once. Under a.mu.
func (a *agent) exportersNow() (*protobufs.RemoteConfigStatus, *protobufs.ComponentHealth) {
	exporters, counts := a.exporters, a.controls.Counts()
	return a.step(func(s *settingspb.AgentStep) {
		s.Input = &settingspb.AgentStep_Exporters{Exporters: &settingspb.AgentExporters{Exporters: exporters, Counts: counts}}
	})
}

// host is what this package carries out: the server's host (server.host), whatever the global
// providers are now.
func (a *agent) host() *settingspb.Host { return a.server.host() }

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
	if n.Kind == opamp.RemoteConfig {
		a.mu.Lock()
		if a.reporter == nil {
			a.reporter = c
		}
		a.mu.Unlock()
		a.remoteConfig(context.Background(), n.RemoteConfig)
		return
	}
	a.tell(c.Endpoint(), n)
}

// tell tells the flow of a notification other than a remote config; inUse is the endpoint the
// client uses now.
func (a *agent) tell(inUse string, n opamp.Notification) {
	var input func(*settingspb.AgentStep)
	switch n.Kind {
	case opamp.Connected:
		input = func(s *settingspb.AgentStep) {
			s.Input = &settingspb.AgentStep_Connected{Connected: &settingspb.AgentConnected{Endpoint: inUse}}
		}
	case opamp.ConnectFailed:
		input = func(s *settingspb.AgentStep) {
			s.Input = &settingspb.AgentStep_ConnectFailed{ConnectFailed: &settingspb.AgentConnectFailed{
				Endpoint: inUse, Status: uint32(max(n.Status, 0)), Detail: n.Detail}}
		}
	case opamp.InstanceUIDChanged:
		input = func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_InstanceUidChanged{InstanceUidChanged: n.InstanceUID[:]} }
	case opamp.StateFromNewerVersion:
		input = func(s *settingspb.AgentStep) {
			s.Input = &settingspb.AgentStep_StateFromNewerVersion{StateFromNewerVersion: n.StateVersion}
		}
	case opamp.Stopped:
		stopped := &settingspb.AgentStopped{}
		if n.Err != nil {
			stopped.Error = n.Err.Error()
		}
		input = func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_Stopped{Stopped: stopped} }
	default:
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.step(input)
}

// remoteConfig tells the flow of a remote config; the answer, which goes to the client too.
func (a *agent) remoteConfig(_ context.Context, config *protobufs.AgentRemoteConfig) *protobufs.RemoteConfigStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A provider replaced since the last look is reported before the decision.
	if a.noticeReplaced() {
		a.reportReplaced()
	}
	status, _ := a.step(func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_RemoteConfig{RemoteConfig: config} })
	if status == nil {
		return failed(config, "the settings layer gave no answer")
	}
	return status
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
