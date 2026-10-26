// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package opampextension is the OpenTelemetry Collector's OpAMP extension on the shared core
// (go/opamp, in platform mode) instead of opamp-go: upstream's extension
// (opentelemetry-collector-contrib's extension/opampextension), with its config, its component type
// and its custom-message API, so that a collector built with it in place of upstream's runs the
// same config. What changes is the client underneath, and what that brings: with state_file the
// instance UID is kept across restarts, and with accepts_opamp_connection_settings the server can
// issue new credentials, which are verified before they replace the old ones and kept in the state
// file. See README.md.
package opampextension

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/statekey"
)

// coreClient is the shared core's client (go/opamp, in platform mode) behind opamp-go's client
// interface, so the extension runs on it unchanged but for where it makes its client
// (newOpampAgent) and a line telling it the host (useHost). What the core does differently, the
// extension gets for free:
//
//   - Without a configured instance_uid, the core makes the UID and, with state_file, keeps it
//     across restarts; the extension learns it as upstream learns a UID the server assigns.
//   - OpAMP connection settings offers (accepts_opamp_connection_settings) are verified by a
//     connection the server accepts, kept in state_file, and tried again first after a restart.
//   - The status reported before Start (health, custom capabilities) goes into the first message.
//   - The effective config is pushed when the extension says it changed, not pulled.
//   - The available components' hash is the core's.
//   - An auth extension runs on every request over HTTP, and is asked for its headers about every
//     second over WebSocket, so each upgrade carries what it gives then (core_auth.go).
//
// Its callbacks run on the client's goroutine, one at a time, as opamp-go's do.
type coreClient struct {
	cfg    *Config
	logger *zap.Logger

	mu        sync.Mutex
	host      component.Host
	client    *opamp.Client
	caps      protobufs.AgentCapabilities
	status    opamp.Status
	callbacks types.Callbacks
	// uid is the instance UID the extension has: the one it started with, or the last
	// InstanceUIDChanged passed on (on the client's goroutine after Start).
	uid [16]byte
}

var _ client.OpAMPClient = (*coreClient)(nil)

// errNotRunning: a call that needs the client running, before Start or after Stop.
var errNotRunning = errors.New("the OpAMP client isn't running")

// errUnsupported: what the extension never asks of the client.
var errUnsupported = errors.New("not supported by the OpAMP extension's client")

func newCoreClient(cfg *Config, logger *zap.Logger) *coreClient {
	return &coreClient{cfg: cfg, logger: logger.Named("opamp-client")}
}

// useHost gives the client the collector's host, where the auth extension is found.
func useHost(c client.OpAMPClient, host component.Host) {
	if cc, ok := c.(*coreClient); ok {
		cc.mu.Lock()
		cc.host = host
		cc.mu.Unlock()
	}
}

// validateState checks the state file's settings.
func (cfg *Config) validateState() error {
	if cfg.Capabilities.AcceptsOpAMPConnectionSettings {
		if cfg.StateFile == "" {
			// Settings the server issued and the client forgot at a restart would lock the
			// agent out once the server has dropped the old ones.
			return errors.New("accepts_opamp_connection_settings requires state_file")
		}
		if cfg.Server.GetAuthExtensionID() != (component.ID{}) {
			// The auth extension would replace the headers the server issued.
			return errors.New("accepts_opamp_connection_settings can't be used with an auth extension")
		}
	}
	if cfg.StateKey != "" && cfg.StateFile == "" {
		return errors.New("state_key requires state_file")
	}
	return nil
}

func (c *coreClient) Start(ctx context.Context, settings types.StartSettings) error {
	if settings.OpAMPServerURL == "" {
		// No server: a config Validate refuses, which only a test starts with. opamp-go's client
		// starts, and fails to connect; this one doesn't start.
		c.logger.Error("No OpAMP server endpoint: the OpAMP client isn't started")
		return nil
	}
	c.mu.Lock()
	host := c.host
	c.mu.Unlock()
	server := c.cfg.Server
	httpClient, err := newHTTPClient(server, settings.TLSConfig, host)
	if err != nil {
		return err
	}
	authHeader, err := newAuthHeader(server, host, c.logger)
	if err != nil {
		return err
	}
	var sealer opamp.StateSealer
	if c.cfg.StateKey != "" {
		// Found (or made) before the client starts, so a missing key stops the extension rather
		// than the client writing the issued credentials in the clear.
		if sealer, err = statekey.Load(c.cfg.StateKey, c.cfg.StateFile); err != nil {
			return err
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return errors.New("the OpAMP client was started already")
	}
	cfg := opamp.Config{
		Endpoint: settings.OpAMPServerURL,
		// Heartbeats, as the extension has always sent on this client: a client declares the
		// bit to send them (ReportsHeartbeat), and upstream's capabilities don't name it.
		Capabilities: uint64(c.caps | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat),
		Header:       settings.Header,
		// Platform mode: net/http carries the requests, so the collector's TLS settings, the
		// environment's proxies and auth extensions apply as they do to its other clients.
		Mode:                     opamp.ModePlatform,
		HTTPClient:               httpClient,
		AuthHeader:               authHeader,
		StateFile:                c.cfg.StateFile,
		StateSealer:              sealer,
		AcceptConnectionSettings: c.cfg.Capabilities.AcceptsOpAMPConnectionSettings,
	}
	if c.cfg.InstanceUID != "" {
		// Configured; otherwise the core makes one, or uses the one it kept.
		cfg.InstanceUID = [16]byte(settings.InstanceUid)
	}
	if server.HTTP != nil {
		// Over plain HTTP the heartbeat is the poll.
		cfg.HeartbeatInterval = server.GetPollingInterval()
	}
	c.callbacks = settings.Callbacks
	initial := c.status
	if c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig) && c.callbacks.GetEffectiveConfig != nil {
		if ec, err := c.callbacks.GetEffectiveConfig(ctx); err == nil && ec != nil {
			initial.EffectiveConfig = ec
		}
	}
	// What the server isn't to be told: the core would declare the capability for it.
	if !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth) {
		initial.Health = nil
	}
	if !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents) {
		initial.AvailableComponents = nil
	}
	c.uid = [16]byte(settings.InstanceUid)
	started, err := opamp.Start(ctx, cfg, initial, c.onNotification)
	if err != nil {
		return err
	}
	c.client = started
	return nil
}

func (c *coreClient) Stop(ctx context.Context) error {
	c.mu.Lock()
	started := c.client
	c.client = nil
	c.mu.Unlock()
	if started == nil {
		// opamp-go's error, which the extension takes for "nothing to stop".
		return errors.New("cannot stop because not started")
	}
	return started.Stop(ctx)
}

// onNotification turns what the core says into the extension's callbacks.
func (c *coreClient) onNotification(_ *opamp.Client, n opamp.Notification) {
	ctx := context.Background()
	cb := c.callbacks
	message := func(m *types.MessageData) {
		if cb.OnMessage != nil {
			cb.OnMessage(ctx, m)
		}
	}
	switch n.Kind {
	case opamp.Connected:
		if cb.OnConnect != nil {
			cb.OnConnect(ctx)
		}
	case opamp.ConnectFailed:
		if cb.OnConnectFailed != nil {
			cb.OnConnectFailed(ctx, fmt.Errorf("connecting to the OpAMP server failed (HTTP status %d): %s", n.Status, n.Detail))
		}
	case opamp.ErrorResponse:
		if cb.OnError != nil {
			cb.OnError(ctx, &protobufs.ServerErrorResponse{Type: n.ErrorType, ErrorMessage: n.Message})
		}
	case opamp.CustomMessage:
		message(&types.MessageData{CustomMessage: n.CustomMessage})
	case opamp.InstanceUIDChanged:
		uid := n.InstanceUID
		if uid == c.uid {
			return
		}
		// One the core made or kept (then the extension's was a placeholder), or one the server
		// assigned.
		c.uid = uid
		message(&types.MessageData{AgentIdentification: &protobufs.AgentIdentification{NewInstanceUid: uid[:]}})
	case opamp.Command:
		if cb.OnCommand != nil {
			if err := cb.OnCommand(ctx, n.Command); err != nil {
				c.logger.Error("Failed to carry out the OpAMP server's command", zap.Error(err))
			}
		}
	case opamp.CustomMessageNotSent:
		c.logger.Warn("A custom message to the OpAMP server was not sent",
			zap.String("capability", n.CustomMessage.GetCapability()), zap.String("reason", n.Detail))
	case opamp.StatusTooLarge:
		c.logger.Warn("A status field was too large to send to the OpAMP server",
			zap.String("field", n.FieldName), zap.Int("bytes", n.Bytes))
	case opamp.BadMessage:
		c.logger.Warn("The OpAMP server sent a message that could not be read", zap.String("detail", n.Detail))
	case opamp.SaveStalled:
		if n.Stalled {
			c.logger.Warn("Saving the OpAMP state has stalled: nothing else is saved until it ends",
				zap.Duration("waited", n.Waited))
		} else {
			c.logger.Info("The stalled save of the OpAMP state has ended", zap.Duration("waited", n.Waited))
		}
	case opamp.StateFromNewerVersion:
		c.logger.Warn("The saved OpAMP state was written by a newer version, which this one can't read: "+
			"it is kept as it is, and this run saves nothing", zap.Uint32("state_format", n.StateVersion))
	case opamp.Stopped:
		if n.Err != nil {
			c.logger.Error("The OpAMP client stopped", zap.Error(n.Err))
		}
	default:
		// Remote config, own-telemetry settings and packages: capabilities the extension doesn't
		// declare, so the server has none to send.
	}
}

// has says whether the capability is declared; with c.mu held.
func (c *coreClient) has(capability protobufs.AgentCapabilities) bool { return c.caps&capability != 0 }

func (c *coreClient) SetAgentDescription(descr *protobufs.AgentDescription) error {
	if descr == nil {
		return errors.New("agent description is nil")
	}
	descr = proto.Clone(descr).(*protobufs.AgentDescription)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Description = descr
	if c.client != nil {
		c.client.SetDescription(descr)
	}
	return nil
}

func (c *coreClient) AgentDescription() *protobufs.AgentDescription {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.Description
}

func (c *coreClient) SetHealth(health *protobufs.ComponentHealth) error {
	if health == nil {
		return errors.New("health is nil")
	}
	health = proto.Clone(health).(*protobufs.ComponentHealth)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Health = health
	if c.client != nil && c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth) {
		c.client.SetHealth(health)
	}
	return nil
}

func (c *coreClient) UpdateEffectiveConfig(ctx context.Context) error {
	c.mu.Lock()
	get, running := c.callbacks.GetEffectiveConfig, c.client != nil
	c.mu.Unlock()
	if !running || get == nil {
		// Start asks for it.
		return nil
	}
	ec, err := get(ctx)
	if err != nil || ec == nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.EffectiveConfig = ec
	if c.client != nil && c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig) {
		c.client.SetEffectiveConfig(ec)
	}
	return nil
}

func (*coreClient) SetRemoteConfigStatus(*protobufs.RemoteConfigStatus) error { return errUnsupported }

func (*coreClient) SetPackageStatuses(*protobufs.PackageStatuses) error { return errUnsupported }

func (*coreClient) RequestConnectionSettings(*protobufs.ConnectionSettingsRequest) error {
	return errUnsupported
}

func (*coreClient) SetConnectionSettingsStatus(*protobufs.ConnectionSettingsStatus) error {
	return errUnsupported
}

func (c *coreClient) SetCustomCapabilities(customCapabilities *protobufs.CustomCapabilities) error {
	if customCapabilities == nil {
		return errors.New("custom capabilities are nil")
	}
	customCapabilities = proto.Clone(customCapabilities).(*protobufs.CustomCapabilities)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.CustomCapabilities = customCapabilities
	if c.client != nil {
		c.client.SetCustomCapabilities(customCapabilities)
	}
	return nil
}

func (*coreClient) SetFlags(protobufs.AgentToServerFlags) {}

// SendCustomMessage hands the message to the client, which queues it: the channel returned is
// closed already (never ErrCustomMessagePending), and a message it can't send after all is
// logged (CustomMessageNotSent).
func (c *coreClient) SendCustomMessage(message *protobufs.CustomMessage) (chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return nil, errNotRunning
	}
	c.client.SendCustomMessage(message)
	sent := make(chan struct{})
	close(sent)
	return sent, nil
}

func (c *coreClient) SetAvailableComponents(components *protobufs.AvailableComponents) error {
	if components == nil {
		return errors.New("available components are nil")
	}
	components = proto.Clone(components).(*protobufs.AvailableComponents)
	// The core computes the hash from the components.
	components.Hash = nil
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.AvailableComponents = components
	if c.client != nil && c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents) {
		c.client.SetAvailableComponents(components)
	}
	return nil
}

func (c *coreClient) SetCapabilities(capabilities *protobufs.AgentCapabilities) error {
	if capabilities == nil {
		return errors.New("capabilities are nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return errors.New("capabilities can't change once the OpAMP client has started")
	}
	c.caps = *capabilities
	return nil
}
