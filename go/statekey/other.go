// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build !darwin && !windows

package statekey

import "errors"

const autoKind = "systemd-creds"

func keychain(string) ([]byte, error) { return nil, errors.New("statekey: keychain is macOS only") }

func dpapi(string) ([]byte, error) { return nil, errors.New("statekey: dpapi is Windows only") }
