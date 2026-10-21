// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
)

// The Logfire SDK policy beside the collector's YAML: the Supervisor says the collector takes it
// (the collector lists the logfirepolicy processor), puts the processor in the pipeline, and the
// collector redacts and filters by level as an SDK under the same policy does. A policy that
// overrides a control the collector can't carry out is FAILED with the control's name, and the
// collector goes on with the one it ran.
func TestSupervisorCarriesOutTheLogfireSDKPolicy(t *testing.T) {
	var status atomic.Pointer[protobufs.RemoteConfigStatus]
	var description atomic.Pointer[protobufs.AgentDescription]
	server := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if m.RemoteConfigStatus != nil {
				status.Store(m.RemoteConfigStatus)
			}
			if m.AgentDescription != nil {
				description.Store(m.AgentDescription)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	s, _ := newSupervisor(t, "sdk_policy", map[string]string{"url": server.addr, "storage_dir": t.TempDir()})
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	waitForSupervisorConnection(server.supervisorConnected, true)

	require.Eventually(t, func() bool {
		for _, kv := range description.Load().GetNonIdentifyingAttributes() {
			if kv.GetKey() == "logfire.opamp.config_format" {
				return kv.GetValue().GetStringValue() == "logfire-sdk-policy/v1"
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "the description never said the collector takes the policy")

	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.log"), filepath.Join(dir, "output.json")
	require.NoError(t, os.WriteFile(input, nil, 0o600))
	collector := fmt.Sprintf(`receivers:
  filelog:
    include: [%q]
    start_at: beginning
    operators:
      - type: json_parser
        parse_to: attributes
        severity: {parse_from: attributes.level}
exporters:
  file:
    path: %q
service:
  pipelines:
    logs: {receivers: [filelog], exporters: [file]}
`, input, output)
	send := func(policy string) []byte {
		files := map[string]*protobufs.AgentConfigObject{
			"collector.yaml":          {Body: []byte(collector)},
			"logfire-sdk-policy.json": {Body: []byte(policy), ContentType: "application/json"},
		}
		h := sha256.Sum256([]byte(collector + policy))
		server.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
			Config: &protobufs.AgentConfigMap{ConfigMap: files}, ConfigHash: h[:]}})
		return h[:]
	}
	waitStatus := func(hash []byte, want protobufs.RemoteConfigStatuses) *protobufs.RemoteConfigStatus {
		var got *protobufs.RemoteConfigStatus
		require.Eventually(t, func() bool {
			got = status.Load()
			return got != nil && bytes.Equal(got.LastRemoteConfigHash, hash) && got.Status == want
		}, 60*time.Second, 100*time.Millisecond, "never %s: %v", want, status.Load())
		return got
	}
	const exportsLocal = `"signal_exports":{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}}`
	policy := `{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"local"},` + exportsLocal +
		`,"log_levels":{"mode":"override","level":"warn"},"attribute_redaction":{"mode":"override","drop":["secret"],"mask":["*token*"]}}`
	waitStatus(send(policy), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)

	lines := `{"level":"info","msg":"below the level","secret":"s","api_token":"t"}` + "\n" +
		`{"level":"error","msg":"kept","secret":"s","api_token":"t","user":"u"}` + "\n"
	require.NoError(t, os.WriteFile(input, []byte(lines), 0o600))
	var records []plog.LogRecord
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(output)
		if err != nil {
			return false
		}
		records = records[:0]
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs([]byte(line))
			if err != nil {
				return false
			}
			for i := range ld.ResourceLogs().Len() {
				for j := range ld.ResourceLogs().At(i).ScopeLogs().Len() {
					lr := ld.ResourceLogs().At(i).ScopeLogs().At(j).LogRecords()
					for k := range lr.Len() {
						records = append(records, lr.At(k))
					}
				}
			}
		}
		return len(records) > 0
	}, 60*time.Second, 200*time.Millisecond, "the collector exported nothing")
	require.Len(t, records, 1, "the info record is below the policy's level")
	attrs := records[0].Attributes().AsRaw()
	require.Equal(t, "kept", attrs["msg"])
	require.NotContains(t, attrs, "secret")
	require.Equal(t, "[REDACTED]", attrs["api_token"])
	require.Equal(t, "u", attrs["user"])

	refused := waitStatus(send(`{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"local"},`+exportsLocal+
		`,"sampling_rules":{"mode":"override","rules":[{"rate":0}]}}`), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
	require.Contains(t, refused.ErrorMessage, "sampling_rules: rules: this SDK cannot apply the requested override")
}
