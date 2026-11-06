// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package sdk decides what an OpenTelemetry SDK does with the settings an OpAMP server sends it,
// the same way in every language: the SDK settings layer (a WebAssembly module, checked in
// beside this package), run in wazero. It keeps nothing between calls: the agent flow's state is
// bytes the caller keeps and passes back. See docs/sdk-settings.md.
package sdksettings

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
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

// Unavailable is why the layer can't be loaded here (its module doesn't compile or doesn't
// instantiate), or nil when it can.
func Unavailable(ctx context.Context) error {
	l, err := New(ctx)
	if err != nil {
		return err
	}
	return l.Close(ctx)
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

// Start begins the agent flow, before the SDK is built: what to put in force from the client's
// saved state (Start.CoreState), and what to do first. The output's Agent is the flow's state:
// pass it back, untouched, with the next Step. Carry out the effects in order, and Step again
// with a Timer at WakeAtMonoMs. See docs/sdk-settings.md, "The agent flow".
func (l *Layer) Start(ctx context.Context, start *settingspb.AgentStart) (*settingspb.AgentOutput, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_AgentStart{AgentStart: start}})
	if err != nil {
		return nil, err
	}
	return r.GetAgentOutput(), nil
}

// Step tells the agent flow one thing that happened (step.Input), with the state the last output
// gave (step.Agent). As Start, carry out the effects in order.
func (l *Layer) Step(ctx context.Context, step *settingspb.AgentStep) (*settingspb.AgentOutput, error) {
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_AgentStep{AgentStep: step}})
	if err != nil {
		return nil, err
	}
	return r.GetAgentOutput(), nil
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
// format the layer reads and the controls host carries out at nowUnixMs, so that a
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
// with the settings host carries out at nowUnixMs added. The hash is left empty for the
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

// Exporters decides, the same in every language, what Fleet is told of the exporters an
// integration found on its SDK's providers: the name each is listed under, where it sends (its own
// endpoint, else the OTEL_EXPORTER_* variables in env, as os.Environ gives them), which signals are
// governed, and a warning for each ungoverned one. Called at start and when the providers change.
func (l *Layer) Exporters(ctx context.Context, found []*settingspb.FoundExporter, env []string,
	defaultProtocol string) (*settingspb.ExportersDecided, error) {
	e := &settingspb.Exporters{Found: found, DefaultProtocol: defaultProtocol}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "OTEL_EXPORTER_") {
			e.Env = append(e.Env, &settingspb.EnvVar{Name: k, Value: v})
		}
	}
	r, err := l.do(ctx, &settingspb.Request{Call: &settingspb.Request_Exporters{Exporters: e}})
	if err != nil {
		return nil, err
	}
	return r.GetExportersDecided(), nil
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
