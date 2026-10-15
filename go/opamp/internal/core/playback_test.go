// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// playbackCase is one session of the core's proof playback: a cover's input from the model
// checker's concrete playback, as the bytes a host hands the core, and the native core's replies.
type playbackCase struct {
	Name   string `json:"name"`
	Config string `json:"config"`
	Init   string `json:"init"`
	Steps  []struct {
		Step  string `json:"step"`
		Reply string `json:"reply"`
	} `json:"steps"`
}

// rawCall runs fn on in as Core.call does, and returns the encoded Reply as the module gave it.
func rawCall(ctx context.Context, t *testing.T, mod api.Module, fn string, in []byte) []byte {
	t.Helper()
	res, err := mod.ExportedFunction("alloc").Call(ctx, uint64(len(in)))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, in) {
		t.Fatal("the input out of range")
	}
	res, err = mod.ExportedFunction(fn).Call(ctx, uint64(ptr), uint64(len(in)))
	if err != nil {
		t.Fatalf("%s trapped: %v", fn, err)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	view, ok := mod.Memory().Read(outPtr, outLen)
	if !ok {
		t.Fatal("the reply out of range")
	}
	out := bytes.Clone(view)
	if _, err := mod.ExportedFunction("free").Call(ctx, uint64(outPtr), uint64(outLen)); err != nil {
		t.Fatalf("free: %v", err)
	}
	return out
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The shipped module (the one this package embeds, every SDK's copy the same bytes) gives each
// session of the kernel proofs' cover inputs exactly the replies the native build of the same
// source gave, on wazero's compiler and on its interpreter. The sessions come from the core's
// tooling, which writes them (OPAMP_KANI_PLAYBACK_CASES) and runs this.
func TestKaniPlayback(t *testing.T) {
	path := os.Getenv("OPAMP_KANI_PLAYBACK_CASES")
	if path == "" {
		t.Skip("OPAMP_KANI_PLAYBACK_CASES is not set: the core's tooling writes the sessions and runs this")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []playbackCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatalf("no sessions in %s", path)
	}
	ctx := context.Background()
	engines := []struct {
		name string
		cfg  wazero.RuntimeConfig
	}{{"compiler", wazero.NewRuntimeConfig()}, {"interpreter", wazero.NewRuntimeConfigInterpreter()}}
	for _, e := range engines {
		engine := e.name
		rt := wazero.NewRuntimeWithConfig(ctx, e.cfg)
		defer func() { _ = rt.Close(ctx) }()
		compiled, err := rt.CompileModule(ctx, wasm)
		if err != nil {
			t.Fatal(err)
		}
		steps, differ := 0, 0
		for _, c := range cases {
			mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := rawCall(ctx, t, mod, "init", unhex(t, c.Config)), unhex(t, c.Init); !bytes.Equal(got, want) {
				t.Errorf("%s, %s: init replied %x, the native core %x", engine, c.Name, got, want)
				differ++
			}
			for i, s := range c.Steps {
				got, want := rawCall(ctx, t, mod, "step", unhex(t, s.Step)), unhex(t, s.Reply)
				if !bytes.Equal(got, want) {
					t.Errorf("%s, %s: step %d replied %x, the native core %x", engine, c.Name, i, got, want)
					differ++
				}
				steps++
			}
			_ = mod.Close(ctx)
		}
		t.Logf("%s: %d sessions, %d steps, %d replies other than the native core's", engine, len(cases), steps, differ)
	}
}
