// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package supervisor

import (
	"os"
	"path/filepath"

	"github.com/pydantic/opamp-clients/go/statekey"
)

// loadStorageSealer loads storage.state_key's key, if one is set: it seals the OpAMP client's
// state file and the Supervisor's own copies of what the server sent (the remote configs and
// own-telemetry settings, which may carry credentials). The collector's config files can't be
// sealed: the collector reads them. Loaded with the first OpAMP client, before the Supervisor
// reads any of those files, and kept for the clients that replace it.
func (s *Supervisor) loadStorageSealer() error {
	if s.config.Storage.StateKey == "" || s.storageSealer != nil {
		return nil
	}
	// The key is the client state file's (a keychain or DPAPI key is named after it).
	key, err := statekey.Load(s.config.Storage.StateKey, filepath.Join(s.config.Storage.Directory, coreClientStateFile))
	if err != nil {
		return err
	}
	s.storageSealer = key
	return nil
}

// writeStorageFile writes a file in the storage directory, for the Supervisor's user only,
// sealed when storage.state_key is set.
func (s *Supervisor) writeStorageFile(name string, data []byte) error {
	if s.storageSealer != nil {
		sealed, err := s.storageSealer.Seal(data)
		if err != nil {
			return err
		}
		data = sealed
	}
	return os.WriteFile(filepath.Join(s.config.Storage.Directory, name), data, 0o600)
}

// readStorageFile reads a file writeStorageFile wrote. One written before sealing was turned on
// is read as it is (the next write seals it).
func (s *Supervisor) readStorageFile(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.config.Storage.Directory, name))
	if err != nil || s.storageSealer == nil {
		return data, err
	}
	return s.storageSealer.Open(data)
}
