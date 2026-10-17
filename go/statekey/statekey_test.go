// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package statekey

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustSealer(t *testing.T, key byte) *Sealer {
	t.Helper()
	s, err := New(bytes.Repeat([]byte{key}, KeySize), "test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A sealed state opens to what was sealed, and holds none of it in the clear.
func TestSealOpen(t *testing.T) {
	s := mustSealer(t, 1)
	state := []byte("Authorization: Bearer issued")
	sealed, err := s.Seal(state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("Bearer")) {
		t.Fatal("the sealed file holds the credential in the clear")
	}
	if !bytes.HasPrefix(sealed, magic) {
		t.Fatal("no magic")
	}
	again, _ := s.Seal(state)
	if bytes.Equal(sealed, again) {
		t.Fatal("two seals of one state are the same: the nonce isn't fresh")
	}
	got, err := s.Open(sealed)
	if err != nil || !bytes.Equal(got, state) {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

// Another key, a changed byte anywhere, or a cut file is refused, not read as garbage.
func TestOpenRefuses(t *testing.T) {
	s := mustSealer(t, 1)
	sealed, _ := s.Seal([]byte("state"))
	if _, err := mustSealer(t, 2).Open(sealed); err == nil {
		t.Fatal("another key opened it")
	}
	for i := len(magic); i < len(sealed); i++ {
		changed := append([]byte(nil), sealed...)
		changed[i] ^= 1
		if _, err := s.Open(changed); err == nil {
			t.Fatalf("a change at byte %d went unnoticed", i)
		}
	}
	if _, err := s.Open(sealed[:len(magic)+4]); err == nil {
		t.Fatal("a cut file opened")
	}
}

// A state saved before sealing was turned on is taken as it is, so its credentials survive.
func TestOpenTakesUnsealedState(t *testing.T) {
	got, err := mustSealer(t, 1).Open([]byte{0x08, 0x01})
	if err != nil || !bytes.Equal(got, []byte{0x08, 0x01}) {
		t.Fatalf("Open = %v, %v", got, err)
	}
}

func TestNewRefusesShortKey(t *testing.T) {
	if _, err := New(make([]byte, 16), "x"); err == nil {
		t.Fatal("a 16-byte key was taken")
	}
}

// file: makes a key readable by its owner only the first time, and reads it back after.
func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.key")
	first, err := Load("file:"+path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %o", info.Mode().Perm())
	}
	sealed, _ := first.Seal([]byte("state"))
	second, err := Load("file:"+path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.Open(sealed); err != nil || string(got) != "state" {
		t.Fatalf("the key read back doesn't open what the first sealed: %q, %v", got, err)
	}
}

func TestKeyFileRefusesOthersReading(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the directory's ACL decides on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(make([]byte, KeySize))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("file:"+path, ""); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable key file was taken: %v", err)
	}
}

func TestKeyFileRefusesNotAKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("file:"+path, ""); err == nil {
		t.Fatal("garbage was taken as a key")
	}
}

// systemd-creds reads the credential the unit loaded, raw or base64, and is never made here.
func TestSystemdCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if _, err := Load("systemd-creds", ""); err == nil {
		t.Fatal("a missing credential was made up")
	}
	raw := bytes.Repeat([]byte{7}, KeySize)
	if err := os.WriteFile(filepath.Join(dir, defaultCredential), raw, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "named"), []byte(base64.StdEncoding.EncodeToString(raw)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	a, err := Load("systemd-creds", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load("systemd-creds:named", "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := a.Seal([]byte("state"))
	if _, err := b.Open(sealed); err != nil {
		t.Fatal("the raw and base64 forms of one key differ")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := Load("systemd-creds", ""); err == nil || !strings.Contains(err.Error(), "LoadCredentialEncrypted") {
		t.Fatalf("no credentials directory: %v", err)
	}
}

func TestLoadRefusesUnknownSpecs(t *testing.T) {
	for _, spec := range []string{"", "file:", "vault", "auto:x"} {
		if _, err := Load(spec, "state"); err == nil {
			t.Errorf("%q was taken", spec)
		}
	}
}
