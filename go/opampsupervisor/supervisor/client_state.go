// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/extensions"
)

// The Supervisor keeps no state of its own: the OpAMP client's state file holds the instance UID,
// the remote configs (the last received, the last applied, a refused one's status) and the
// own-telemetry settings, and says what it resumes before it connects. Upstream's Supervisor kept
// persistent_state.yaml and three .dat files beside it; the code that read and wrote them is
// upstream's still, and reads the client's state through these hooks:
//
//   - persistentState is the client's instance UID and refused status, in memory
//     (clientPersistentState);
//   - readStorageFile serves the .dat files from what the client resumes, and writeStorageFile
//     writes none of them (clientKeeps);
//   - loadRemoteConfig is loadResumedRemoteConfig: a config the collector refused isn't run
//     again at the next start, with or without agent::automatic_config_rollback.

// clientPersistentState opens the OpAMP client to read its state: nothing is sent. The instance
// UID is agent::instance_id if set, else the one the client kept (or made). startOpAMPClient
// starts this client (takeOpenedClient): one opened again with the UID it made would take it
// for a configured one, and not keep it.
func (s *Supervisor) clientPersistentState() (*persistentState, error) {
	var tlsConfig *tls.Config
	parsedURL, err := url.Parse(s.config.Server.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse server endpoint: %w", err)
	}
	if parsedURL.Scheme == "wss" || parsedURL.Scheme == "https" {
		if tlsConfig, err = s.config.Server.TLS.LoadTLSConfig(s.runCtx); err != nil {
			return nil, err
		}
	}
	if err := s.loadStorageSealer(); err != nil {
		return nil, err
	}
	c := newCoreClient(s.telemetrySettings.Logger.Named("opamp-client"), filepath.Join(s.config.Storage.Directory, coreClientStateFile), s.storageSealer)
	if s.config.Capabilities.AcceptsPackages {
		c.installPackage = s.onInstallPackage
	}
	capabilities := s.config.Capabilities
	if len(s.config.Agent.ConnectionEnv) == 0 {
		capabilities.AcceptsOtherConnectionSettings = false
	}
	supported := capabilities.SupportedCapabilities()
	if err := c.SetCapabilities(&supported); err != nil {
		return nil, err
	}
	settings := types.StartSettings{
		OpAMPServerURL: s.config.Server.Endpoint,
		Header:         s.config.Server.Headers,
		TLSConfig:      tlsConfig,
		InstanceUid:    types.InstanceUid(s.configuredInstanceID()),
	}
	// What startOpAMPClient sets, but that the client takes as it opens.
	if s.config.Server.Auth != (component.ID{}) {
		if settings.HeaderFunc, err = extensions.MakeHeadersFunc(s.telemetrySettings.Logger, s.config.Server.Auth, s.extensions.GetExtensions()); err != nil {
			return nil, fmt.Errorf("failed to create auth header function: %w", err)
		}
	}
	if s.config.Capabilities.ReportsHeartbeat {
		d := time.Duration(s.heartbeatIntervalSeconds) * time.Second
		settings.HeartbeatInterval = &d
	}
	if err := c.Open(s.runCtx, settings); err != nil {
		return nil, fmt.Errorf("read the OpAMP client's state: %w", err)
	}
	s.closeOpenedClient()
	s.openedClient = c

	resumed := c.Resumed()
	id := uuid.UUID(c.InstanceUID())
	s.resumed = resumed
	p := &persistentState{InstanceID: id, logger: s.telemetrySettings.Logger}
	if refused := resumed.RemoteConfigRefused; refused != nil {
		p.LastRemoteConfigStatus = &RemoteConfigStatus{
			Status:               refused.GetStatus(),
			LastRemoteConfigHash: fmt.Sprintf("%x", refused.GetLastRemoteConfigHash()),
			ErrorMessage:         refused.GetErrorMessage(),
		}
	}
	return p, nil
}

// takeOpenedClient is the client clientPersistentState opened, for startOpAMPClient to start;
// nil once taken (a client started again is a new one).
func (s *Supervisor) takeOpenedClient() *coreClient {
	c := s.openedClient
	s.openedClient = nil
	return c
}

// closeOpenedClient closes the client clientPersistentState opened, if nothing started it.
func (s *Supervisor) closeOpenedClient() {
	if c := s.takeOpenedClient(); c != nil {
		_ = c.Stop(context.Background())
	}
}

// configuredInstanceID is agent::instance_id, or zero: none, or one that isn't a UUID (the
// client then uses the one it kept, or makes one).
func (s *Supervisor) configuredInstanceID() uuid.UUID {
	if s.config.Agent.InstanceID == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(s.config.Agent.InstanceID)
	if err != nil {
		s.telemetrySettings.Logger.Warn("Failed to parse instance_id, generating one automatically", zap.Error(err))
		return uuid.Nil
	}
	return id
}

// loadResumedRemoteConfig takes the remote config the collector starts on from what the client
// resumes (startupConfig).
func (s *Supervisor) loadResumedRemoteConfig() {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		s.telemetrySettings.Logger.Debug("Remote config is not supported, will not attempt to load config from file")
		return
	}
	st := startupConfig(s.resumed, true, s.config.Agent.AutomaticConfigRollback)
	if st.lastWorking != nil {
		s.lastWorkingRemoteConfig.Store(st.lastWorking)
	}
	if s.remoteConfig.Load() != nil {
		// The server sent a config as the client started (Start starts it first, as upstream):
		// newer than the one resumed, and what the collector starts on.
		return
	}
	if st.config == nil {
		s.telemetrySettings.Logger.Info("No last received remote config found")
		return
	}
	if st.rolledBack {
		s.telemetrySettings.Logger.Info("The last remote config received failed: starting on the last working one")
	}
	// One a tightened policy refuses isn't run; the server sends another.
	if err := s.checkRemoteConfigPolicy(st.config); err != nil {
		s.telemetrySettings.Logger.Error("Not running the saved remote config", zap.Error(err))
		s.usingLastWorkingRemoteConfig.Store(false)
		return
	}
	s.usingLastWorkingRemoteConfig.Store(st.rolledBack)
	s.remoteConfig.Store(st.config)
}

// hasResumedRemoteConfig says whether the client resumes a remote config (upstream's
// hasPersistedRemoteConfig): the collector then doesn't start on the fallback config.
func (s *Supervisor) hasResumedRemoteConfig() bool {
	return resumesRemoteConfig(s.resumed, s.config.Capabilities.AcceptsRemoteConfig)
}

// clientKeeps says whether name is one of upstream's files the client's state holds instead.
func clientKeeps(name string) bool {
	switch name {
	case lastRecvRemoteConfigFile, lastWorkingRemoteConfigFile, lastRecvOwnTelemetryConfigFile:
		return true
	}
	return false
}

// resumedFile is what upstream read from name, from what the client resumes. ok is false when
// the client resumes none.
func (s *Supervisor) resumedFile(name string) (data []byte, ok bool, err error) {
	var m proto.Message
	switch name {
	case lastRecvRemoteConfigFile:
		if r := s.resumed.RemoteConfig; r != nil {
			m = r
		}
	case lastWorkingRemoteConfigFile:
		if r := s.resumed.AppliedRemoteConfig; r != nil {
			m = r
		}
	case lastRecvOwnTelemetryConfigFile:
		if r := s.resumed.ConnectionSettings; r != nil {
			m = r
		}
	}
	if m == nil {
		return nil, false, nil
	}
	data, err = proto.Marshal(m)
	return data, true, err
}
