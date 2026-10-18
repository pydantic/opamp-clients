// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: a state file
// with no instance ID (empty, null or {}) is replaced by a new one instead of crashing the
// Supervisor or giving it the all-zero UID, the file is replaced atomically, and the instance ID
// can be read from another goroutine (instanceID).

package supervisor

import (
	"encoding/hex"
	"errors"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// persistentState represents persistent state for the supervisor
type persistentState struct {
	// InstanceID must be a valid UUID string. If it is not, a new UUIDv7 will be generated automatically.
	InstanceID             uuid.UUID           `yaml:"instance_id"`
	LastRemoteConfigStatus *RemoteConfigStatus `yaml:"last_remote_config_status"`

	// Path to the config file that the state should be saved to.
	// This is not marshaled.
	configPath string      `yaml:"-"`
	logger     *zap.Logger `yaml:"-"`
	// idMu guards InstanceID for instanceID, which is read off the goroutine that sets it.
	idMu sync.RWMutex
}

// RemoteConfigStatus is a custom struct that is used to marshal/unmarshal the remote config status.
// LastRemoteConfigHash is a hex encoded string of the last remote config hash for human readability.
type RemoteConfigStatus struct {
	// Status is the status of the last remote config.
	Status protobufs.RemoteConfigStatuses `yaml:"status"`
	// LastRemoteConfigHash is a hex encoded string of the last remote config hash for human readability.
	LastRemoteConfigHash string `yaml:"last_remote_config_hash"`
	// ErrorMessage is the error message of the last remote config.
	ErrorMessage string `yaml:"error_message"`
}

func (p *persistentState) SetInstanceID(id uuid.UUID) error {
	p.idMu.Lock()
	p.InstanceID = id
	p.idMu.Unlock()
	return p.writeState()
}

// instanceID is InstanceID, for goroutines other than the one that sets it.
func (p *persistentState) instanceID() uuid.UUID {
	p.idMu.RLock()
	defer p.idMu.RUnlock()
	return p.InstanceID
}

func (p *persistentState) SetLastRemoteConfigStatus(status *protobufs.RemoteConfigStatus) error {
	p.LastRemoteConfigStatus = &RemoteConfigStatus{
		Status:               status.Status,
		LastRemoteConfigHash: hex.EncodeToString(status.LastRemoteConfigHash),
		ErrorMessage:         status.ErrorMessage,
	}
	return p.writeState()
}

func (p *persistentState) GetLastRemoteConfigStatus() *protobufs.RemoteConfigStatus {
	if p.LastRemoteConfigStatus == nil {
		return nil
	}
	lastRemoteConfigHash, err := hex.DecodeString(p.LastRemoteConfigStatus.LastRemoteConfigHash)
	if err != nil {
		p.logger.Error("Failed to decode last remote config hash, returning empty status", zap.Error(err))
		return nil
	}
	return &protobufs.RemoteConfigStatus{
		Status:               p.LastRemoteConfigStatus.Status,
		LastRemoteConfigHash: lastRemoteConfigHash,
		ErrorMessage:         p.LastRemoteConfigStatus.ErrorMessage,
	}
}

func (p *persistentState) writeState() error {
	by, err := yaml.Marshal(p)
	if err != nil {
		return err
	}

	// Write and rename, so a crash mid-write leaves the old file instead of a truncated one.
	tmp := p.configPath + ".tmp"
	if err := os.WriteFile(tmp, by, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.configPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadOrCreatePersistentState attempts to load the persistent state from disk. If it doesn't
// exist, a new persistent state file is created.
// instanceID must be a valid UUID string, or an empty string to generate a new UUIDv7 automatically.
func loadOrCreatePersistentState(file, instanceID string, logger *zap.Logger) (*persistentState, error) {
	state, err := loadPersistentState(file, logger)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return createNewPersistentState(file, instanceID, logger)
	case err == nil && (state == nil || state.InstanceID == uuid.Nil):
		logger.Warn("The persistent state file has no instance ID, replacing it", zap.String("file", file))
		return createNewPersistentState(file, instanceID, logger)
	case err != nil:
		return nil, err
	default:
		return state, nil
	}
}

func loadPersistentState(file string, logger *zap.Logger) (*persistentState, error) {
	var state *persistentState

	by, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(by, &state); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, nil // an empty or null file
	}

	state.configPath = file
	state.logger = logger

	return state, nil
}

func createNewPersistentState(file, instanceID string, logger *zap.Logger) (*persistentState, error) {
	id, err := uuid.Parse(instanceID)
	if err != nil {
		if instanceID != "" {
			logger.Warn("Failed to parse instance_id, generating one automatically", zap.Error(err))
		}
		id, err = uuid.NewV7()
		if err != nil {
			return nil, err
		}
	}

	p := &persistentState{
		InstanceID: id,
		configPath: file,
		logger:     logger,
	}

	err = p.writeState()
	return p, err
}
