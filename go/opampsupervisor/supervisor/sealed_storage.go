// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"path/filepath"

	"github.com/pydantic/opamp-clients/go/statekey"
)

// loadStorageSealer loads storage.state_key's key, if one is set: it seals the OpAMP client's
// state file, which holds what the server sent (the remote configs and own-telemetry settings,
// which may carry credentials). The collector's config files can't be sealed: the collector
// reads them. Loaded with the first OpAMP client, and kept for the clients that replace it.
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
