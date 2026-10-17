// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package statekey encrypts an OpAMP client's state file (opamp.Config.StateSealer) under a key
// the operating system keeps: the state holds the credentials a server issued, and this keeps
// them out of backups, disk images and anything else that can read the file but not the key.
//
// The key is a random 256-bit data key, and the file is sealed with AES-256-GCM. Where the key
// lives is a spec (Load): systemd credentials, the macOS Keychain, Windows DPAPI, or a key file.
// Anyone who can act as the agent's user (or root) can still get the key: this protects the
// file at rest, not the agent from itself.
package statekey

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the data key's size in bytes (AES-256).
const KeySize = 32

// magic starts a sealed file (and is its authenticated data): format version 1.
var magic = []byte("OPAMPSK1")

// Sealer seals and opens a state file with one data key (opamp.StateSealer). Open also takes a
// file saved before sealing was turned on, as it is, so turning sealing on keeps the agent's
// credentials; the next save seals them.
type Sealer struct {
	aead cipher.AEAD
	// Source says where the key came from, for logs.
	Source string
}

// New makes a Sealer from a data key of KeySize bytes.
func New(key []byte, source string) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("statekey: the key from %s is %d bytes, not %d", source, len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, Source: source}, nil
}

// Seal encrypts state: magic, a random nonce, then the ciphertext and its tag.
func (s *Sealer) Seal(state []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte{}, magic...), nonce...)
	return s.aead.Seal(out, nonce, state, magic), nil
}

// errWrongKey: a sealed file the key doesn't open (another key, or changed since).
var errWrongKey = errors.New("statekey: the state file can't be opened with this key (another key, or changed since it was saved)")

// Open decrypts a file Seal made. A file without the magic is a state saved before sealing, and
// is returned as it is.
func (s *Sealer) Open(file []byte) ([]byte, error) {
	if !bytes.HasPrefix(file, magic) {
		return file, nil
	}
	rest := file[len(magic):]
	n := s.aead.NonceSize()
	if len(rest) < n+s.aead.Overhead() {
		return nil, errWrongKey
	}
	state, err := s.aead.Open(nil, rest[:n], rest[n:], magic)
	if err != nil {
		return nil, errWrongKey
	}
	return state, nil
}

// Load finds or makes the data key spec names, for the state file at stateFile:
//
//   - "systemd-creds" or "systemd-creds:NAME": the service's credential NAME (default
//     opamp-state-key) from $CREDENTIALS_DIRECTORY, as LoadCredentialEncrypted= gives it: the
//     key encrypted on disk to the host (and TPM, if it has one), and decrypted for the service
//     alone. Made at install (systemd-creds encrypt); never made here.
//   - "keychain": the macOS Keychain (a generic password named after the state file), made
//     the first time.
//   - "dpapi": stateFile + ".key", the key encrypted with Windows DPAPI for the agent's user,
//     made the first time.
//   - "file:PATH": a key file readable by the agent's user only, made the first time.
//   - "auto": keychain on macOS, dpapi on Windows, systemd-creds elsewhere.
func Load(spec, stateFile string) (*Sealer, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	if kind == "auto" && arg == "" {
		kind = autoKind
	}
	var key []byte
	var err error
	switch kind {
	case "systemd-creds":
		key, err = systemdCredential(arg)
	case "file":
		if arg == "" {
			return nil, errors.New(`statekey: "file:" needs a path`)
		}
		key, err = keyFile(arg)
	case "keychain":
		key, err = keychain(stateFile)
	case "dpapi":
		key, err = dpapi(stateFile + ".key")
	default:
		return nil, fmt.Errorf("statekey: unknown key source %q (systemd-creds[:NAME], keychain, dpapi, file:PATH or auto)", spec)
	}
	if err != nil {
		return nil, err
	}
	return New(key, spec)
}

// newKey is a random data key.
func newKey() ([]byte, error) {
	key := make([]byte, KeySize)
	_, err := rand.Read(key)
	return key, err
}
