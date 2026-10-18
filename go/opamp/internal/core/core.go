// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package core runs the shared OpAMP client core (a WebAssembly module, checked in beside this
// package) in wazero, a WebAssembly runtime written in pure Go: no cgo, nothing to
// install. Each Core is one module instance, holding one client's protocol state.
package core

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

//go:embed opamp_core.wasm
var wasm []byte

var (
	compileOnce sync.Once
	runtime     wazero.Runtime
	compiled    wazero.CompiledModule
	compileErr  error

	// Modules other than the embedded core (tests only: Options.Module), compiled once each.
	othersMu sync.Mutex
	others   = map[[sha256.Size]byte]wazero.CompiledModule{}
)

// Core is one instance of the protocol core. Not safe for concurrent use.
type Core struct {
	mod                   api.Module
	alloc, free, init, st api.Function
	tap                   *tap // nil: no tap
}

func compileCore() {
	compileOnce.Do(func() {
		runtime = wazero.NewRuntime(context.Background())
		compiled, compileErr = runtime.CompileModule(context.Background(), wasm)
		// The compiler's garbage (about 12 MB) back to the operating system now, not over the
		// next minutes: a small container would carry it as if it were in use. Once a process.
		debug.FreeOSMemory()
	})
}

// Options override a core's defaults. Production passes the zero value; tests set them.
type Options struct {
	// Module is a WebAssembly module with the core's exports and ABI, run instead of the embedded
	// core (the adversary, testdata/core/opamp_core_adversary.wasm). nil: the embedded core.
	Module []byte
	// Tap is the file to tap this core to, whatever OPAMP_CORE_TAP says. "": as it says.
	Tap string
}

// New starts a core with the given settings and opts. It returns init's output too: the
// instance UID in use from the start (one the core made, if cfg has none).
func New(ctx context.Context, opts Options, cfg *corepb.Config) (*Core, *corepb.Output, error) {
	compileCore()
	if compileErr != nil {
		return nil, nil, fmt.Errorf("compile the OpAMP core: %w", compileErr)
	}
	t := openTap(opts.Tap)
	if opts.Module == nil {
		return instantiate(ctx, compiled, cfg, t)
	}
	module := opts.Module
	sum := sha256.Sum256(module)
	othersMu.Lock()
	m, ok := others[sum]
	if !ok {
		var err error
		if m, err = runtime.CompileModule(context.Background(), module); err != nil {
			othersMu.Unlock()
			t.close()
			return nil, nil, fmt.Errorf("compile the module: %w", err)
		}
		others[sum] = m
	}
	othersMu.Unlock()
	return instantiate(ctx, m, cfg, t)
}

func instantiate(ctx context.Context, compiled wazero.CompiledModule, cfg *corepb.Config, t *tap) (*Core, *corepb.Output, error) {
	// Anonymous instances: any number can run side by side.
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		t.close()
		return nil, nil, fmt.Errorf("instantiate the OpAMP core: %w", err)
	}
	c := &Core{
		mod:   mod,
		alloc: mod.ExportedFunction("alloc"),
		free:  mod.ExportedFunction("free"),
		init:  mod.ExportedFunction("init"),
		st:    mod.ExportedFunction("step"),
		tap:   t,
	}
	out, err := c.call(ctx, c.init, tapInitIn, cfg)
	if err != nil {
		_ = c.Close(ctx)
		return nil, nil, err
	}
	return c, out, nil
}

// Step feeds the core one input at now (milliseconds on the caller's monotonic clock) and unixMs
// (the wall clock, in ms since the Unix epoch: core mode reads Retry-After dates against it).
func (c *Core) Step(ctx context.Context, nowMs, unixMs uint64, in *corepb.Input) (*corepb.Output, error) {
	return c.call(ctx, c.st, tapStepIn, &corepb.Step{NowMs: nowMs, Input: in, UnixMs: unixMs})
}

// FileTap is how the client records the file operations it carries out for this core (kind 5 in
// the tap: one JSON object each, the tap checker's file records), from any goroutine; nil with no tap.
// A record after Close is dropped.
func (c *Core) FileTap() func(op []byte) {
	if c.tap == nil {
		return nil
	}
	return func(op []byte) { c.tap.record(tapFile, op) }
}

// Close releases the instance.
func (c *Core) Close(ctx context.Context) error {
	c.tap.close()
	return c.mod.Close(ctx)
}

// call runs fn on msg; kind is the tap's record kind for what goes in (kind+1 for the reply).
func (c *Core) call(ctx context.Context, fn api.Function, kind byte, msg proto.Message) (*corepb.Output, error) {
	in, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}
	res, err := c.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return nil, fmt.Errorf("OpAMP core alloc: %w", err)
	}
	ptr := uint32(res[0])
	if !c.mod.Memory().Write(ptr, in) {
		return nil, errors.New("OpAMP core: input out of range")
	}
	c.tap.record(kind, in)
	// The core takes ownership of the input buffer.
	res, err = fn.Call(ctx, uint64(ptr), uint64(len(in)))
	if err != nil {
		// A trap: a bug in the core. The instance can't be trusted after it.
		return nil, fmt.Errorf("OpAMP core trapped: %w", err)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	raw, ok := c.mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, errors.New("OpAMP core: reply out of range")
	}
	c.tap.record(kind+1, raw)
	reply := &corepb.Reply{}
	err = proto.Unmarshal(raw, reply) // Read returns a view: decode before freeing
	if _, ferr := c.free.Call(ctx, uint64(outPtr), uint64(outLen)); ferr != nil && err == nil {
		err = ferr
	}
	if err != nil {
		return nil, fmt.Errorf("OpAMP core reply: %w", err)
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("OpAMP core: %s", reply.Error)
	}
	return reply.Output, nil
}

// Core tap: with OPAMP_CORE_TAP=<path> set, every call into the core is appended to a file for the
// core's effects checker (opamp-tap). Each record is a kind byte (1 init in, 2 init out, 3 step in,
// 4 step out), a u32 LE length, a u64 LE monotonic time in nanoseconds, then the bytes exactly as
// they crossed (Config, Reply, Step, Reply). A process's first core writes <path>, each later one
// <path>.<pid>.<n>. Unset, it costs nothing. Records of kind 5 are the client's file operations
// (FileTap), written from the goroutines that save and download: the writes are serialized.
type tap struct {
	mu sync.Mutex
	f  *os.File // nil once closed
}

const (
	tapInitIn byte = 1 // tapInitIn+1: init out
	tapStepIn byte = 3 // tapStepIn+1: step out
	tapFile   byte = 5 // a file operation
)

var (
	// tapped counts the cores created with the tap on, in this process.
	tapped atomic.Uint64
	// tapEpoch is the origin of the tap's clock (Go's clock readings carry the monotonic clock).
	tapEpoch = time.Now()
)

// openTap opens the tap file for a new core, or returns nil: the variable is unset, or the file
// can't be opened (said once on stderr; the core runs without it). to, if set, is the file
// instead (tests).
func openTap(to string) *tap {
	path := to
	if path == "" {
		if path = os.Getenv("OPAMP_CORE_TAP"); path == "" {
			return nil
		}
		if n := tapped.Add(1) - 1; n > 0 {
			path = fmt.Sprintf("%s.%d.%d", path, os.Getpid(), n)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opamp: core tap: %v; running without it\n", err)
		return nil
	}
	return &tap{f: f}
}

// record appends one record, in a single write: a killed process leaves whole records.
func (t *tap) record(kind byte, b []byte) {
	if t == nil {
		return
	}
	rec := make([]byte, 13, 13+len(b))
	rec[0] = kind
	binary.LittleEndian.PutUint32(rec[1:], uint32(len(b)))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return
	}
	// The time is read under the lock: the records' times never go back.
	binary.LittleEndian.PutUint64(rec[5:], uint64(time.Since(tapEpoch)))
	_, _ = t.f.Write(append(rec, b...))
}

func (t *tap) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

// Unavailable is why the core can't be loaded here (its module doesn't compile or doesn't
// instantiate), or nil when it can.
func Unavailable(ctx context.Context) error {
	compileCore()
	if compileErr != nil {
		return fmt.Errorf("compile the OpAMP core: %w", compileErr)
	}
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return fmt.Errorf("instantiate the OpAMP core: %w", err)
	}
	return mod.Close(ctx)
}

// Setup runs the core's Setup call (an SDK integration's start-up decisions): stateless, on an
// instance of its own that it closes. The reply's error is the core's word that the call is
// unusable (a bug in the caller).
func Setup(ctx context.Context, call *corepb.SetupCall) (*corepb.SetupReply, error) {
	compileCore()
	if compileErr != nil {
		return nil, fmt.Errorf("compile the OpAMP core: %w", compileErr)
	}
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate the OpAMP core: %w", err)
	}
	defer mod.Close(ctx)
	in, err := proto.Marshal(call)
	if err != nil {
		return nil, err
	}
	res, err := mod.ExportedFunction("alloc").Call(ctx, uint64(len(in)))
	if err != nil {
		return nil, fmt.Errorf("OpAMP core alloc: %w", err)
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, in) {
		return nil, errors.New("OpAMP core: input out of range")
	}
	// The core takes ownership of the input buffer.
	if res, err = mod.ExportedFunction("setup").Call(ctx, uint64(ptr), uint64(len(in))); err != nil {
		return nil, fmt.Errorf("OpAMP core trapped: %w", err)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	raw, ok := mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, errors.New("OpAMP core: reply out of range")
	}
	reply := &corepb.SetupReply{}
	if err := proto.Unmarshal(raw, reply); err != nil {
		return nil, fmt.Errorf("OpAMP core setup reply: %w", err)
	}
	if e := reply.GetError(); e != "" {
		return nil, fmt.Errorf("OpAMP core: %s", e)
	}
	return reply, nil
}
