// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// What the collector starts on, from what the OpAMP client resumes.
func TestStartupConfig(t *testing.T) {
	cfg := func(hash string) *protobufs.AgentRemoteConfig {
		return &protobufs.AgentRemoteConfig{ConfigHash: []byte(hash)}
	}
	refused := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: []byte("bad"), Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED}
	for _, tc := range []struct {
		name       string
		resumed    opamp.Resumed
		rollback   bool
		want       string // the config's hash; "" none
		rolledBack bool
	}{
		{name: "nothing saved", want: ""},
		{name: "the last received, applied", resumed: opamp.Resumed{RemoteConfig: cfg("a"), AppliedRemoteConfig: cfg("a")}, want: "a"},
		{name: "the last received, not applied yet", resumed: opamp.Resumed{RemoteConfig: cfg("b"), AppliedRemoteConfig: cfg("a")}, want: "b"},
		{name: "the last received, nothing applied", resumed: opamp.Resumed{RemoteConfig: cfg("b")}, want: "b"},
		{name: "refused, rollback on", rollback: true, resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a"), RemoteConfigRefused: refused}, want: "a", rolledBack: true},
		{name: "refused, rollback off", resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a"), RemoteConfigRefused: refused}, want: "a", rolledBack: true},
		{name: "refused, nothing applied", rollback: true, resumed: opamp.Resumed{RemoteConfigRefused: refused}, want: ""},
		{name: "another endpoint's: the applied one", resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a")}, want: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := startupConfig(tc.resumed, true, tc.rollback)
			require.Equal(t, tc.want, string(st.config.GetConfigHash()))
			require.Equal(t, tc.rolledBack, st.rolledBack)
			if tc.rollback {
				require.Same(t, tc.resumed.AppliedRemoteConfig, st.lastWorking)
			} else {
				require.Nil(t, st.lastWorking)
			}
		})
	}
	require.Equal(t, startup{}, startupConfig(opamp.Resumed{RemoteConfig: cfg("a")}, false, true), "without accepts_remote_config")
}

// startupChoice is one input of startupConfig and what it chose, by config hash ("" for none).
type startupChoice struct {
	Remote      string `json:"remote"`
	Applied     string `json:"applied"`
	Refused     bool   `json:"refused"`
	Accepts     bool   `json:"accepts"`
	Rollback    bool   `json:"rollback"`
	Config      string `json:"config"`
	LastWorking string `json:"lastWorking"`
	RolledBack  bool   `json:"rolledBack"`
}

// startupConfigTableEnv names a file to write startupConfig's choice on every input to, as JSON
// lines, for a check of the rule outside this package.
const startupConfigTableEnv = "SUPERVISOR_STARTUP_CONFIG_TABLE"

// startupConfig on every input, with two configs: what the client resumes (none, one or the other
// as the config handed over and as the last applied; a refusal or none), with and without
// accepts_remote_config and agent::automatic_config_rollback. Each choice is a config it was
// given, and the same again for the same input.
func TestStartupConfigOnEveryInput(t *testing.T) {
	hashes := []string{"", "a", "b"}
	cfg := func(hash string) *protobufs.AgentRemoteConfig {
		if hash == "" {
			return nil
		}
		return &protobufs.AgentRemoteConfig{ConfigHash: []byte(hash)}
	}
	var rows []startupChoice
	for _, remote := range hashes {
		for _, applied := range hashes {
			for _, refused := range []bool{false, true} {
				for _, accepts := range []bool{false, true} {
					for _, rollback := range []bool{false, true} {
						r := opamp.Resumed{RemoteConfig: cfg(remote), AppliedRemoteConfig: cfg(applied)}
						if refused {
							r.RemoteConfigRefused = &protobufs.RemoteConfigStatus{
								LastRemoteConfigHash: []byte("refused"),
								Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
							}
						}
						st := startupConfig(r, accepts, rollback)
						require.Contains(t, []*protobufs.AgentRemoteConfig{nil, r.RemoteConfig, r.AppliedRemoteConfig}, st.config)
						require.Equal(t, st, startupConfig(r, accepts, rollback))
						rows = append(rows, startupChoice{
							Remote: remote, Applied: applied, Refused: refused, Accepts: accepts, Rollback: rollback,
							Config: string(st.config.GetConfigHash()), LastWorking: string(st.lastWorking.GetConfigHash()),
							RolledBack: st.rolledBack,
						})
					}
				}
			}
		}
	}
	require.Len(t, rows, 3*3*2*2*2)
	path := os.Getenv(startupConfigTableEnv)
	if path == "" {
		return
	}
	f, err := os.Create(path)
	require.NoError(t, err)
	enc := json.NewEncoder(f)
	for _, row := range rows {
		require.NoError(t, enc.Encode(row))
	}
	require.NoError(t, f.Close())
}
