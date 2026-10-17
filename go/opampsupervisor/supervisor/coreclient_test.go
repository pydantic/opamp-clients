// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap/zaptest"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
)

// These run the Supervisor's OpAMP client against opamp-go's server, over both transports:
// what the server learns is what is judged.

const coreWaitFor = 20 * time.Second

// coreTestServer is an opamp-go server that keeps the Authorization header of each connection
// (in order) and every message.
type coreTestServer struct {
	t       *testing.T
	ws, url string

	mu      sync.Mutex
	auths   []string
	msgs    []*protobufs.AgentToServer
	changed chan struct{}
	// respond, if set, answers a message (the instance UID is filled in).
	respond func(auth string, msg *protobufs.AgentToServer) *protobufs.ServerToAgent
}

func newCoreTestServer(t *testing.T) *coreTestServer {
	ts, _ := newCoreTestServerTLS(t, false)
	return ts
}

// newCoreTestServerTLS is newCoreTestServer, over TLS with a certificate of its own if useTLS
// (the endpoints are then wss and https). httptest's servers all share one certificate, so
// trusting one would trust them all.
func newCoreTestServerTLS(t *testing.T, useTLS bool) (*coreTestServer, *httptest.Server) {
	ts := &coreTestServer{t: t, changed: make(chan struct{}, 1)}
	handler, connContext, err := server.New(nil).Attach(server.Settings{Callbacks: serverTypes.Callbacks{
		OnConnecting: func(r *http.Request) serverTypes.ConnectionResponse {
			auth := r.Header.Get("Authorization")
			ts.record(func() { ts.auths = append(ts.auths, auth) })
			return serverTypes.ConnectionResponse{Accept: true, ConnectionCallbacks: serverTypes.ConnectionCallbacks{
				OnMessage: func(_ context.Context, _ serverTypes.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
					ts.record(func() { ts.msgs = append(ts.msgs, msg) })
					ts.mu.Lock()
					respond := ts.respond
					ts.mu.Unlock()
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
	hs.Config.ConnContext = connContext
	ws, h := "ws://", "http://"
	if useTLS {
		hs.TLS = &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
		hs.StartTLS()
		ws, h = "wss://", "https://"
	} else {
		hs.Start()
	}
	t.Cleanup(hs.Close)
	host := hs.Listener.Addr().String()
	ts.ws, ts.url = ws+host+"/v1/opamp", h+host+"/v1/opamp"
	return ts, hs
}

// selfSignedCert is a new certificate for 127.0.0.1, signed by its own key.
func selfSignedCert(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (ts *coreTestServer) record(f func()) {
	ts.mu.Lock()
	f()
	ts.mu.Unlock()
	select {
	case ts.changed <- struct{}{}:
	default:
	}
}

func (ts *coreTestServer) setRespond(f func(auth string, msg *protobufs.AgentToServer) *protobufs.ServerToAgent) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.respond = f
}

// waitMsg waits for a message that ok accepts (called with ts.mu held), and returns it.
func (ts *coreTestServer) waitMsg(what string, ok func(*protobufs.AgentToServer) bool) *protobufs.AgentToServer {
	ts.t.Helper()
	deadline := time.After(coreWaitFor)
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

func (ts *coreTestServer) forget() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.auths, ts.msgs = nil, nil
}

func (ts *coreTestServer) seenAuths() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.auths...)
}

func (ts *coreTestServer) endpoints() map[string]string {
	return map[string]string{"ws": ts.ws, "http": ts.url}
}

// newClientSupervisor is the part of a Supervisor its OpAMP client needs, with its storage:
// what a restarted Supervisor would have, with no collector.
func newClientSupervisor(t *testing.T, server config.OpAMPServer, caps config.Capabilities, storage config.Storage) *Supervisor {
	t.Helper()
	s := clientSupervisor(t, server, caps, storage)
	require.NoError(t, s.startOpAMPClient())
	return s
}

// clientSupervisor is newClientSupervisor's Supervisor, its client not started.
func clientSupervisor(t *testing.T, server config.OpAMPServer, caps config.Capabilities, storage config.Storage) *Supervisor {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	mp := metric.NewMeterProvider()
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := telemetry.NewMetrics(mp)
	require.NoError(t, err)
	agentDesc := &atomic.Value{}
	agentDesc.Store(&protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{
		Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "otelcol-test"}},
	}}})
	s := &Supervisor{
		runCtx:       ctx,
		runCtxCancel: cancel,
		telemetrySettings: telemetrySettings{
			TelemetrySettings: component.TelemetrySettings{Logger: zaptest.NewLogger(t), TracerProvider: noop.NewTracerProvider()},
		},
		config: config.Supervisor{
			Server:       server,
			Capabilities: caps,
			Storage:      storage,
		},
		heartbeatIntervalSeconds:       30,
		persistentState:                &persistentState{InstanceID: uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")},
		agentDescription:               agentDesc,
		availableComponents:            &atomic.Value{},
		effectiveConfig:                &atomic.Value{},
		cfgState:                       &atomic.Value{},
		agentConfigOwnTelemetrySection: &atomic.Value{},
		agentConn:                      &atomic.Value{},
		hasNewConfig:                   make(chan struct{}, 1),
		metrics:                        metrics,
	}
	t.Cleanup(cancel)
	return s
}

// Credentials the server issues (an OpAMP connection settings offer) replace the configured
// ones once a connection with them has worked, and are kept in the storage directory: a
// restarted Supervisor, with the enrollment credentials still in its config, connects with the
// issued ones. Upstream's client kept them in memory only. With storage.state_key they are
// sealed on the disk.
func TestIssuedCredentialsSurviveSupervisorRestart(t *testing.T) {
	ts := newCoreTestServer(t)
	for _, sealed := range []bool{false, true} {
		for name, endpoint := range ts.endpoints() {
			if sealed {
				name += "/sealed"
			}
			t.Run(name, func(t *testing.T) { issuedSurviveSupervisorRestart(t, ts, name, endpoint, sealed) })
		}
	}
}

func issuedSurviveSupervisorRestart(t *testing.T, ts *coreTestServer, name, endpoint string, sealed bool) {
	ts.forget()
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

	srv := config.OpAMPServer{Endpoint: endpoint, Headers: http.Header{"Authorization": {"Bearer enroll"}}}
	caps := config.Capabilities{AcceptsOpAMPConnectionSettings: true}
	dir := t.TempDir()
	storage := config.Storage{Directory: dir}
	if sealed {
		storage.StateKey = "file:" + filepath.Join(t.TempDir(), "state.key")
	}

	s := newClientSupervisor(t, srv, caps, storage)
	ts.waitMsg("a message with the issued credentials", func(*protobufs.AgentToServer) bool {
		return slices.Contains(ts.auths, "Bearer issued")
	})
	ts.waitMsg("the issued settings reported applied", func(m *protobufs.AgentToServer) bool {
		return m.ConnectionSettingsStatus.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED
	})
	require.NoError(t, s.stopOpAMPClient())
	info, err := os.Stat(filepath.Join(dir, coreClientStateFile))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the state file holds credentials")
	}
	state, err := os.ReadFile(filepath.Join(dir, coreClientStateFile))
	require.NoError(t, err)
	assert.Equal(t, !sealed, bytes.Contains(state, []byte("Bearer issued")), "issued credentials in the clear")

	ts.forget()
	s = newClientSupervisor(t, srv, caps, storage)
	defer func() { assert.NoError(t, s.stopOpAMPClient()) }()
	ts.waitMsg("a message after the restart", func(*protobufs.AgentToServer) bool { return true })
	auths := ts.seenAuths()
	require.NotEmpty(t, auths)
	assert.Equal(t, "Bearer issued", auths[0], "the restarted Supervisor connects with the issued credentials")
	assert.NotContains(t, auths, "Bearer enroll")
}

func remoteConfig(body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{
		ConfigHash: []byte("hash-" + body),
		Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"": {Body: []byte(body), ContentType: "text/yaml"},
		}},
	}
}

// The Supervisor reports APPLYING for a config it restarts the collector with, then APPLIED or
// FAILED once the collector has run with it. A newer config is handed over at once, without
// waiting for that outcome (the Supervisor takes the latest, as upstream's did), and the late
// outcome of the one it replaced isn't reported: the status is about the last config received.
func TestNewerRemoteConfigHandedOverAtOnce(t *testing.T) {
	ts := newCoreTestServer(t)
	for name, endpoint := range ts.endpoints() {
		t.Run(name, func(t *testing.T) {
			ts.forget()
			first, second := remoteConfig("first"), remoteConfig("second")
			var sentFirst, sentSecond sync.Once
			ts.setRespond(func(_ string, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
				var out *protobufs.ServerToAgent
				sentFirst.Do(func() { out = &protobufs.ServerToAgent{RemoteConfig: first} })
				if out == nil && string(m.RemoteConfigStatus.GetLastRemoteConfigHash()) == "hash-first" {
					sentSecond.Do(func() { out = &protobufs.ServerToAgent{RemoteConfig: second} })
				}
				return out
			})
			defer ts.setRespond(nil)

			var mu sync.Mutex
			var events []string
			event := func(e string) {
				mu.Lock()
				events = append(events, e)
				mu.Unlock()
			}
			c := newCoreClient(zaptest.NewLogger(t), filepath.Join(t.TempDir(), coreClientStateFile), nil)
			require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
			interval := 50 * time.Millisecond
			done, gotSecond, appliedFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
			require.NoError(t, c.Start(t.Context(), types.StartSettings{
				OpAMPServerURL:    endpoint,
				InstanceUid:       types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
				HeartbeatInterval: &interval,
				Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
					protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
					protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat,
				Callbacks: types.Callbacks{OnMessage: func(_ context.Context, m *types.MessageData) {
					if m.RemoteConfig == nil {
						return
					}
					hash := m.RemoteConfig.ConfigHash
					event("received " + string(hash))
					status := func(s protobufs.RemoteConfigStatuses) {
						assert.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: hash, Status: s}))
					}
					status(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING)
					go func() {
						if string(hash) == "hash-first" {
							// The collector is still coming up with the first when the second
							// arrives; a client that held the second back gives up waiting.
							select {
							case <-gotSecond:
							case <-time.After(2 * time.Second):
							}
						} else {
							close(gotSecond)
							<-appliedFirst
						}
						event("applied " + string(hash))
						status(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
						if string(hash) == "hash-first" {
							close(appliedFirst)
						} else {
							close(done)
						}
					}()
				}},
			}))
			defer func() { assert.NoError(t, c.Stop(context.Background())) }()

			select {
			case <-done:
			case <-time.After(coreWaitFor):
				t.Fatal("the second config was never applied")
			}
			mu.Lock()
			assert.Equal(t, []string{"received hash-first", "received hash-second", "applied hash-first", "applied hash-second"}, events)
			mu.Unlock()
			ts.waitMsg("the second config reported applied", func(m *protobufs.AgentToServer) bool {
				return string(m.RemoteConfigStatus.GetLastRemoteConfigHash()) == "hash-second" &&
					m.RemoteConfigStatus.GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
			})
			ts.mu.Lock()
			defer ts.mu.Unlock()
			for _, m := range ts.msgs {
				assert.False(t, string(m.RemoteConfigStatus.GetLastRemoteConfigHash()) == "hash-first" &&
					m.RemoteConfigStatus.GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
					"the replaced config's outcome is not reported")
			}
		})
	}
}

// An auth extension's headers are asked for again as the client runs, so a token it refreshes is
// used from a following poll on; they go over the configured header of the same name.
func TestAuthHeadersOnEveryHTTPRequest(t *testing.T) {
	ts := newCoreTestServer(t)
	var n atomic.Int32
	n.Store(1)
	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(t.TempDir(), coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	interval := 50 * time.Millisecond
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL:    ts.url,
		InstanceUid:       types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		HeartbeatInterval: &interval,
		Capabilities:      protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat,
		Header:            http.Header{"Authorization": {"Bearer configured"}},
		HeaderFunc: func(h http.Header) http.Header {
			h = h.Clone()
			h.Set("Authorization", fmt.Sprintf("Bearer token-%d", n.Load()))
			return h
		},
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()
	ts.waitMsg("a few polls", func(*protobufs.AgentToServer) bool { return len(ts.auths) >= 3 })
	n.Store(2)
	require.Eventually(t, func() bool { return slices.Contains(ts.seenAuths(), "Bearer token-2") }, 20*time.Second, 20*time.Millisecond)
	auths := ts.seenAuths()
	assert.Equal(t, "Bearer token-1", auths[0])
	assert.NotContains(t, auths, "Bearer configured")
}

// Start's context reaches what the start does, such as asking the Supervisor for its effective
// config: a Supervisor shutting down cancels it.
func TestStartPassesItsContextOn(t *testing.T) {
	ts := newCoreTestServer(t)
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "start")
	var got any
	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(t.TempDir(), coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(ctx, types.StartSettings{
		OpAMPServerURL: ts.url,
		InstanceUid:    types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		Capabilities:   protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig,
		Callbacks: types.Callbacks{
			GetEffectiveConfig: func(ctx context.Context) (*protobufs.EffectiveConfig, error) {
				got = ctx.Value(key{})
				return &protobufs.EffectiveConfig{}, nil
			},
		},
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()
	assert.Equal(t, "start", got)
}

// Over WebSocket an auth extension's headers are taken at the upgrade: not on the plain HTTP path,
// which a WebSocket client never takes.
func TestAuthHeadersAtTheWebSocketUpgrade(t *testing.T) {
	ts := newCoreTestServer(t)
	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(t.TempDir(), coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL: ts.ws,
		InstanceUid:    types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		Header:         http.Header{"Authorization": {"Bearer configured"}},
		HeaderFunc: func(h http.Header) http.Header {
			h = h.Clone()
			h.Set("Authorization", "Bearer from-the-extension")
			return h
		},
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()
	ts.waitMsg("the upgrade", func(*protobufs.AgentToServer) bool { return len(ts.auths) >= 1 })
	assert.Equal(t, "Bearer from-the-extension", ts.seenAuths()[0])
}

// A headers-only offer (a token rotation) keeps the configured certificate checks. Upstream's
// Supervisor turned them off for one (InsecureSkipVerify), and then sent the rotated token to a
// server whose certificate nothing trusted.
func TestHeadersOnlyOfferKeepsCertificateChecks(t *testing.T) {
	trusted, trustedServer := newCoreTestServerTLS(t, true)
	// Its own self-signed certificate, which the config doesn't trust.
	untrusted, _ := newCoreTestServerTLS(t, true)
	var once sync.Once
	trusted.setRespond(func(string, *protobufs.AgentToServer) *protobufs.ServerToAgent {
		var offer *protobufs.ServerToAgent
		once.Do(func() {
			offer = &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
				Hash: []byte("rotated"),
				Opamp: &protobufs.OpAMPConnectionSettings{
					DestinationEndpoint: untrusted.url,
					Headers: &protobufs.Headers{Headers: []*protobufs.Header{
						{Key: "Authorization", Value: "Bearer rotated"},
					}},
				},
			}}
		})
		return offer
	})
	srv := config.OpAMPServer{Endpoint: trusted.url, Headers: http.Header{"Authorization": {"Bearer enroll"}}}
	srv.TLS.CAPem = configopaque.String(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trustedServer.Certificate().Raw}))

	s := newClientSupervisor(t, srv, config.Capabilities{AcceptsOpAMPConnectionSettings: true}, config.Storage{Directory: t.TempDir()})
	defer func() { assert.NoError(t, s.stopOpAMPClient()) }()
	trusted.waitMsg("the rotation refused", func(m *protobufs.AgentToServer) bool {
		return m.ConnectionSettingsStatus.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED
	})
	assert.Empty(t, untrusted.seenAuths(), "nothing reached the server with the untrusted certificate")
	assert.NotContains(t, trusted.seenAuths(), "Bearer rotated")
}

// A state key the Supervisor can't find stops its client at the start, rather than the client
// keeping the issued credentials in the clear.
func TestSupervisorRefusesAMissingStateKey(t *testing.T) {
	ts := newCoreTestServer(t)
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	srv := config.OpAMPServer{Endpoint: ts.endpoints()["http"]}
	s := clientSupervisor(t, srv, config.Capabilities{}, config.Storage{Directory: dir, StateKey: "systemd-creds"})
	require.ErrorContains(t, s.startOpAMPClient(), "opamp-state-key")
	_, err := os.Stat(filepath.Join(dir, coreClientStateFile))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
