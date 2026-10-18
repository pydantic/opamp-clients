// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Changed here: OnConnected is passed on (onConnected).

package supervisor

import (
	"context"
	"net/http"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
)

type flattenedSettings struct {
	onMessage         func(conn serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent
	onConnecting      func(request *http.Request) (shouldConnect bool, rejectStatusCode int)
	onConnected       func(conn serverTypes.Connection)
	onConnectionClose func(conn serverTypes.Connection)
	endpoint          string
}

func (fs flattenedSettings) toServerSettings() server.StartSettings {
	return server.StartSettings{
		Settings: server.Settings{
			Callbacks: serverTypes.Callbacks{
				OnConnecting: fs.OnConnecting,
			},
		},
		ListenEndpoint: fs.endpoint,
	}
}

func (fs flattenedSettings) OnConnecting(request *http.Request) serverTypes.ConnectionResponse {
	if fs.onConnecting != nil {
		shouldConnect, rejectStatusCode := fs.onConnecting(request)
		if !shouldConnect {
			return serverTypes.ConnectionResponse{
				Accept:         false,
				HTTPStatusCode: rejectStatusCode,
			}
		}
	}

	return serverTypes.ConnectionResponse{
		Accept: true,
		ConnectionCallbacks: serverTypes.ConnectionCallbacks{
			OnConnected:       fs.OnConnected,
			OnMessage:         fs.OnMessage,
			OnConnectionClose: fs.OnConnectionClose,
		},
	}
}

// OnConnected is called once a request accepted by OnConnecting is a connection: a WebSocket
// upgraded, or a plain HTTP request whose message decoded.
func (fs flattenedSettings) OnConnected(_ context.Context, conn serverTypes.Connection) {
	if fs.onConnected != nil {
		fs.onConnected(conn)
	}
}

func (fs flattenedSettings) OnMessage(_ context.Context, conn serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
	if fs.onMessage != nil {
		return fs.onMessage(conn, message)
	}

	return &protobufs.ServerToAgent{}
}

func (fs flattenedSettings) OnConnectionClose(conn serverTypes.Connection) {
	if fs.onConnectionClose != nil {
		fs.onConnectionClose(conn)
	}
}
