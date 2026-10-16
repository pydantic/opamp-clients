// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Package sdk decides what an OpenTelemetry SDK does with the settings an OpAMP server sends it,
// the same way in every language: the SDK settings layer (a WebAssembly module, checked in
// beside this package), run in wazero. It keeps nothing between calls; the caller keeps the
// settings documents it is handed (Current, and the one it saves) and passes them back. See
// docs/sdk-settings.md.
package sdk

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

//go:embed opamp_sdk_settings.wasm
var wasm []byte

var (
	compileOnce sync.Once
	runtime     wazero.Runtime
	compiled    wazero.CompiledModule
	compileErr  error
)

// Layer is an instance of the settings layer. Safe for concurrent use.
type Layer struct {
	mu                sync.Mutex // mod, closed
	mod               api.Module
	closed            bool
	alloc, free, call api.Function
}

// New instantiates the layer.
func New(ctx context.Context) (*Layer, error) {
	compileOnce.Do(func() {
		runtime = wazero.NewRuntime(context.Background())
		compiled, compileErr = runtime.CompileModule(context.Background(), wasm)
		// The compiler's garbage back to the operating system now, as the core's (core.go).
		debug.FreeOSMemory()
	})
	if compileErr != nil {
		return nil, fmt.Errorf("compile the SDK settings layer: %w", compileErr)
	}
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate the SDK settings layer: %w", err)
	}
	return &Layer{mod: mod, alloc: mod.ExportedFunction("alloc"), free: mod.ExportedFunction("free"),
		call: mod.ExportedFunction("call")}, nil
}

// Close releases the instance, once a call in progress has returned. A call after it fails.
func (l *Layer) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.mod.Close(ctx)
}

// Load is for the start, before the SDK is built: the settings to start with, from the document
// saved at the last change (nil: nothing saved). Put all of them in force, next-start ones too.
// host names the settings the SDK carries out, at once or at the next start (nil: none carried
// out). A name that is no setting is an error. Without the time, the SDK does not carry out debug
// mode: see LoadAt.
func (l *Layer) Load(ctx context.Context, saved []byte, host *settingspb.Host) (*settingspb.Loaded, error) {
	return l.LoadAt(ctx, saved, host, 0)
}

// LoadAt is Load at nowUnixMs, the wall clock in milliseconds since the Unix epoch, which debug
// mode is decided by (0: none).
func (l *Layer) LoadAt(ctx context.Context, saved []byte, host *settingspb.Host, nowUnixMs uint64) (*settingspb.Loaded, error) {
	return l.LoadFrom(ctx, nil, saved, host, nowUnixMs)
}

// LoadFrom is LoadAt with coreState, the OpAMP core's saved state (what it asked the host to store;
// nil: none). The settings then come from the last config applied in it, and saved is read only
// when it holds none (a document kept before the core kept the config applied).
func (l *Layer) LoadFrom(ctx context.Context, coreState, saved []byte, host *settingspb.Host, nowUnixMs uint64) (*settingspb.Loaded, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Load{Load: &settingspb.Load{
		Saved: saved, Host: host, NowUnixMs: nowUnixMs, CoreState: coreState}}})
	if err != nil {
		return nil, err
	}
	return r.GetLoaded(), nil
}

// Decide says how to answer a remote config. current is the document in force (from the last Load
// or Decide), atStart the one loaded at start.
//
// The layer decides what to keep for the next start; save keeps it (whole or not at all, where
// Load reads it then), or returns why it can't; the layer then answers from how that went: a
// config whose save failed is refused, saying why. A nil save: the SDK keeps no state across
// restarts, so a config that changes a next-start setting is refused. A policy that overrides a
// control host does not carry out is refused too.
//
// Without the time, the SDK does not carry out debug mode: see DecideAt.
func (l *Layer) Decide(ctx context.Context, config *protobufs.AgentRemoteConfig, current, atStart []byte,
	host *settingspb.Host, save func(doc []byte) error) (*settingspb.Decision, error) {
	return l.DecideAt(ctx, config, current, atStart, host, 0, save)
}

// DecideAt is Decide at nowUnixMs, as LoadAt.
func (l *Layer) DecideAt(ctx context.Context, config *protobufs.AgentRemoteConfig, current, atStart []byte,
	host *settingspb.Host, nowUnixMs uint64, save func(doc []byte) error) (*settingspb.Decision, error) {
	decide := &settingspb.Decide{Config: config, Current: current, AtStart: atStart, Host: host, NowUnixMs: nowUnixMs}
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Decide{Decide: decide}})
	if err != nil {
		return nil, err
	}
	plan := r.GetPlan()
	if plan == nil {
		return nil, errors.New("SDK settings layer: a decide answered without a plan")
	}
	commit := &settingspb.Commit{Decide: decide}
	if plan.Save != nil && save != nil {
		if err := save(plan.GetSave()); err != nil {
			commit.Saved, commit.Error = settingspb.Commit_SAVED_FAILED, err.Error()
		} else {
			commit.Saved = settingspb.Commit_SAVED_DONE
		}
	}
	r, err = l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Commit{Commit: commit}})
	if err != nil {
		return nil, err
	}
	return r.GetDecision(), nil
}

// Refresh gives what is in force at nowUnixMs, at the time a load, decision or refresh said to ask
// again (RefreshAtUnixMs): the end of debug mode. current and atStart as for Decide.
func (l *Layer) Refresh(ctx context.Context, current, atStart []byte, host *settingspb.Host,
	nowUnixMs uint64) (*settingspb.Refreshed, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Refresh{Refresh: &settingspb.Refresh{
		Current: current, AtStart: atStart, Host: host, NowUnixMs: nowUnixMs}}})
	if err != nil {
		return nil, err
	}
	return r.GetRefreshed(), nil
}

// Health gives export health from the gates' counts since start: whether to send it now, and the
// entry for the agent health's component_health_map["exporters"]. previous is what the last call
// answered (nil at the first), settings those in force.
func (l *Layer) Health(ctx context.Context, counts *settingspb.ExportCounts, previous *settingspb.HealthReport,
	nowUnixMs uint64, settings *settingspb.Settings) (*settingspb.HealthReport, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Health{Health: &settingspb.Health{
		Counts: counts, Previous: previous, NowUnixMs: nowUnixMs, Settings: settings}}})
	if err != nil {
		return nil, err
	}
	return r.GetHealthReport(), nil
}

// Evaluate is for tests: what the layer's reference matchers make of one input under a settings
// policy (how a span is sampled, whether a log record passes the levels, what redaction leaves
// of attributes, what a rate cap lets through), to hold an integration's own matchers to.
func (l *Layer) Evaluate(ctx context.Context, e *settingspb.Evaluate) (*settingspb.Evaluated, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Evaluate{Evaluate: e}})
	if err != nil {
		return nil, err
	}
	return r.GetEvaluated(), nil
}

// Describe gives the agent description from the SDK's resource attributes. It adds the policy
// formats the layer reads and the controls host carries out at nowUnixMs (as LoadAt), so that a
// server knows which overrides this SDK can apply.
func (l *Layer) Describe(ctx context.Context, resource []*protobufs.KeyValue, host *settingspb.Host,
	nowUnixMs uint64) (*protobufs.AgentDescription, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Describe{Describe: &settingspb.Describe{
		Resource: resource, Host: host, NowUnixMs: nowUnixMs}}})
	if err != nil {
		return nil, err
	}
	return r.GetDescription(), nil
}

// Components gives the inventory as the agent's available components, for
// Status.AvailableComponents at start and Client.SetAvailableComponents when it changes: what the
// integration lists of itself, its SDK, the instrumentations it can run and the exporters it built,
// with the settings host carries out at nowUnixMs (as LoadAt) added. The hash is left empty for the
// client to compute (docs/available-components.md).
func (l *Layer) Components(ctx context.Context, inventory *settingspb.Inventory, host *settingspb.Host,
	nowUnixMs uint64) (*protobufs.AvailableComponents, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Components{Components: &settingspb.Components{
		Inventory: inventory, Host: host, NowUnixMs: nowUnixMs}}})
	if err != nil {
		return nil, err
	}
	return r.GetAvailableComponents(), nil
}

func (l *Layer) do(ctx context.Context, req *settingspb.Request) (*settingspb.Reply, error) {
	in, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("SDK settings layer: closed")
	}
	res, err := l.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return nil, fmt.Errorf("SDK settings layer alloc: %w", err)
	}
	ptr := uint32(res[0])
	if !l.mod.Memory().Write(ptr, in) {
		return nil, errors.New("SDK settings layer: input out of range")
	}
	res, err = l.call.Call(ctx, uint64(ptr), uint64(len(in)))
	if err != nil {
		return nil, fmt.Errorf("SDK settings layer trapped: %w", err)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	raw, ok := l.mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, errors.New("SDK settings layer: reply out of range")
	}
	reply := &settingspb.Reply{}
	err = proto.Unmarshal(raw, reply) // Read returns a view: decode before freeing
	if _, ferr := l.free.Call(ctx, uint64(outPtr), uint64(outLen)); ferr != nil && err == nil {
		err = ferr
	}
	if err != nil {
		return nil, fmt.Errorf("SDK settings layer reply: %w", err)
	}
	if e, ok := reply.Result.(*settingspb.Reply_Error); ok {
		return nil, fmt.Errorf("SDK settings layer: %s", e.Error)
	}
	return reply, nil
}
