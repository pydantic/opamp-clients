// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

// ownLogsSink is an OTLP/HTTP logs endpoint that keeps the resources of what it is sent, with the
// Authorization header each came with.
type ownLogsSink struct {
	mu        sync.Mutex
	resources []map[string]any
	auth      []string
}

func (o *ownLogsSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/logs" {
		// The policy's own metrics land here too; only the logs matter.
		w.WriteHeader(http.StatusOK)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = gz
	}
	data, err := io.ReadAll(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req := plogotlp.NewExportRequest()
	if err := req.UnmarshalProto(data); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	rls := req.Logs().ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		if rls.At(i).ScopeLogs().Len() == 0 {
			continue
		}
		o.resources = append(o.resources, rls.At(i).Resource().Attributes().AsRaw())
		o.auth = append(o.auth, r.Header.Get("Authorization"))
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := plogotlp.NewExportResponse().MarshalProto()
	_, _ = w.Write(out)
}

func (o *ownLogsSink) received() ([]map[string]any, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]map[string]any(nil), o.resources...), append([]string(nil), o.auth...)
}

// A server's own_logs offer sends the Collector's own logs there, with the offered headers and
// the instance UID as service.instance.id. A remote config whose service::telemetry exports only
// metrics (and names resource attributes) keeps them flowing: the offer's logs are not replaced.
func TestSupervisorSendsCollectorOwnLogsToTheOffer(t *testing.T) {
	sink := &ownLogsSink{}
	ingest := httptest.NewServer(sink)
	t.Cleanup(ingest.Close)

	var instanceUID sync.Map
	server := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
			instanceUID.Store("uid", uuid.UUID(message.InstanceUid).String())
			return &protobufs.ServerToAgent{}
		},
	})

	s, _ := newSupervisor(t, "basic", map[string]string{"url": server.addr, "storage_dir": t.TempDir()})
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	waitForSupervisorConnection(server.supervisorConnected, true)

	policy := []byte(`receivers:
  nop:
exporters:
  nop:
service:
  telemetry:
    resource:
      logfire.fleet.policy.id: policy-one
    metrics:
      readers:
        - periodic:
            interval: 60000
            exporter:
              otlp:
                protocol: http/protobuf
                endpoint: ` + ingest.URL + `
  pipelines:
    logs:
      receivers: [nop]
      exporters: [nop]
`)
	server.sendToSupervisor(&protobufs.ServerToAgent{
		RemoteConfig: &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: policy}}},
			ConfigHash: []byte("policy-one"),
		},
		ConnectionSettings: &protobufs.ConnectionSettingsOffers{
			Hash: []byte("own-logs"),
			OwnLogs: &protobufs.TelemetryConnectionSettings{
				DestinationEndpoint: ingest.URL + "/v1/logs",
				Headers: &protobufs.Headers{Headers: []*protobufs.Header{
					{Key: "Authorization", Value: "Bearer own-logs-token"},
				}},
			},
		},
	})

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		uid, ok := instanceUID.Load("uid")
		if !assert.True(c, ok) {
			return
		}
		resources, auth := sink.received()
		if !assert.NotEmpty(c, resources, "no Collector logs reached the offered endpoint") {
			return
		}
		found := false
		for i, r := range resources {
			if r["service.instance.id"] == uid && r["logfire.fleet.policy.id"] == "policy-one" && auth[i] == "Bearer own-logs-token" {
				found = true
			}
		}
		assert.True(c, found, "no Collector logs with the instance UID, the policy's resource and the offered header: %v %v", resources, auth)
	}, 30*time.Second, 250*time.Millisecond)
}
