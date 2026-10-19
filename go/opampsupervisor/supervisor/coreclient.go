// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// coreClientStateFile is the client's state file in the storage directory: the instance UID,
// the remote config and own-telemetry settings received, and the OpAMP connection settings the
// server issued. It holds those credentials, so the client writes it readable by the
// Supervisor's user only. Its name is the one every SDK integration gives the same file
// (opamp-state), so operators and docs know one name, and the sealed state's key is named after it.
const coreClientStateFile = "opamp-state"

// coreClient is the shared core's client (go/opamp, in platform mode) behind opamp-go's client
// interface, so the Supervisor runs on it unchanged but for where it makes its client. What the
// core does differently, the Supervisor gets for free:
//
//   - OpAMP connection settings offers are the core's: verified by a connection the server
//     accepts, kept in the state file, and tried again first after a restart. The Supervisor's
//     own OnOpampConnectionSettings is never called.
//   - The remote config is handed over once, and again at start from the state file. The
//     Supervisor's APPLYING ends the hand-over, so a newer config is handed over at once (the
//     Supervisor takes the latest, as upstream); a late answer about one it replaced is dropped.
//   - The effective config is pushed when the Supervisor says it changed, not pulled.
//   - Health, the effective config, the remote config status and the available components reach
//     the server only with their capability, as with opamp-go: the core would declare it for them.
//   - An auth extension's headers are asked for at each HTTP request, and at each WebSocket
//     upgrade.
//   - Packages (with accepts_packages) are downloaded by the core; the collector itself, the
//     top-level package named "", is installed by the Supervisor, which checks its signature.
//
// Calls before Start set the status the client starts with. Only Stop waits (for the client to
// finish); the Supervisor's callbacks run on the client's goroutine, one at a time.
type coreClient struct {
	logger    *zap.Logger
	stateFile string
	sealer    opamp.StateSealer

	mu sync.Mutex
	// opened is the client once opened (Open, or Start), and settings what it was opened with.
	opened   *opamp.Opened
	settings types.StartSettings
	client   *opamp.Client
	stopped  bool
	caps     protobufs.AgentCapabilities
	status   opamp.Status
	// callbacks are the Supervisor's, from Start.
	callbacks types.Callbacks
	// installPackage installs a package the server offers, set before Start; it answers through
	// the request's done, from any goroutine. nil: packages aren't accepted.
	installPackage func(agentInstall)
}

var _ client.OpAMPClient = (*coreClient)(nil)

// newCoreClient is a client to Start once, keeping its state in stateFile (one per storage
// directory: two clients on one state file would overwrite each other's), sealed with sealer
// unless it is nil.
func newCoreClient(logger *zap.Logger, stateFile string, sealer opamp.StateSealer) *coreClient {
	return &coreClient{logger: logger, stateFile: stateFile, sealer: sealer}
}

// errNotRunning: a call that needs the client running, before Start or after Stop.
var errNotRunning = errors.New("the OpAMP client isn't running")

// errUnsupported: what opamp-go's client does that the Supervisor doesn't use.
var errUnsupported = errors.New("not supported by the shared core's client")

// opamp-go's errors (internal to it) for a report without its capability.
var (
	errReportsEffectiveConfigNotSet = errors.New("ReportsEffectiveConfig capability is not set")
	errReportsRemoteConfigNotSet    = errors.New("ReportsRemoteConfig capability is not set")
)

// Open opens the core's client with settings: it reads the state file, so the instance UID and
// what the client resumes are known (InstanceUID, Resumed), and sends nothing. Start starts it,
// with the settings it was opened with. ctx bounds the opening only.
func (c *coreClient) Open(ctx context.Context, settings types.StartSettings) error {
	if _, err := url.Parse(settings.OpAMPServerURL); err != nil {
		return fmt.Errorf("parse server endpoint: %w", err)
	}

	header := settings.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = settings.TLSConfig
	var auth func() http.Header
	if headerFunc := settings.HeaderFunc; headerFunc != nil {
		// An auth extension's headers, asked again as the client runs: the core sets them over
		// the configured ones on every request and upgrade, and checks what it sends.
		auth = func() http.Header { return headerFunc(http.Header{}) }
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opened != nil || c.client != nil || c.stopped {
		return errors.New("the OpAMP client was started already")
	}
	if settings.Capabilities != 0 {
		c.caps = settings.Capabilities
	}
	caps := c.caps
	cfg := opamp.Config{
		Endpoint:     settings.OpAMPServerURL,
		InstanceUID:  [16]byte(settings.InstanceUid),
		Capabilities: uint64(caps),
		Header:       header,
		// Platform mode: net/http and gorilla/websocket, with the Supervisor's TLS settings and the
		// environment's proxies, as opamp-go had.
		Mode:        opamp.ModePlatform,
		HTTPClient:  &http.Client{Transport: transport},
		AuthHeader:  auth,
		StateFile:   c.stateFile,
		StateSealer: c.sealer,
		// The capability is the switch: the core declares it, and the status bit, itself.
		AcceptConnectionSettings: caps&protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings != 0,
		AcceptPackages:           caps&protobufs.AgentCapabilities_AgentCapabilities_AcceptsPackages != 0 && c.installPackage != nil,
	}
	if settings.HeartbeatInterval != nil {
		cfg.HeartbeatInterval = *settings.HeartbeatInterval
	}
	opened, err := opamp.Open(ctx, cfg)
	if err != nil {
		return err
	}
	c.opened, c.settings = opened, settings
	return nil
}

// InstanceUID is the instance UID in use: the one the client opened with, or the server's since.
// Zero before Open.
func (c *coreClient) InstanceUID() [16]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client.InstanceUID()
	}
	if c.opened != nil {
		return c.opened.InstanceUID()
	}
	return [16]byte{}
}

// Resumed is what the client starts from (from Open on); the zero Resumed before.
func (c *coreClient) Resumed() opamp.Resumed {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opened == nil {
		return opamp.Resumed{}
	}
	return c.opened.Resumed()
}

// Start starts the core's client, opening it with settings first unless it is open (then it
// starts with the settings it was opened with). ctx bounds the start only, as with opamp-go:
// the client runs until Stop.
func (c *coreClient) Start(ctx context.Context, settings types.StartSettings) error {
	c.mu.Lock()
	opened := c.opened
	c.mu.Unlock()
	if opened == nil {
		if err := c.Open(ctx, settings); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil || c.stopped {
		return errors.New("the OpAMP client was started already")
	}
	settings = c.settings
	c.callbacks = settings.Callbacks
	initial := c.status
	if settings.RemoteConfigStatus != nil && initial.RemoteConfigStatus == nil {
		initial.RemoteConfigStatus = settings.RemoteConfigStatus
	}
	if initial.RemoteConfigStatus.GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING {
		// The core reports APPLYING for the saved config it hands over at start itself.
		initial.RemoteConfigStatus = nil
	}
	if c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig) && c.callbacks.GetEffectiveConfig != nil {
		if ec, err := c.callbacks.GetEffectiveConfig(ctx); err == nil && ec != nil {
			initial.EffectiveConfig = ec
		}
	}
	// What the server isn't to be told: the core would declare the capability for it.
	if !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth) {
		initial.Health = nil
	}
	if !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig) {
		initial.RemoteConfigStatus = nil
	}
	if !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents) {
		initial.AvailableComponents = nil
	}

	started, err := c.opened.Start(initial, c.onNotification)
	if err != nil {
		return err
	}
	c.client = started
	return nil
}

// Stop stops the client; one opened but not started is let go.
func (c *coreClient) Stop(ctx context.Context) error {
	c.mu.Lock()
	started, opened := c.client, c.opened
	c.client, c.stopped = nil, true
	c.mu.Unlock()
	if started == nil {
		if opened != nil {
			opened.Close()
			return nil
		}
		return errors.New("cannot stop because not started")
	}
	return started.Stop(ctx)
}

// onNotification turns what the core says into the Supervisor's callbacks. It runs on the
// client's goroutine, one at a time, as opamp-go's callbacks run on its receiver's.
func (c *coreClient) onNotification(cl *opamp.Client, n opamp.Notification) {
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
	case opamp.RemoteConfig:
		message(&types.MessageData{RemoteConfig: n.RemoteConfig})
	case opamp.ConnectionSettings:
		offers := n.ConnectionSettings
		message(&types.MessageData{
			OfferedConnectionsSettingsHash: offers.GetHash(),
			OwnMetricsConnSettings:         offers.GetOwnMetrics(),
			OwnTracesConnSettings:          offers.GetOwnTraces(),
			OwnLogsConnSettings:            offers.GetOwnLogs(),
			OtherConnSettings:              offers.GetOtherConnections(),
		})
		// Changed here: the Supervisor answers for the settings it takes, once the collector runs
		// with them or they fail. The others (the opamp part, which the core applies) need an
		// answer all the same: the core needs it to hand over the next.
		if len(offers.GetOtherConnections()) == 0 && offers.GetOwnMetrics() == nil && offers.GetOwnTraces() == nil && offers.GetOwnLogs() == nil {
			cl.SetConnectionSettingsStatus(&protobufs.ConnectionSettingsStatus{
				LastConnectionSettingsHash: offers.GetHash(),
				Status:                     protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED,
			})
		}
	case opamp.CustomMessage:
		message(&types.MessageData{CustomMessage: n.CustomMessage})
	case opamp.ServerCustomCapabilities:
		message(&types.MessageData{CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: n.CustomCapabilities}})
	case opamp.InstanceUIDChanged:
		uid := n.InstanceUID
		message(&types.MessageData{AgentIdentification: &protobufs.AgentIdentification{NewInstanceUid: uid[:]}})
	case opamp.Command:
		if cb.OnCommand != nil {
			if err := cb.OnCommand(ctx, n.Command); err != nil {
				c.logger.Error("Failed to carry out the OpAMP server's command", zap.Error(err))
			}
		}
	case opamp.InstallPackage:
		name, hash := n.PackageName, n.Package.GetHash()
		// Installing stops and starts the collector: not on the client's goroutine, which
		// would hold up everything else the server says meanwhile.
		go c.installPackage(agentInstall{name: name, pkg: n.Package, path: n.Path, done: func(err error) {
			// Changed here: not an outcome the core is told (a shutdown, a rollback that failed).
			// It keeps the package uncertain, and asks again at the next start.
			if unanswered(err) {
				c.logger.Warn("The core isn't told how the collector install went: it asks again at the next start", zap.Error(err))
				return
			}
			cl.PackageInstalled(name, hash, err)
		}})
	case opamp.RemovePackage:
		// The collector the server stops offering is removed, and the configured one runs again;
		// the Supervisor installs no other package.
		name := n.PackageName
		if name != "" || c.installPackage == nil {
			cl.PackageRemoved(name, nil)
			break
		}
		go c.installPackage(agentInstall{name: name, remove: true, done: func(err error) {
			// Changed here: a shutdown before it was done isn't an outcome; the core asks again
			// at the next start.
			if unanswered(err) {
				c.logger.Warn("The core isn't told how removing the collector went: it asks again at the next start", zap.Error(err))
				return
			}
			// Changed here: one still there is reported failed, and runs, until the server
			// offers a collector again and stops offering it. It was left uncertain, and the
			// core asked again at every start.
			if err != nil {
				c.logger.Error("The collector the OpAMP server stopped offering is still installed", zap.Error(err))
			}
			cl.PackageRemoved(name, err)
		}})
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
	case opamp.Stopped:
		if n.Err != nil {
			c.logger.Error("The OpAMP client stopped", zap.Error(n.Err))
		}
	default:
		// Heartbeat intervals are the core's; packages are a capability the Supervisor doesn't
		// declare.
	}
}

// running is the started client, or nil; with c.mu held.
func (c *coreClient) running() *opamp.Client { return c.client }

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
	if cl := c.running(); cl != nil {
		cl.SetDescription(descr)
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
	// opamp-go sends it either way; the core would declare ReportsHealth for it.
	if cl := c.running(); cl != nil && c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth) {
		cl.SetHealth(health)
	}
	return nil
}

func (c *coreClient) UpdateEffectiveConfig(ctx context.Context) error {
	c.mu.Lock()
	get, reports := c.callbacks.GetEffectiveConfig, c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig)
	c.mu.Unlock()
	if !reports {
		return errReportsEffectiveConfigNotSet
	}
	if get == nil {
		return nil
	}
	ec, err := get(ctx)
	if err != nil || ec == nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.EffectiveConfig = ec
	if cl := c.running(); cl != nil {
		cl.SetEffectiveConfig(ec)
	}
	return nil
}

func (c *coreClient) SetRemoteConfigStatus(status *protobufs.RemoteConfigStatus) error {
	if status == nil {
		return errors.New("remote config status is nil")
	}
	status = proto.Clone(status).(*protobufs.RemoteConfigStatus)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Before Start the capabilities may not be set yet; Start leaves it out without them.
	if c.running() != nil && !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig) {
		return errReportsRemoteConfigNotSet
	}
	c.status.RemoteConfigStatus = status
	if cl := c.running(); cl != nil {
		cl.SetRemoteConfigStatus(status)
	}
	return nil
}

func (*coreClient) SetPackageStatuses(*protobufs.PackageStatuses) error { return errUnsupported }

func (*coreClient) RequestConnectionSettings(*protobufs.ConnectionSettingsRequest) error {
	return errUnsupported
}

func (c *coreClient) SetConnectionSettingsStatus(status *protobufs.ConnectionSettingsStatus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.running()
	if cl == nil {
		return errNotRunning
	}
	cl.SetConnectionSettingsStatus(status)
	return nil
}

func (c *coreClient) SetCustomCapabilities(customCapabilities *protobufs.CustomCapabilities) error {
	if customCapabilities == nil {
		return errors.New("custom capabilities are nil")
	}
	customCapabilities = proto.Clone(customCapabilities).(*protobufs.CustomCapabilities)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.CustomCapabilities = customCapabilities
	if cl := c.running(); cl != nil {
		cl.SetCustomCapabilities(customCapabilities)
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
	cl := c.running()
	if cl == nil {
		return nil, errNotRunning
	}
	cl.SendCustomMessage(message)
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
	// Before Start the capabilities may not be set yet; Start leaves it out without them.
	if c.running() != nil && !c.has(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents) {
		return types.ErrReportsAvailableComponentsNotSet
	}
	c.status.AvailableComponents = components
	if cl := c.running(); cl != nil {
		cl.SetAvailableComponents(components)
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
