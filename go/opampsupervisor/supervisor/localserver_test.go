// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	supervisorTelemetry "github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
)

// newLocalServerTestSupervisor starts the Supervisor's local OpAMP server, the one the
// collector's opamp extension connects to, on a free port.
func newLocalServerTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return newLocalServerTestSupervisorWith(t, &mockOpAMPClient{})
}

func newLocalServerTestSupervisorWith(t *testing.T, opampClient *mockOpAMPClient) *Supervisor {
	t.Helper()
	s := &Supervisor{
		runCtx:                         t.Context(),
		telemetrySettings:              newNopTelemetrySettings(),
		config:                         config.Supervisor{},
		persistentState:                &persistentState{InstanceID: uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")},
		agentDescription:               &atomic.Value{},
		agentConfigOwnTelemetrySection: &atomic.Value{},
		effectiveConfig:                &atomic.Value{},
		opampClient:                    opampClient,
		cfgState:                       &atomic.Value{},
		hasNewConfig:                   make(chan struct{}, 1),
		customMessageToServer:          make(chan *protobufs.CustomMessage, 1),
		doneChan:                       make(chan struct{}),
		localServerToken:               testLocalServerToken,
	}
	require.NoError(t, s.startOpAMPServer())
	t.Cleanup(func() { require.NoError(t, s.opampServer.Stop(context.Background())) })
	return s
}

func localServerURL(s *Supervisor) string {
	return fmt.Sprintf("http://localhost:%d/v1/opamp", s.opampServerPort)
}

// postLocal sends a plain HTTP OpAMP request to the local server.
func postLocal(t *testing.T, s *Supervisor, header http.Header, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, localServerURL(s), bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// upgradeLocal asks the local server for a WebSocket. With 101 it returns the connection, open
// until closed; otherwise a nil one.
func upgradeLocal(t *testing.T, s *Supervisor, header http.Header, upgrade bool) (int, io.ReadWriteCloser) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, localServerURL(s), http.NoBody)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	require.NoError(t, err)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode, nil
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok)
	t.Cleanup(func() { _ = conn.Close() })
	return resp.StatusCode, conn
}

// readServerToAgent reads one OpAMP message from a WebSocket the local server upgraded: an
// unmasked binary frame holding a zero varint header and the message.
func readServerToAgent(conn io.Reader) (*protobufs.ServerToAgent, error) {
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, err
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	if len(payload) == 0 || payload[0] != 0 {
		return nil, fmt.Errorf("not an OpAMP message: % x", payload)
	}
	msg := &protobufs.ServerToAgent{}
	return msg, proto.Unmarshal(payload[1:], msg)
}

// The one-collector gate is held by established connections only. A request that never
// becomes one (a plain HTTP request that fails to decode, a failed upgrade) leaves it free.
func TestLocalServerFailedRequestsLeaveTheConnectionGateFree(t *testing.T) {
	s := newLocalServerTestSupervisor(t)
	header := bearer(testLocalServerToken)

	require.Equal(t, http.StatusBadRequest, postLocal(t, s, header, []byte("not an OpAMP message")))
	status, _ := upgradeLocal(t, s, header, false)
	require.Equal(t, http.StatusBadRequest, status, "a GET without the upgrade headers fails the upgrade")

	status, first := upgradeLocal(t, s, header, true)
	require.Equal(t, http.StatusSwitchingProtocols, status, "the collector can still connect")

	status, _ = upgradeLocal(t, s, header, true)
	require.Equal(t, http.StatusConflict, status, "one collector at a time")
	require.Equal(t, http.StatusConflict, postLocal(t, s, header, nil), "one collector at a time")

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool {
		status, next := upgradeLocal(t, s, header, true)
		if next != nil {
			_ = next.Close()
		}
		return status == http.StatusSwitchingProtocols
	}, 5*time.Second, 20*time.Millisecond, "the gate is free again once the collector disconnects")
}

// fakeCollectorUIDEnv names the instance UID the fake collector reports instead of the one in
// its config.
const fakeCollectorUIDEnv = "OTEL_SUPERVISOR_FAKE_COLLECTOR_UID"

// fakeCollectorNoAuthEnv, set, has the fake collector connect without the headers in its config.
const fakeCollectorNoAuthEnv = "OTEL_SUPERVISOR_FAKE_COLLECTOR_NO_AUTH"

// runFakeCollector is the fake-collector helper of TestMain: it connects to the endpoint of
// extensions::opamp in the config given with --config, with that extension's headers and
// instance UID, and runs until interrupted.
func runFakeCollector() int {
	var path string
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			path = os.Args[i+1]
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	conf, err := config.NewConfFromYAML(body)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	endpoint, _ := conf.Get("extensions::opamp::server::ws::endpoint").(string)
	header := http.Header{}
	if headers, ok := conf.Get("extensions::opamp::server::ws::headers").(map[string]any); ok && os.Getenv(fakeCollectorNoAuthEnv) == "" {
		for k, v := range headers {
			header.Set(k, fmt.Sprint(v))
		}
	}
	uidText := fmt.Sprint(conf.Get("extensions::opamp::instance_uid"))
	if v := os.Getenv(fakeCollectorUIDEnv); v != "" {
		uidText = v
	}
	uid, err := uuid.Parse(uidText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	c := client.NewWebSocket(newLoggerFromZap(zap.NewNop(), "fake-collector"))
	if err := c.SetAgentDescription(&protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{{
			Key:   "service.name",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "fake-collector"}},
		}},
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := c.Start(context.Background(), types.StartSettings{
		OpAMPServerURL: endpoint,
		Header:         header,
		InstanceUid:    types.InstanceUid(uid),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh
	_ = c.Stop(context.Background())
	return 0
}

// remoteHealthServer is an OpAMP server standing in for the Supervisor's, keeping the health
// reported to it.
func remoteHealthServer(t *testing.T) (endpoint string, healths <-chan *protobufs.ComponentHealth) {
	t.Helper()
	ch := make(chan *protobufs.ComponentHealth, 100)
	srv := server.New(newLoggerFromZap(zap.NewNop(), "remote"))
	port, err := (&Supervisor{}).findRandomPort()
	require.NoError(t, err)
	require.NoError(t, srv.Start(server.StartSettings{
		ListenEndpoint: fmt.Sprintf("127.0.0.1:%d", port),
		Settings: server.Settings{Callbacks: serverTypes.Callbacks{
			OnConnecting: func(*http.Request) serverTypes.ConnectionResponse {
				return serverTypes.ConnectionResponse{Accept: true, ConnectionCallbacks: serverTypes.ConnectionCallbacks{
					OnMessage: func(_ context.Context, _ serverTypes.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
						if m.Health != nil {
							select {
							case ch <- m.Health:
							default:
							}
						}
						return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
					},
				}}
			},
		}},
	}))
	t.Cleanup(func() { require.NoError(t, srv.Stop(context.Background())) })
	return fmt.Sprintf("http://127.0.0.1:%d/v1/opamp", port), ch
}

// newBootstrapTestSupervisor is a Supervisor ready for getBootstrapInfo, with the fake collector
// as its collector, reporting to serverEndpoint.
func newBootstrapTestSupervisor(t *testing.T, serverEndpoint string, collectorEnv map[string]string) *Supervisor {
	t.Helper()
	env := map[string]string{supervisorRestartHelperEnv: "fake-collector"}
	for k, v := range collectorEnv {
		env[k] = v
	}
	storage := t.TempDir()
	var cfgYAML strings.Builder
	fmt.Fprintf(&cfgYAML, "server:\n  endpoint: %s\ncapabilities:\n  reports_health: true\n", serverEndpoint)
	fmt.Fprintf(&cfgYAML, "storage:\n  directory: %s\n", storage)
	fmt.Fprintf(&cfgYAML, "agent:\n  executable: %s\n  bootstrap_timeout: 10s\n  env:\n", os.Args[0])
	for k, v := range env {
		fmt.Fprintf(&cfgYAML, "    %s: %q\n", k, v)
	}
	cfgPath := filepath.Join(t.TempDir(), "supervisor.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML.String()), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
	require.NoError(t, err)
	s.runCtx = t.Context()
	s.persistentState, err = loadOrCreatePersistentState(filepath.Join(storage, persistentStateFileName), "", zap.NewNop())
	require.NoError(t, err)
	s.localServerToken = testLocalServerToken
	return s
}

// A collector reporting another instance UID at bootstrap is reported to the server as
// unhealthy. The Supervisor has no agent description then, which the report must not need.
func TestBootstrapReportsAnInstanceUIDMismatch(t *testing.T) {
	endpoint, healths := remoteHealthServer(t)
	s := newBootstrapTestSupervisor(t, endpoint, map[string]string{
		fakeCollectorUIDEnv: "018fef3f-14a8-73ef-b63e-3b96b146ea38",
	})

	var err error
	require.NotPanics(t, func() { err = s.getBootstrapInfo() })
	require.ErrorIs(t, err, errNonMatchingInstanceUID)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case h := <-healths:
			if !h.Healthy && strings.Contains(h.LastError, "does not match") {
				return
			}
		case <-deadline:
			t.Fatal("the server was not told the collector's instance UID does not match")
		}
	}
}

// The server's custom messages go to the collector connected now. The connection of one
// that has gone (a collector restart) was kept until the next one's first message, and what the
// server sent meanwhile went to it, and was lost.
func TestLocalServerForwardsToTheCollectorConnectedNow(t *testing.T) {
	described := make(chan struct{}, 10)
	s := newLocalServerTestSupervisorWith(t, &mockOpAMPClient{
		setAgentDescriptionFunc: func(*protobufs.AgentDescription) error {
			described <- struct{}{}
			return nil
		},
	})
	header := bearer(testLocalServerToken)

	// The collector before the restart connects and reports.
	old := client.NewWebSocket(newLoggerFromZap(zap.NewNop(), "old-collector"))
	require.NoError(t, old.SetAgentDescription(&protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{{
			Key:   "service.name",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "old"}},
		}},
	}))
	require.NoError(t, old.Start(t.Context(), types.StartSettings{
		OpAMPServerURL: fmt.Sprintf("ws://localhost:%d/v1/opamp", s.opampServerPort),
		Header:         header,
		InstanceUid:    types.InstanceUid(s.persistentState.InstanceID),
	}))
	select {
	case <-described:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector's first message never came")
	}
	require.NoError(t, old.Stop(t.Context()))

	// The one after it connects, and has sent nothing yet.
	var next io.ReadWriteCloser
	require.Eventually(t, func() bool {
		var status int
		status, next = upgradeLocal(t, s, header, true)
		return status == http.StatusSwitchingProtocols
	}, 5*time.Second, 20*time.Millisecond)

	customMessage := &protobufs.CustomMessage{Capability: "teapot", Type: "brew", Data: []byte("chamomile")}
	s.onMessage(t.Context(), &types.MessageData{CustomMessage: customMessage})

	got := make(chan *protobufs.ServerToAgent, 1)
	go func() {
		if msg, err := readServerToAgent(next); err == nil {
			got <- msg
		}
	}()
	select {
	case msg := <-got:
		require.True(t, proto.Equal(customMessage, msg.GetCustomMessage()), "got %v", msg)
	case <-time.After(3 * time.Second):
		t.Fatal("the server's custom message did not reach the collector connected now")
	}
}

const testLocalServerToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// The local server takes requests with the token only.
func TestLocalServerRefusesRequestsWithoutTheToken(t *testing.T) {
	s := newLocalServerTestSupervisor(t)

	for name, header := range map[string]http.Header{
		"none":        {},
		"another":     bearer("not-the-token"),
		"a prefix":    bearer(testLocalServerToken[:10]),
		"not bearer":  {"Authorization": {testLocalServerToken}},
		"with suffix": bearer(testLocalServerToken + "0"),
	} {
		require.Equal(t, http.StatusUnauthorized, postLocal(t, s, header, nil), name)
		status, _ := upgradeLocal(t, s, header, true)
		require.Equal(t, http.StatusUnauthorized, status, name)
	}

	status, conn := upgradeLocal(t, s, bearer(testLocalServerToken), true)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	require.NoError(t, conn.Close())
}

// The opamp extension config the Supervisor writes carries the token as a header (configopaque
// in the extension, so redacted in the effective config it reports).
func TestOpAMPExtensionConfigCarriesTheToken(t *testing.T) {
	cfg := setupSupervisorConfig(t, configTemplate)
	s, err := NewSupervisor(t.Context(), zap.NewNop(), cfg)
	require.NoError(t, err)
	s.persistentState = &persistentState{InstanceID: uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")}
	s.localServerToken = testLocalServerToken

	conf, err := config.NewConfFromYAML(s.composeOpAMPExtensionConfig())
	require.NoError(t, err)
	require.Equal(t, map[string]any{"Authorization": "Bearer " + testLocalServerToken},
		conf.Get("extensions::opamp::server::ws::headers"))
}

// The collector's messages are taken only with the Supervisor's instance UID.
func TestHandleAgentOpAMPMessageIgnoresAnotherInstanceUID(t *testing.T) {
	var healths atomic.Int32
	s := newLocalServerTestSupervisorWith(t, &mockOpAMPClient{
		setHealthFunc: func(*protobufs.ComponentHealth) { healths.Add(1) },
	})
	metrics, err := supervisorTelemetry.NewMetrics(sdkmetric.NewMeterProvider())
	require.NoError(t, err)
	s.metrics = metrics
	s.agentReadyChan = make(chan struct{}, 1)
	other := uuid.MustParse("018fef3f-14a8-73ef-b63e-3b96b146ea38")

	s.handleAgentOpAMPMessage(&mockConn{}, &protobufs.AgentToServer{
		InstanceUid: other[:],
		Health:      &protobufs.ComponentHealth{Healthy: true},
	})
	require.Zero(t, healths.Load(), "another instance's health was taken")
	require.False(t, s.agentReady.Load())

	id := s.persistentState.InstanceID
	s.handleAgentOpAMPMessage(&mockConn{}, &protobufs.AgentToServer{
		InstanceUid: id[:],
		Health:      &protobufs.ComponentHealth{Healthy: true},
	})
	require.Equal(t, int32(1), healths.Load())
	require.True(t, s.agentReady.Load())
}

// The bootstrap server takes the collector with the token in its config, and only it.
func TestBootstrapNeedsTheToken(t *testing.T) {
	endpoint, _ := remoteHealthServer(t)

	s := newBootstrapTestSupervisor(t, endpoint, nil)
	require.NoError(t, s.getBootstrapInfo())
	ad, ok := s.agentDescription.Load().(*protobufs.AgentDescription)
	require.True(t, ok)
	require.Equal(t, "fake-collector", ad.GetIdentifyingAttributes()[0].GetValue().GetStringValue())

	s = newBootstrapTestSupervisor(t, endpoint, map[string]string{fakeCollectorNoAuthEnv: "1"})
	s.config.Agent.BootstrapTimeout = 2 * time.Second
	require.ErrorContains(t, s.getBootstrapInfo(), "never connected")
}

// The token goes nowhere the Supervisor sends: not in the effective config it reports (its own
// merged config is never reported), nor in the collector's log tail.
func TestLocalServerTokenIsNotReported(t *testing.T) {
	s := newLocalServerTestSupervisor(t)
	withToken := []byte("extensions:\n  opamp:\n    server:\n      ws:\n        headers:\n          Authorization: Bearer " + testLocalServerToken + "\n")

	s.cfgState.Store(&configState{mergedConfig: string(withToken)})
	require.Nil(t, s.createEffectiveConfigMsg())

	reported := &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: withToken}},
	}}
	s.effectiveConfig.Store(reported)
	body := s.createEffectiveConfigMsg().GetConfigMap().GetConfigMap()[""].GetBody()
	require.NotContains(t, string(body), testLocalServerToken)
	require.Contains(t, string(body), "Authorization: Bearer [REDACTED]")
	require.Contains(t, string(reported.ConfigMap.ConfigMap[""].Body), testLocalServerToken, "what the collector reported is left as it was")

	s.config.Agent.PassthroughLogs = true
	s.config.Agent.CollectorCrashLogSnippetKiB = 4
	s.appendPassthroughLogLine("error: header Bearer " + testLocalServerToken)
	require.NotContains(t, s.collectorCrashLogSnippet(), testLocalServerToken)
}
