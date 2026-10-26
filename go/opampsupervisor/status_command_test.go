// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor"
)

func writeStatusFile(t *testing.T, dir string, st supervisor.Status) {
	t.Helper()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, supervisor.StatusFile), b, 0o600))
}

// `status` reads the status file of the storage directory the config names: 0 when all is
// well, 1 when not, 2 with nothing to read.
func TestStatusCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("server:\n  endpoint: wss://opamp.example.com/v1/opamp\nstorage:\n  directory: "+dir+"\n"), 0o600))
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := statusCommand(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, _, errOut := run("--config", cfg)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "hasn't run on this config yet")

	now := time.Now()
	well := supervisor.Status{PID: os.Getpid(), Started: now, Updated: now, Endpoint: "wss://opamp.example.com/v1/opamp",
		Server:    &supervisor.ServerStatus{Connected: true, Since: now},
		Collector: &supervisor.CollectorStatus{Healthy: true, Since: now}}
	writeStatusFile(t, dir, well)
	code, out, _ := run("--config", cfg)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "connected since")
	assert.Contains(t, out, "healthy since")

	unwell := well
	unwell.Server = &supervisor.ServerStatus{Since: now, Error: "dial tcp: lookup opamp.example.com: no such host"}
	writeStatusFile(t, dir, unwell)
	code, out, _ = run("--storage", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "no such host")
	assert.Contains(t, out, "check server.endpoint")

	code, out, _ = run("--storage", dir, "--json")
	assert.Equal(t, 1, code)
	var st supervisor.Status
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	assert.False(t, st.Server.Connected)

	code, _, errOut = run("--config", filepath.Join(t.TempDir(), "missing.yaml"))
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "--storage DIR")
}
