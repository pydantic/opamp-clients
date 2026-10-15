// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package statekey

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const autoKind = "keychain"

// keychainService names the Keychain items; the account is the state file's absolute path.
const keychainService = "opamp-state-key"

// security runs /usr/bin/security with stdin; a var so tests can fake it.
var security = func(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// keychain reads the key from the login (or, for a daemon, System) Keychain, or adds a new one.
// The new key goes through stdin (security -i), never on a command line others could see.
func keychain(stateFile string) ([]byte, error) {
	account, err := filepath.Abs(stateFile)
	if err != nil {
		return nil, err
	}
	out, err := security("", "find-generic-password", "-s", keychainService, "-a", account, "-w")
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(out)))
		if derr != nil || len(key) != KeySize {
			return nil, errors.New("statekey: the Keychain item " + keychainService + " for " + account + " isn't a key")
		}
		return key, nil
	}
	var exit *exec.ExitError
	// 44: errSecItemNotFound.
	if !errors.As(err, &exit) || exit.ExitCode() != 44 {
		return nil, fmt.Errorf("statekey: keychain: %w", err)
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("add-generic-password -s %s -a %q -w %s\n", keychainService, account, hex.EncodeToString(key))
	if _, err := security(cmd, "-i"); err != nil {
		return nil, fmt.Errorf("statekey: keychain: adding the key: %w", err)
	}
	return key, nil
}

func dpapi(string) ([]byte, error) { return nil, errors.New("statekey: dpapi is Windows only") }
