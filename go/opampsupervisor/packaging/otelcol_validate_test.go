// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build repo

package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The bundled collector (build.sh's) takes a config of the kind pydantic-otelcol-configure
// installs, reading the token from where systemd decrypts it.
func TestOtelcolValidatesAnUnmanagedConfig(t *testing.T) {
	token := filepath.Join(t.TempDir(), "otelcol-token")
	require.NoError(t, os.WriteFile(token, []byte("lf-token_123"), 0o600))
	config := writeConfig(t, strings.ReplaceAll(unmanagedConfig,
		"/run/credentials/pydantic-otelcol.service/otelcol-token", token))
	must(t, nil, "dist/otelcol/otelcol", "validate", "--config", config)
}
