// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package statekey

import (
	"os/exec"
	"strings"
	"testing"
)

// The Keychain flow against a fake security: the first Load adds a key (through stdin, never
// argv), the second finds it.
func TestKeychain(t *testing.T) {
	items := map[string]string{}
	security = func(stdin string, args ...string) ([]byte, error) {
		switch args[0] {
		case "find-generic-password":
			if v, ok := items[args[4]]; ok {
				return []byte(v + "\n"), nil
			}
			return nil, exec.Command("sh", "-c", "exit 44").Run()
		case "-i":
			f := strings.Fields(stdin)
			if strings.Contains(strings.Join(args, " "), f[len(f)-1]) {
				t.Fatal("the key was on the command line")
			}
			items[strings.Trim(f[4], `"`)] = f[6]
			return nil, nil
		}
		t.Fatalf("unexpected security %v", args)
		return nil, nil
	}
	a, err := Load("keychain", "/var/lib/agent/state")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load("auto", "/var/lib/agent/state")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := a.Seal([]byte("state"))
	if _, err := b.Open(sealed); err != nil {
		t.Fatal("the key found isn't the key added")
	}
}
