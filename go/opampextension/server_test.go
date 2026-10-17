// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampextension

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/confmap/confmaptest"
	"go.opentelemetry.io/collector/extension/extensioncapabilities"
	"go.opentelemetry.io/collector/extension/extensiontest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.uber.org/zap/zaptest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/opampcustommessages"
)

// These run the extension against opamp-go's server, over both transports: what the server
// learns is what is judged.

const waitFor = 20 * time.Second

// testServer is an opamp-go server that keeps what reached it: the Authorization header of each
// connection (in order) and every message.
type testServer struct {
	t   *testing.T
	ws  string // the WebSocket endpoint
	url string // the plain HTTP one

	mu      sync.Mutex
	auths   []string
	msgs    []*protobufs.AgentToServer
	changed chan struct{}
	// respond, if set, answers a message (the instance UID is filled in); nil answers nothing.
	respond func(auth string, msg *protobufs.AgentToServer) *protobufs.ServerToAgent
	// drop, if set, closes the connection of the next message (dropNext).
	drop bool
}

func newTestServer(t *testing.T) *testServer {
	return newTestServerOn(t, nil)
}

// newTestServerOn is newTestServer listening on ln (a local TCP port when nil).
func newTestServerOn(t *testing.T, ln net.Listener) *testServer {
	ts := &testServer{t: t, changed: make(chan struct{}, 1)}
	srv := server.New(nil)
	handler, connContext, err := srv.Attach(server.Settings{Callbacks: types.Callbacks{
		OnConnecting: func(r *http.Request) types.ConnectionResponse {
			auth := r.Header.Get("Authorization")
			ts.record(func() { ts.auths = append(ts.auths, auth) })
			return types.ConnectionResponse{Accept: true, ConnectionCallbacks: types.ConnectionCallbacks{
				OnMessage: func(_ context.Context, conn types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
					ts.record(func() { ts.msgs = append(ts.msgs, msg) })
					ts.mu.Lock()
					respond, drop := ts.respond, ts.drop
					ts.drop = false
					ts.mu.Unlock()
					if drop {
						go func() { _ = conn.Disconnect() }()
					}
					var answer *protobufs.ServerToAgent
					if respond != nil {
						answer = respond(auth, msg)
					}
					if answer == nil {
						answer = &protobufs.ServerToAgent{}
					}
					answer.InstanceUid = msg.InstanceUid
					return answer
				},
			}}
		},
	}})
	require.NoError(t, err)
	hs := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	if ln != nil {
		_ = hs.Listener.Close()
		hs.Listener = ln
	}
	hs.Config.ConnContext = connContext
	hs.Start()
	t.Cleanup(hs.Close)
	host := hs.Listener.Addr().String()
	ts.ws, ts.url = "ws://"+host+"/v1/opamp", "http://"+host+"/v1/opamp"
	return ts
}

// dropNext makes the server close the connection that the next message comes on.
func (ts *testServer) dropNext() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.drop = true
}

func (ts *testServer) record(f func()) {
	ts.mu.Lock()
	f()
	ts.mu.Unlock()
	select {
	case ts.changed <- struct{}{}:
	default:
	}
}

func (ts *testServer) setRespond(f func(auth string, msg *protobufs.AgentToServer) *protobufs.ServerToAgent) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.respond = f
}

// waitMsg waits for a message that ok accepts, and returns it.
func (ts *testServer) waitMsg(what string, ok func(*protobufs.AgentToServer) bool) *protobufs.AgentToServer {
	ts.t.Helper()
	deadline := time.After(waitFor)
	for {
		ts.mu.Lock()
		for _, m := range ts.msgs {
			if ok(m) {
				ts.mu.Unlock()
				return m
			}
		}
		ts.mu.Unlock()
		select {
		case <-ts.changed:
		case <-deadline:
			ts.t.Fatalf("the server never got %s", what)
		}
	}
}

// forget drops what the server has seen so far.
func (ts *testServer) forget() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.auths, ts.msgs = nil, nil
}

func (ts *testServer) seenAuths() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.auths...)
}

// transports: the extension's config for each, against ts.
func transports(ts *testServer) map[string]*OpAMPServer {
	return map[string]*OpAMPServer{
		"ws":   {WS: &commonFields{Endpoint: ts.ws}},
		"http": {HTTP: &httpFields{commonFields: commonFields{Endpoint: ts.url}, PollingInterval: 100 * time.Millisecond}},
	}
}

func startAgent(t *testing.T, cfg *Config) *opampAgent {
	t.Helper()
	require.NoError(t, cfg.Validate())
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	set.Logger = zaptest.NewLogger(t)
	set.Resource.Attributes().PutStr(string(semconv.ServiceNameKey), "otelcol-test")
	o, err := newOpampAgent(cfg, set)
	require.NoError(t, err)
	require.NoError(t, o.Start(t.Context(), newAvailableComponentsHost(t)))
	return o
}

func stopAgent(t *testing.T, o *opampAgent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, o.Shutdown(ctx))
}

func attr(kvs []*protobufs.KeyValue, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

// The status a collector reports reaches the server: the description, the components, health
// (unhealthy until the pipelines are ready, then healthy) and the effective config once the
// collector says what it is.
func TestReportsStatusToServer(t *testing.T) {
	ts := newTestServer(t)
	for name, srv := range transports(ts) {
		t.Run(name, func(t *testing.T) {
			ts.forget()
			cfg := createDefaultConfig().(*Config)
			cfg.Server = srv
			o := startAgent(t, cfg)
			defer stopAgent(t, o)

			first := ts.waitMsg("an agent description", func(m *protobufs.AgentToServer) bool {
				return m.AgentDescription != nil
			})
			assert.Equal(t, "otelcol-test", attr(first.AgentDescription.IdentifyingAttributes, string(semconv.ServiceNameKey)))
			assert.NotEmpty(t, first.AvailableComponents.GetHash(), "the core computes the components' hash")
			assert.False(t, first.Health.GetHealthy(), "not healthy before the pipelines are ready")
			assert.NotZero(t, first.Capabilities&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth))

			require.NoError(t, o.Ready())
			ts.waitMsg("healthy", func(m *protobufs.AgentToServer) bool { return m.Health.GetHealthy() })

			effective, err := confmaptest.LoadConf(filepath.Join("testdata", "effective.yaml"))
			require.NoError(t, err)
			require.NoError(t, o.NotifyConfigSnapshot(t.Context(), extensioncapabilities.NewConfigSnapshot(effective, nil)))
			got := ts.waitMsg("the effective config", func(m *protobufs.AgentToServer) bool {
				return m.EffectiveConfig.GetConfigMap().GetConfigMap()[""] != nil
			})
			assert.Contains(t, string(got.EffectiveConfig.ConfigMap.ConfigMap[""].Body), "receivers")
		})
	}
}

// Credentials the server issues (an OpAMP connection settings offer) replace the ones in the
// collector's config once a connection with them has worked, and are kept in the state file: a
// restarted collector, with the enrollment credentials still in its config, connects with the
// issued ones.
func TestIssuedCredentialsSurviveRestart(t *testing.T) {
	ts := newTestServer(t)
	for _, sealed := range []bool{false, true} {
		for name, srv := range transports(ts) {
			if sealed {
				name += "/sealed"
			}
			t.Run(name, func(t *testing.T) {
				issuedSurviveRestart(t, ts, name, srv, sealed)
			})
		}
	}
}

// issuedSurviveRestart enrolls, restarts, and checks the restarted collector uses the issued
// credentials; sealed keeps the state file encrypted with a file: key, and checks it is.
func issuedSurviveRestart(t *testing.T, ts *testServer, name string, srv *OpAMPServer, sealed bool) {
	ts.forget()
	endpoint := srv.GetEndpoint()
	var once sync.Once
	ts.setRespond(func(auth string, _ *protobufs.AgentToServer) *protobufs.ServerToAgent {
		var offer *protobufs.ServerToAgent
		if auth == "Bearer enroll" {
			once.Do(func() {
				offer = &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
					Hash: []byte("issued-" + name),
					Opamp: &protobufs.OpAMPConnectionSettings{
						DestinationEndpoint: endpoint,
						Headers: &protobufs.Headers{Headers: []*protobufs.Header{
							{Key: "Authorization", Value: "Bearer issued"},
						}},
					},
				}}
			})
		}
		return offer
	})
	defer ts.setRespond(nil)

	if srv.WS != nil {
		srv.WS.Headers = map[string]configopaque.String{"Authorization": "Bearer enroll"}
	} else {
		srv.HTTP.Headers = map[string]configopaque.String{"Authorization": "Bearer enroll"}
	}
	cfg := createDefaultConfig().(*Config)
	cfg.Server = srv
	cfg.StateFile = filepath.Join(t.TempDir(), "opamp.state")
	cfg.Capabilities.AcceptsOpAMPConnectionSettings = true
	if sealed {
		cfg.StateKey = "file:" + filepath.Join(t.TempDir(), "state.key")
	}

	o := startAgent(t, cfg)
	ts.waitMsg("the issued settings applied", func(m *protobufs.AgentToServer) bool {
		return m.ConnectionSettingsStatus.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED
	})
	stopAgent(t, o)
	assert.Contains(t, ts.seenAuths(), "Bearer issued")
	state, err := os.ReadFile(cfg.StateFile)
	require.NoError(t, err)
	assert.Equal(t, !sealed, bytes.Contains(state, []byte("Bearer issued")), "issued credentials in the clear")

	ts.forget()
	o = startAgent(t, cfg)
	defer stopAgent(t, o)
	ts.waitMsg("a message after the restart", func(*protobufs.AgentToServer) bool { return true })
	auths := ts.seenAuths()
	require.NotEmpty(t, auths)
	assert.Equal(t, "Bearer issued", auths[0], "the restarted collector connects with the issued credentials")
	assert.NotContains(t, auths, "Bearer enroll")
}

// With a state file the instance UID the client made up is kept across restarts.
func TestInstanceUIDKeptInStateFile(t *testing.T) {
	ts := newTestServer(t)
	cfg := createDefaultConfig().(*Config)
	cfg.Server = transports(ts)["ws"]
	cfg.StateFile = filepath.Join(t.TempDir(), "opamp.state")

	uids := make([][]byte, 2)
	for i := range uids {
		ts.forget()
		o := startAgent(t, cfg)
		uids[i] = ts.waitMsg("a message", func(*protobufs.AgentToServer) bool { return true }).InstanceUid
		stopAgent(t, o)
	}
	require.Len(t, uids[0], 16)
	assert.Equal(t, uids[0], uids[1])
	assert.Equal(t, uuid.Version(7), uuid.UUID(uids[0]).Version(), "the client makes up a UUIDv7")
}

// A configured instance UID is the one the server sees.
func TestConfiguredInstanceUID(t *testing.T) {
	ts := newTestServer(t)
	cfg := createDefaultConfig().(*Config)
	cfg.Server = transports(ts)["ws"]
	cfg.InstanceUID = "01BX5ZZKBKACTAV9WEVGEMMVRZ"
	want, err := parseInstanceIDString(cfg.InstanceUID)
	require.NoError(t, err)

	o := startAgent(t, cfg)
	defer stopAgent(t, o)
	got := ts.waitMsg("a message", func(*protobufs.AgentToServer) bool { return true })
	assert.Equal(t, want[:], got.InstanceUid)
}

// Custom messages go both ways through the registry other components use, for a capability
// registered before the client started (as components do) and so declared in its first message.
func TestCustomMessages(t *testing.T) {
	ts := newTestServer(t)
	for name, srv := range transports(ts) {
		t.Run(name, func(t *testing.T) {
			ts.forget()
			const capability = "io.opentelemetry.test.echo"
			var once sync.Once
			ts.setRespond(func(_ string, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
				var out *protobufs.ServerToAgent
				if slices.Contains(m.CustomCapabilities.GetCapabilities(), capability) {
					once.Do(func() {
						out = &protobufs.ServerToAgent{CustomMessage: &protobufs.CustomMessage{
							Capability: capability, Type: "ping", Data: []byte("from the server"),
						}}
					})
				}
				return out
			})
			defer ts.setRespond(nil)

			cfg := createDefaultConfig().(*Config)
			cfg.Server = srv
			set := extensiontest.NewNopSettings(extensiontest.NopType)
			set.Logger = zaptest.NewLogger(t)
			o, err := newOpampAgent(cfg, set)
			require.NoError(t, err)
			handler, err := o.Register(capability, opampcustommessages.WithMaxQueuedMessages(4))
			require.NoError(t, err)
			require.NoError(t, o.Start(t.Context(), componenttest.NewNopHost()))
			defer stopAgent(t, o)

			select {
			case m := <-handler.Message():
				assert.Equal(t, []byte("from the server"), m.Data)
			case <-time.After(waitFor):
				t.Fatal("the custom message from the server never arrived")
			}

			sent, err := handler.SendMessage("pong", []byte("from the collector"))
			require.NoError(t, err)
			<-sent
			ts.waitMsg("the collector's custom message", func(m *protobufs.AgentToServer) bool {
				return m.CustomMessage.GetCapability() == capability && bytes.Equal(m.CustomMessage.GetData(), []byte("from the collector"))
			})
		})
	}
}

// A custom message sent before the client starts is refused, not lost silently.
func TestCustomMessageBeforeStart(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	o, err := newOpampAgent(cfg, set)
	require.NoError(t, err)
	handler, err := o.Register("io.opentelemetry.test.early")
	require.NoError(t, err)
	_, err = handler.SendMessage("t", nil)
	require.ErrorIs(t, err, errNotStarted)
	require.NoError(t, o.Shutdown(t.Context()))
}

// A state key the extension can't find stops it at the start, rather than the client keeping the
// issued credentials in the clear.
func TestStartRefusesAMissingStateKey(t *testing.T) {
	ts := newTestServer(t)
	cfg := createDefaultConfig().(*Config)
	cfg.Server = transports(ts)["http"]
	cfg.StateFile = filepath.Join(t.TempDir(), "opamp.state")
	cfg.StateKey = "systemd-creds"
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	set := extensiontest.NewNopSettings(extensiontest.NopType)
	o, err := newOpampAgent(cfg, set)
	require.NoError(t, err)
	err = o.Start(t.Context(), newAvailableComponentsHost(t))
	require.ErrorContains(t, err, "opamp-state-key")
	_, statErr := os.Stat(cfg.StateFile)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// With a socket set, the extension dials it in place of the endpoint's host and port, over both
// transports. The endpoint names localhost on the default port, where nothing listens, so a
// message reaches the server only through the socket.
func TestConnectsOverUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix domain sockets on Windows")
	}
	// macOS limits a socket path to about 104 bytes, which t.TempDir() can exceed.
	dir, err := os.MkdirTemp("/tmp", "opampuds") //nolint:usetesting // see above
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "opamp.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	ts := newTestServerOn(t, ln)
	socket := &confignet.AddrConfig{Transport: confignet.TransportTypeUnix, Endpoint: path}
	for name, srv := range map[string]*OpAMPServer{
		"ws":   {WS: &commonFields{Endpoint: "ws://localhost/v1/opamp", Socket: socket}},
		"http": {HTTP: &httpFields{commonFields: commonFields{Endpoint: "http://localhost/v1/opamp", Socket: socket}, PollingInterval: 100 * time.Millisecond}},
	} {
		t.Run(name, func(t *testing.T) {
			ts.forget()
			cfg := createDefaultConfig().(*Config)
			cfg.Server = srv
			o := startAgent(t, cfg)
			defer stopAgent(t, o)
			msg := ts.waitMsg("a message over the socket", func(m *protobufs.AgentToServer) bool {
				return m.AgentDescription != nil
			})
			assert.Equal(t, "otelcol-test", attr(msg.AgentDescription.IdentifyingAttributes, string(semconv.ServiceNameKey)))
		})
	}
}
