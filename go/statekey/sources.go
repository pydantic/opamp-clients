// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package statekey

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultCredential is the systemd credential's name when the spec gives none.
const defaultCredential = "opamp-state-key"

// systemdCredential reads the service's credential name from $CREDENTIALS_DIRECTORY.
func systemdCredential(name string) ([]byte, error) {
	if name == "" {
		name = defaultCredential
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return nil, fmt.Errorf("statekey: systemd-creds: $CREDENTIALS_DIRECTORY isn't set (give the unit LoadCredentialEncrypted=%s:...)", name)
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("statekey: systemd-creds: %w", err)
	}
	return decodeKey(b, "systemd credential "+name)
}

// decodeKey takes a key as raw bytes, or as base64 text (what `systemd-creds encrypt` of
// `openssl rand -base64 32` holds).
func decodeKey(b []byte, what string) ([]byte, error) {
	if len(b) == KeySize {
		return b, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != KeySize {
		return nil, fmt.Errorf("statekey: %s isn't a %d-byte key (raw or base64)", what, KeySize)
	}
	return key, nil
}

// keyFile reads the key at path, or makes it (readable by this user only) if there is none.
func keyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return makeKeyFile(path, func(key []byte) ([]byte, error) {
			return []byte(base64.StdEncoding.EncodeToString(key) + "\n"), nil
		})
	}
	if err != nil {
		return nil, fmt.Errorf("statekey: %w", err)
	}
	if err := privateFile(path); err != nil {
		return nil, err
	}
	return decodeKey(b, "key file "+path)
}

// makeKeyFile makes a new key and writes encode(key) to path, new (never over another file),
// readable by this user only, flushed before the key is used: a key that didn't reach the disk
// would leave a state sealed with it unreadable after a crash.
func makeKeyFile(path string, encode func([]byte) ([]byte, error)) (key []byte, err error) {
	if key, err = newKey(); err != nil {
		return nil, err
	}
	data, err := encode(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("statekey: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
			key = nil
		}
	}()
	if _, err = f.Write(data); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	return key, syncDir(filepath.Dir(path))
}

// privateFile refuses a key file others can read or write (not checked on Windows, where the
// directory's ACL decides).
func privateFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("statekey: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("statekey: key file %s is mode %o: it must be readable by its owner only (chmod 600)", path, info.Mode().Perm())
	}
	return nil
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
