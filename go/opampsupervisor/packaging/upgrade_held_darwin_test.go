// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && darwin

package packaging

import "testing"

// holdForUpgrade holds nothing on macOS: the installer shuts no program down.
func holdForUpgrade(t *testing.T) func() { return func() {} }

// installUpgrade installs OPAMP_PKG as any install does.
func installUpgrade(t *testing.T) { install(t) }
