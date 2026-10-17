// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !darwin && !windows

package statekey

import "errors"

const autoKind = "systemd-creds"

func keychain(string) ([]byte, error) { return nil, errors.New("statekey: keychain is macOS only") }

func dpapi(string) ([]byte, error) { return nil, errors.New("statekey: dpapi is Windows only") }
