// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
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
	const applied, unset = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_UNSET
	for _, tc := range []struct {
		name       string
		resumed    opamp.Resumed
		rollback   bool
		want       string // the config's hash; "" none
		status     protobufs.RemoteConfigStatuses
		rolledBack bool
	}{
		{name: "nothing saved", want: "", status: unset},
		{name: "the last received, applied", resumed: opamp.Resumed{RemoteConfig: cfg("a"), AppliedRemoteConfig: cfg("a")}, want: "a", status: applied},
		{name: "the last received, not applied yet", resumed: opamp.Resumed{RemoteConfig: cfg("b"), AppliedRemoteConfig: cfg("a")}, want: "b", status: unset},
		{name: "the last received, nothing applied", resumed: opamp.Resumed{RemoteConfig: cfg("b")}, want: "b", status: unset},
		{name: "refused, rollback on", rollback: true, resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a"), RemoteConfigRefused: refused}, want: "a", status: applied, rolledBack: true},
		{name: "refused, rollback off", resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a"), RemoteConfigRefused: refused}, want: "a", status: applied, rolledBack: true},
		{name: "refused, nothing applied", rollback: true, resumed: opamp.Resumed{RemoteConfigRefused: refused}, want: "", status: unset},
		{name: "another endpoint's: the applied one", resumed: opamp.Resumed{AppliedRemoteConfig: cfg("a")}, want: "a", status: applied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := startupConfig(tc.resumed, true, tc.rollback)
			require.Equal(t, tc.want, string(st.config.GetConfigHash()))
			require.Equal(t, tc.status, st.status)
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
