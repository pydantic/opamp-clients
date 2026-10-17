// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package opampextension is the OpenTelemetry Collector's OpAMP extension on the shared core
// (go/opamp, in Go mode) instead of opamp-go: upstream's extension (opentelemetry-collector-contrib's
// extension/opampextension), with its config, its component type and its custom-message API, so
// that a collector built with it in place of upstream's runs the same config. What changes is the
// client underneath, and what that brings: with state_file the instance UID is kept across
// restarts, and with accepts_opamp_connection_settings the server can issue new credentials,
// which are verified before they replace the old ones and kept in the state file. See README.md.
package opampextension
