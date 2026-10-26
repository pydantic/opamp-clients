//go:build install || container

package packaging

import (
	"context"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"
)

// issuingServer accepts the enrollment token and the token it issues, issues one on an agent's
// first connection with the enrollment token, and records each connection's Authorization.
type issuingServer struct {
	endpoint string
	mu       sync.Mutex
	auths    []string
	statuses []protobufs.ConnectionSettingsStatuses
	offered  bool
}

func newIssuingServer(t *testing.T) *issuingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	s := &issuingServer{endpoint: "ws://" + addr + "/v1/opamp"}
	srv := server.New(nil)
	require.NoError(t, srv.Start(server.StartSettings{
		ListenEndpoint: addr,
		Settings:       server.Settings{Callbacks: types.Callbacks{OnConnecting: s.connecting}},
	}))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return s
}

func (s *issuingServer) connecting(r *http.Request) types.ConnectionResponse {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.auths = append(s.auths, auth)
	s.mu.Unlock()
	if auth != "Bearer enroll-token" && auth != "Bearer issued-token" {
		return types.ConnectionResponse{HTTPStatusCode: http.StatusUnauthorized}
	}
	return types.ConnectionResponse{Accept: true, ConnectionCallbacks: types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			s.mu.Lock()
			defer s.mu.Unlock()
			if st := m.GetConnectionSettingsStatus(); st != nil {
				s.statuses = append(s.statuses, st.Status)
			}
			reply := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
			if auth == "Bearer enroll-token" && !s.offered {
				s.offered = true
				reply.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
					Hash: []byte("issued"),
					Opamp: &protobufs.OpAMPConnectionSettings{
						DestinationEndpoint: s.endpoint,
						Headers:             &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer issued-token"}}},
					},
				}
			}
			return reply
		},
	}}
}

func (s *issuingServer) seen() ([]string, []protobufs.ConnectionSettingsStatuses) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths), slices.Clone(s.statuses)
}

func (s *issuingServer) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths, s.statuses = nil, nil
}
