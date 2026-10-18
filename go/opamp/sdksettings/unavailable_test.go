// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package sdksettings

import (
	"context"
	"sync"
	"testing"
)

// needsAnImport is a module that compiles but can't be instantiated: it asks for an import
// nobody gives.
var needsAnImport = []byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 1, 4, 1, 0x60, 0, 0, 2, 7, 1, 1, 0x6d, 1, 0x66, 0, 0}

func TestALayerModuleThatCantBeLoadedIsUnavailable(t *testing.T) {
	ctx := context.Background()
	if err := Unavailable(ctx); err != nil {
		t.Fatalf("the layer in place: %v", err)
	}
	// The embedded module swapped for one that is missing, not a module, or won't instantiate,
	// then put back as it was, compiled.
	was, wasRuntime, wasCompiled := wasm, runtime, compiled
	t.Cleanup(func() {
		wasm, runtime, compiled, compileErr = was, wasRuntime, wasCompiled, nil
		compileOnce = sync.Once{}
		compileOnce.Do(func() {})
	})
	for name, module := range map[string][]byte{"missing": nil, "not a module": []byte("not a module"),
		"needs an import": needsAnImport} {
		wasm, compileOnce = module, sync.Once{}
		if err := Unavailable(ctx); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
