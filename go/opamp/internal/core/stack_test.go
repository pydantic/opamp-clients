// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package core

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// The most frames the core may have on the engine's stack at once: the budget the core's own
// stack check proves the module stays within before each release. wazero's interpreter, which Go
// runs the core on where wazero has no compiler (GOARCHes other than amd64 and arm64), traps at
// 2,000 (callStackCeiling in wazero 1.12).
const depthBudget = 1800

// frameCounter counts the core's frames on the stack, and the most there were at once.
type frameCounter struct{ now, most int }

func (f *frameCounter) Before(context.Context, api.Module, api.FunctionDefinition, []uint64, experimental.StackIterator) {
	f.now++
	f.most = max(f.most, f.now)
}
func (f *frameCounter) After(context.Context, api.Module, api.FunctionDefinition, []uint64) { f.now-- }
func (f *frameCounter) Abort(context.Context, api.Module, api.FunctionDefinition, error)    { f.now-- }

// interpreted starts the embedded core on wazero's interpreter, as on a GOARCH without wazero's
// compiler; with frames non-nil, every call into the core counts its frames there.
func interpreted(t *testing.T, frames *frameCounter) *Core {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	cctx := ctx
	if frames != nil {
		cctx = experimental.WithFunctionListenerFactory(ctx, experimental.FunctionListenerFactoryFunc(
			func(api.FunctionDefinition) experimental.FunctionListener { return frames }))
	}
	compiled, err := rt.CompileModule(cctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		t.Fatal(err)
	}
	c := &Core{mod: mod, alloc: mod.ExportedFunction("alloc"), free: mod.ExportedFunction("free"),
		init: mod.ExportedFunction("init"), st: mod.ExportedFunction("step")}
	cfg := &corepb.Config{InstanceUid: bytes.Repeat([]byte{7}, 16), Endpoint: "http://server/v1/opamp"}
	if _, err := c.call(ctx, c.init, tapInitIn, cfg); err != nil {
		t.Fatal(err)
	}
	return c
}

// deepValue is an AnyValue n arrays deep. With replaced set, its outermost value comes twice, an
// array and then a string, so decoding it replaces (drops) the whole array partway through: the
// deepest path the core's stack check finds runs through decoding a component's metadata and
// dropping a value as deep as prost decodes.
func deepValue(n int, replaced bool) *protobufs.AnyValue {
	v := &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "leaf"}}
	for range n {
		v = &protobufs.AnyValue{Value: &protobufs.AnyValue_ArrayValue{ArrayValue: &protobufs.ArrayValue{Values: []*protobufs.AnyValue{v}}}}
	}
	if replaced {
		// string_value (field 1), after the array (field 5) in the encoding: prost takes the last
		// of a oneof's fields, dropping the one before.
		v.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "x"))
	}
	return v
}

func deepHealth(n int) *protobufs.ComponentHealth {
	h := &protobufs.ComponentHealth{Healthy: true, Status: "leaf"}
	for i := range n {
		h = &protobufs.ComponentHealth{Healthy: true, ComponentHealthMap: map[string]*protobufs.ComponentHealth{
			fmt.Sprintf("c%d", i): h, "sibling": {Status: "ok"},
		}}
	}
	return h
}

func deepComponents(n int, metadataDepth int) *protobufs.AvailableComponents {
	meta := []*protobufs.KeyValue{{Key: "k", Value: deepValue(metadataDepth, true)}}
	d := &protobufs.ComponentDetails{Metadata: meta}
	for i := range n {
		d = &protobufs.ComponentDetails{Metadata: meta, SubComponentMap: map[string]*protobufs.ComponentDetails{fmt.Sprintf("c%d", i): d}}
	}
	return &protobufs.AvailableComponents{Components: map[string]*protobufs.ComponentDetails{"root": d}}
}

// deepest is the deepest of build(n) the core still decodes (prost's limit of 100 nested messages),
// and that the next is refused for that reason.
func deepest(t *testing.T, c *Core, build func(n int) *corepb.Input) *corepb.Input {
	t.Helper()
	for n := 64; n > 0; n-- {
		_, err := c.Step(context.Background(), 0, 0, build(n))
		if err == nil {
			if n == 64 {
				t.Fatal("64 levels deep decoded: want the deepest that decodes")
			}
			return build(n)
		}
		if !strings.Contains(err.Error(), "recursion limit") {
			t.Fatalf("%d deep: %v", n, err)
		}
	}
	t.Fatal("nothing decodes")
	return nil
}

// TestTheInterpreterRunsTheDeepestMessages runs the core on wazero's interpreter with the inputs
// that nest deepest: each status tree and value as deep as the core decodes, a component's
// metadata value replaced partway through its decoding, and then the first report, which clones,
// sizes and encodes them all. None may trap, and none may take the core past the frames
// the core's stack check proves it within (and so not near the interpreter's 2,000).
func TestTheInterpreterRunsTheDeepestMessages(t *testing.T) {
	ctx := context.Background()
	inputs := func(c *Core) []*corepb.Input {
		return []*corepb.Input{
			deepest(t, c, func(n int) *corepb.Input {
				return &corepb.Input{Kind: &corepb.Input_SetAvailableComponents{SetAvailableComponents: deepComponents(1, n)}}
			}),
			deepest(t, c, func(n int) *corepb.Input {
				return &corepb.Input{Kind: &corepb.Input_SetAvailableComponents{SetAvailableComponents: deepComponents(n, 1)}}
			}),
			deepest(t, c, func(n int) *corepb.Input {
				return &corepb.Input{Kind: &corepb.Input_SetHealth{SetHealth: deepHealth(n)}}
			}),
			deepest(t, c, func(n int) *corepb.Input {
				return &corepb.Input{Kind: &corepb.Input_SetDescription{SetDescription: &protobufs.AgentDescription{
					IdentifyingAttributes: []*protobufs.KeyValue{{Key: "k", Value: deepValue(n, true)}},
				}}}
			}),
			{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}},
		}
	}
	// As a host runs it, without a listener: nothing traps.
	plain := interpreted(t, nil)
	for _, in := range inputs(plain) {
		if _, err := plain.Step(ctx, 1, 0, in); err != nil {
			t.Fatalf("%T: %v", in.Kind, err)
		}
	}
	// Counted: the deepest any of them goes, and the next deeper of each, which prost refuses.
	frames := &frameCounter{}
	c := interpreted(t, frames)
	for _, in := range inputs(c) {
		out, err := c.Step(ctx, 1, 0, in)
		if err != nil {
			t.Fatalf("%T: %v", in.Kind, err)
		}
		if in.GetStart() != nil && (len(out.Effects) == 0 || out.Effects[0].GetHttpRequest() == nil) {
			t.Fatalf("no first report: %v", out.Effects)
		}
	}
	t.Logf("the deepest messages took the core %d frames deep (budget %d)", frames.most, depthBudget)
	// Each of the 40-odd levels of the deepest takes a frame or more to decode: fewer than 64 in
	// all would mean the messages were not as deep as they are meant to be.
	if frames.most < 64 {
		t.Fatalf("only %d frames deep: the messages are not deep", frames.most)
	}
	if frames.most > depthBudget {
		t.Fatalf("%d frames deep, over the %d the core's stack check proves", frames.most, depthBudget)
	}
}
