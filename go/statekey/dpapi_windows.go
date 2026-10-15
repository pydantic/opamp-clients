// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package statekey

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const autoKind = "dpapi"

// dpapiEntropy ties the protected blob to this use.
var dpapiEntropy = []byte("opamp-state-key")

// dpapi reads the key protected with DPAPI for this user at path, or makes and protects a new
// one there: only this user (on this machine) can unprotect it.
func dpapi(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return makeKeyFile(path, protect)
	}
	if err != nil {
		return nil, fmt.Errorf("statekey: %w", err)
	}
	key, err := unprotect(blob)
	if err != nil {
		return nil, fmt.Errorf("statekey: dpapi: %s: %w", path, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("statekey: dpapi: %s isn't a key", path)
	}
	return key, nil
}

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func take(out *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

func protect(key []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob(key), nil, blob(dpapiEntropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("statekey: dpapi: %w", err)
	}
	return take(&out), nil
}

func unprotect(b []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(b), nil, blob(dpapiEntropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return take(&out), nil
}

func keychain(string) ([]byte, error) { return nil, errors.New("statekey: keychain is macOS only") }
