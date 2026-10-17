// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"math"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// NotificationKind says what happened.
type NotificationKind int

const (
	// Connected: a WebSocket connected, or an HTTP request succeeded, the first or the first after
	// failures.
	Connected NotificationKind = iota
	// ConnectFailed: connecting or a request failed (Status, Detail).
	ConnectFailed
	// ErrorResponse: the server sent an error_response (ErrorType, Message).
	ErrorResponse
	// RemoteConfig: a config to apply (RemoteConfig); answer with SetRemoteConfigStatus.
	RemoteConfig
	// Command: a command from the server (Command).
	Command
	// InstanceUIDChanged: the server assigned a new UID (InstanceUID); persist it if you persist the UID.
	InstanceUIDChanged
	// HeartbeatInterval: the heartbeat or polling interval changed (HeartbeatInterval; 0: off).
	HeartbeatInterval
	// StatusTooLarge: a status field was left out for size (Field, FieldName, Bytes); not tried
	// again until it changes or the server asks for the full state.
	StatusTooLarge
	// BadMessage: the server sent something that isn't a ServerToAgent (Detail).
	BadMessage
	// Stopped: the client has stopped: after Stop, or on its own because of an error (Err).
	Stopped
	// ConnectionSettings: settings for the agent's own telemetry and other connections, all of
	// them (ConnectionSettings); answer with SetConnectionSettingsStatus.
	ConnectionSettings
	// CustomMessage: a custom message from the server, for a custom capability the agent
	// declared (CustomMessage).
	CustomMessage
	// ServerCustomCapabilities: the custom capabilities the server supports changed
	// (CustomCapabilities).
	ServerCustomCapabilities
	// CustomMessageNotSent: a custom message the application sent won't be (CustomMessage,
	// Detail: why).
	CustomMessageNotSent
	// InstallPackage: install package PackageName (Package) from the file at Path; answer with
	// PackageInstalled. Checking the file's signature is the application's job; the client
	// deletes the file after the answer.
	InstallPackage
	// RemovePackage: remove package PackageName, no longer offered; answer with PackageRemoved.
	RemovePackage
	// SaveStalled: the save under way has run for its stall time without ending (Stalled), and
	// the client waits on it still, saving nothing else meanwhile; or it has ended at last (not
	// Stalled). Waited: how long it had run. A disk that hangs is the application's to see to.
	SaveStalled
)

// StatusField names a status field.
type StatusField int

const (
	FieldDescription StatusField = iota
	FieldHealth
	FieldEffectiveConfig
	FieldRemoteConfigStatus
	FieldConnectionSettingsStatus
	FieldAvailableComponents
	FieldCustomCapabilities
	FieldPackageStatuses
)

// Notification is something the application is told. Kind says which fields are set.
type Notification struct {
	Kind         NotificationKind
	Status       int // HTTP status, 0 if none
	Detail       string
	ErrorType    protobufs.ServerErrorResponseType
	Message      string
	RemoteConfig *protobufs.AgentRemoteConfig
	// ConnectionSettings: the telemetry settings (no OpAMP part: the client applies that itself).
	ConnectionSettings *protobufs.ConnectionSettingsOffers
	// CustomMessage: the message (CustomMessage, CustomMessageNotSent).
	CustomMessage *protobufs.CustomMessage
	// CustomCapabilities: the server's (ServerCustomCapabilities).
	CustomCapabilities []string
	Command            *protobufs.ServerToAgentCommand
	// PackageName: the package (InstallPackage, RemovePackage).
	PackageName string
	// Package: the package as offered: version, hash, type, and the file's signature (InstallPackage).
	Package *protobufs.PackageAvailable
	// Path: the downloaded file (InstallPackage).
	Path              string
	InstanceUID       [16]byte
	HeartbeatInterval time.Duration
	Bytes             int
	Field             StatusField
	// FieldName: the field's name in AgentToServer ("health", say), as the core gives it
	// (StatusTooLarge).
	FieldName string
	// Stalled: the save has stalled, or (false) it has ended at last (SaveStalled).
	Stalled bool
	// Waited: how long the save had run (SaveStalled).
	Waited time.Duration
	// Err: why the client stopped on its own (Stopped); nil after Stop. Stop returns it too.
	Err error
}

func fromCore(n *corepb.Notification) Notification {
	switch k := n.Kind.(type) {
	case *corepb.Notification_Connected:
		return Notification{Kind: Connected}
	case *corepb.Notification_ConnectFailed:
		return Notification{Kind: ConnectFailed, Status: int(k.ConnectFailed.GetStatus()), Detail: k.ConnectFailed.Detail}
	case *corepb.Notification_ErrorResponse:
		return Notification{Kind: ErrorResponse, ErrorType: protobufs.ServerErrorResponseType(k.ErrorResponse.Kind),
			Message: k.ErrorResponse.Message}
	case *corepb.Notification_RemoteConfig:
		return Notification{Kind: RemoteConfig, RemoteConfig: k.RemoteConfig}
	case *corepb.Notification_CustomMessage:
		return Notification{Kind: CustomMessage, CustomMessage: k.CustomMessage}
	case *corepb.Notification_ServerCustomCapabilities:
		return Notification{Kind: ServerCustomCapabilities, CustomCapabilities: k.ServerCustomCapabilities.Capabilities}
	case *corepb.Notification_CustomMessageNotSent:
		return Notification{Kind: CustomMessageNotSent, CustomMessage: k.CustomMessageNotSent.Message,
			Detail: k.CustomMessageNotSent.Reason}
	case *corepb.Notification_InstallPackage:
		return Notification{Kind: InstallPackage, PackageName: k.InstallPackage.Name, Package: k.InstallPackage.Package}
	case *corepb.Notification_RemovePackage:
		return Notification{Kind: RemovePackage, PackageName: k.RemovePackage.Name}
	case *corepb.Notification_ConnectionSettings:
		return Notification{Kind: ConnectionSettings, ConnectionSettings: k.ConnectionSettings}
	case *corepb.Notification_Command:
		return Notification{Kind: Command, Command: k.Command}
	case *corepb.Notification_InstanceUidChanged:
		var uid [16]byte
		copy(uid[:], k.InstanceUidChanged)
		return Notification{Kind: InstanceUIDChanged, InstanceUID: uid}
	case *corepb.Notification_HeartbeatIntervalMs:
		// As far as a time.Duration goes (the core holds it to a day).
		return Notification{Kind: HeartbeatInterval, HeartbeatInterval: time.Duration(min(k.HeartbeatIntervalMs, maxMillis)) * time.Millisecond}
	case *corepb.Notification_StatusTooLarge:
		return Notification{Kind: StatusTooLarge, Field: StatusField(k.StatusTooLarge.Field),
			FieldName: k.StatusTooLarge.Name, Bytes: int(min(k.StatusTooLarge.Bytes, math.MaxInt))}
	case *corepb.Notification_BadMessage:
		return Notification{Kind: BadMessage, Detail: k.BadMessage.Detail}
	case *corepb.Notification_SaveStalled:
		return Notification{Kind: SaveStalled, Stalled: k.SaveStalled.Stalled,
			Waited: time.Duration(min(k.SaveStalled.WaitedMs, maxMillis)) * time.Millisecond}
	case *corepb.Notification_Stopped:
		return Notification{Kind: Stopped}
	default:
		return Notification{Kind: unknownNotification}
	}
}

// unknownNotification is a notification of a kind this client doesn't know (a newer core's): the
// application isn't told it, as in the Node.js client. Never Stopped: the client goes on, and
// nothing reaches the application after Stopped (RULE-SHELL-01).
const unknownNotification NotificationKind = -1
