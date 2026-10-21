// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// startup is the remote config the collector starts on, from what the OpAMP client resumes.
type startup struct {
	// config is the remote config to start on; nil: none (the local config only). Nothing is
	// known of it: whether it runs healthy is for the collector started on it to show, when the
	// client hands it over again (configRetargets), even if a collector ran it healthy before.
	config *protobufs.AgentRemoteConfig
	// lastWorking is the config a failed one is rolled back to: the last applied, with
	// agent::automatic_config_rollback on.
	lastWorking *protobufs.AgentRemoteConfig
	// rolledBack: config is the last applied, in place of the last config received, which was
	// refused.
	rolledBack bool
}

// startupConfig decides what the collector starts on:
//
//   - The last config received, unless it was refused: the client hands it over again at start.
//   - Otherwise the last applied (with none received, one refused, or what was received from
//     another endpoint). A refused config isn't run again: the client doesn't hand it over until
//     the server offers another. Refused before it ran (it didn't compose, or the policy refused
//     it), the collector went on running the one before; refused after it ran, the collector goes
//     back to the last one that worked. It was run again, and failed again, with
//     agent::automatic_config_rollback off.
//
// Without accepts_remote_config, none.
func startupConfig(r opamp.Resumed, acceptsRemoteConfig, rollback bool) startup {
	if !acceptsRemoteConfig {
		return startup{}
	}
	var st startup
	if rollback {
		st.lastWorking = r.AppliedRemoteConfig
	}
	st.config = r.RemoteConfig
	if st.config == nil {
		st.config = r.AppliedRemoteConfig
		st.rolledBack = st.config != nil && r.RemoteConfigRefused != nil
	}
	return st
}

// resumesRemoteConfig says whether the client resumes any remote config: the collector then
// doesn't start on the fallback config.
func resumesRemoteConfig(r opamp.Resumed, acceptsRemoteConfig bool) bool {
	return acceptsRemoteConfig && (r.RemoteConfig != nil || r.AppliedRemoteConfig != nil || r.RemoteConfigRefused != nil)
}
