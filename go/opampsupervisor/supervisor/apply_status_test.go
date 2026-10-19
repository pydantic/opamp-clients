// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// applyTestSupervisor runs the agent loop with a stand-in collector, and records the remote
// config statuses it reports. The test plays the collector's health reports.
type applyTestSupervisor struct {
	*Supervisor
	mu       sync.Mutex
	statuses []*protobufs.RemoteConfigStatus
	conns    int
}

func newApplyTestSupervisor(t *testing.T, helper string) *applyTestSupervisor {
	t.Helper()
	r := newRestartTestSupervisor(t, false)
	a := &applyTestSupervisor{Supervisor: r.Supervisor}
	a.config.Agent.ConfigApplyTimeout = 700 * time.Millisecond
	a.opampClient.(*mockOpAMPClient).setRemoteConfigStatusFunc = func(rcs *protobufs.RemoteConfigStatus) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.statuses = append(a.statuses, rcs)
		return nil
	}
	a.agentDescription.Store(&protobufs.AgentDescription{})
	require.NoError(t, a.createTemplates())
	a.commander = newHelperCommander(t, helper)
	return a
}

// receiversConfig is a remote config the test Supervisor composes, with body as its receivers.
func receiversConfig(hash, receivers string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"": {Body: []byte("receivers:\n  " + receivers + ": null\n")},
		}},
		ConfigHash: []byte(hash),
	}
}

// offer gives the Supervisor rc as the server would, and the agent loop the config if it
// changed.
func (a *applyTestSupervisor) offer(t *testing.T, rc *protobufs.AgentRemoteConfig) {
	t.Helper()
	if a.processRemoteConfigMessage(t.Context(), rc) {
		select {
		case a.hasNewConfig <- struct{}{}:
		default:
		}
	}
}

// connect is a new collector connection to the local server; the one before is closed.
func (a *applyTestSupervisor) connect() *mockConn {
	if old := a.agentConn.current(); old != nil {
		a.agentConn.release(old)
	}
	conn := &mockConn{}
	a.agentConn.claim(conn)
	return conn
}

// health is the collector's health report over conn.
func (a *applyTestSupervisor) health(conn *mockConn, healthy bool) {
	n, _ := a.agentConn.held(conn)
	a.lastHealthFromClient.Store(&agentHealth{ComponentHealth: &protobufs.ComponentHealth{Healthy: healthy}, connection: n})
}

func (a *applyTestSupervisor) reported() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.statuses {
		out = append(out, fmt.Sprintf("%s %s", s.LastRemoteConfigHash, strings.TrimPrefix(s.Status.String(), "RemoteConfigStatuses_")))
	}
	return out
}

// waitFor waits until want has been reported (each "hash STATUS").
func (a *applyTestSupervisor) waitFor(t *testing.T, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, r := range a.reported() {
			if r == want {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "never reported %q: %v", want, a.reported())
}

// A config that fails while another's apply is under way is FAILED, and only FAILED: the
// running config's outcome is about the running config. It was reported about the config
// that came last, so the failed config became APPLIED, and the last working one.
func TestAConfigThatFailsDuringAnApplyStaysFailed(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	a.config.Agent.AutomaticConfigRollback = true
	startAgentLoop(t, a.Supervisor)

	a.offer(t, receiversConfig("A", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	conn := a.connect()
	a.offer(t, &protobufs.AgentRemoteConfig{
		Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("receivers: [")}}},
		ConfigHash: []byte("B"),
	})
	a.waitFor(t, "B FAILED")
	a.health(conn, true)

	a.waitFor(t, "A APPLIED")
	require.NotContains(t, a.reported(), "B APPLIED")
	require.Equal(t, []byte("A"), a.lastWorkingRemoteConfig.Load().GetConfigHash())
	require.Equal(t, []byte("A"), a.remoteConfig.Load().GetConfigHash(), "the config that failed isn't the current one")
}

// Health the collector reported before it was given the config, as the old collector can just
// before it is stopped, says nothing about the config: a collector that never reports healthy
// on it is FAILED at the timeout.
func TestHealthFromBeforeTheApplyDoesNotApplyIt(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	old := a.connect()
	startAgentLoop(t, a.Supervisor)

	a.offer(t, receiversConfig("A", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	a.health(old, true)
	a.waitFor(t, "A FAILED")
	require.NotContains(t, a.reported(), "A APPLIED")

	// Healthy on a connection made after it, the next config is APPLIED.
	a.offer(t, receiversConfig("B", "nop/2"))
	require.Eventually(t, func() bool { return a.cfgState.Load().(*configState).remote.GetConfigHash() != nil }, time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	a.health(a.connect(), true)
	a.waitFor(t, "B APPLIED")
}

// A config whose merged body is the running one's gets the running apply's outcome: APPLYING
// while it is under way, then its outcome; or its outcome at once if it is known. It was
// APPLIED at once, and then FAILED if the apply failed.
func TestUnchangedRemoteConfigGetsTheRunningConfigsOutcome(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	startAgentLoop(t, a.Supervisor)

	a.offer(t, receiversConfig("A", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	conn := a.connect()
	a.offer(t, receiversConfig("A2", "nop"))
	a.waitFor(t, "A2 APPLYING")
	a.health(conn, true)
	a.waitFor(t, "A2 APPLIED")
	require.NotContains(t, a.reported(), "A APPLIED", "the outcome is the latest config's")

	a.offer(t, receiversConfig("A3", "nop"))
	a.waitFor(t, "A3 APPLIED")
	require.NotContains(t, a.reported(), "A3 APPLYING")

	// After a failed apply, a config with the failed body is FAILED.
	a.offer(t, receiversConfig("B", "nop/2"))
	a.waitFor(t, "B FAILED")
	a.offer(t, receiversConfig("B2", "nop/2"))
	a.waitFor(t, "B2 FAILED")
	require.NotContains(t, a.reported(), "B2 APPLIED")
}

// A collector that exits on a config after it was APPLIED makes it FAILED, and APPLIED again
// once the restarted collector reports healthy on it. It stayed APPLIED while the collector
// crash-looped.
func TestACollectorExitAfterAPPLIEDIsFAILED(t *testing.T) {
	a := newApplyTestSupervisor(t, "exit-soon")
	a.config.Agent.ConfigApplyTimeout = 300 * time.Millisecond
	startAgentLoop(t, a.Supervisor)

	a.offer(t, receiversConfig("A", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	a.health(a.connect(), true)
	a.waitFor(t, "A APPLIED")
	a.waitFor(t, "A FAILED")

	// Restarted on the backoff, and healthy again.
	require.Eventually(t, a.commander.IsRunning, 10*time.Second, 10*time.Millisecond)
	a.health(a.connect(), true)
	require.Eventually(t, func() bool {
		r := a.reported()
		return r[len(r)-1] == "A APPLIED" && len(r) >= 3
	}, 10*time.Second, 10*time.Millisecond, "%v", a.reported())
}

// After a config fails and the last working one is restored, the server is told the failed one
// is FAILED, and that the last working one runs in its place. Nothing is reported about the
// last working config itself: the core drops statuses about any config but the latest.
func TestARollbackReportsOnlyTheFailedConfig(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	a.config.Agent.AutomaticConfigRollback = true
	startAgentLoop(t, a.Supervisor)

	a.offer(t, receiversConfig("W", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	a.health(a.connect(), true)
	a.waitFor(t, "W APPLIED")

	a.offer(t, receiversConfig("R", "nop/2"))
	a.waitFor(t, "R FAILED")
	a.mu.Lock()
	last := a.statuses[len(a.statuses)-1]
	a.mu.Unlock()
	require.Contains(t, last.ErrorMessage, "The last working remote config runs in its place.")
	require.Eventually(t, func() bool {
		return bytes.Equal(a.cfgState.Load().(*configState).remote.GetConfigHash(), []byte("W"))
	}, 5*time.Second, 10*time.Millisecond)
	a.health(a.connect(), true)
	time.Sleep(3 * a.config.Agent.ConfigApplyTimeout)
	require.Equal(t, []string{"W APPLYING", "W APPLIED", "R APPLYING", "R FAILED"}, a.reported())
	require.True(t, proto.Equal(&protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte("R"),
		Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
		ErrorMessage:         last.ErrorMessage,
	}, a.persistentState.GetLastRemoteConfigStatus()), "the next start runs the last working config")
}

// connectionStatuses records the connection settings statuses a reports.
func (a *applyTestSupervisor) connectionStatuses() func() []string {
	var mu sync.Mutex
	var got []string
	a.opampClient.(*mockOpAMPClient).setConnectionStatusFunc = func(st *protobufs.ConnectionSettingsStatus) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s %s %s", st.LastConnectionSettingsHash,
			strings.TrimPrefix(st.Status.String(), "ConnectionSettingsStatuses_"), st.ErrorMessage))
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func ownMetrics(hash, endpoint string) *types.MessageData {
	return &types.MessageData{
		OfferedConnectionsSettingsHash: []byte(hash),
		OwnMetricsConnSettings:         &protobufs.TelemetryConnectionSettings{DestinationEndpoint: endpoint},
	}
}

// Own telemetry settings are APPLIED once the collector runs healthy with them, and FAILED if it
// doesn't. They were APPLIED as they arrived, before the collector restarted.
func TestOwnTelemetrySettingsAreAnsweredWithTheApply(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	got := a.connectionStatuses()
	startAgentLoop(t, a.Supervisor)
	a.offer(t, receiversConfig("A", "nop"))
	require.Eventually(t, a.commander.IsRunning, 5*time.Second, 10*time.Millisecond)
	a.health(a.connect(), true)
	a.waitFor(t, "A APPLIED")

	a.onMessage(t.Context(), ownMetrics("m1", "https://ingest.example/v1/metrics"))
	require.Empty(t, got(), "answered before the collector ran with them")
	require.Eventually(t, func() bool { return a.pendingConnectionSettings.Load() == nil }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	a.health(a.connect(), true)
	require.Eventually(t, func() bool { return len(got()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"m1 APPLIED "}, got())

	// The collector doesn't come up healthy on the next.
	a.onMessage(t.Context(), ownMetrics("m2", "https://ingest.example/v2/metrics"))
	require.Eventually(t, func() bool { return len(got()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "m2 FAILED The Collector didn't run with the offered settings.", got()[1])

	// The same settings again: the collector runs with them already.
	a.onMessage(t.Context(), ownMetrics("m3", "https://ingest.example/v2/metrics"))
	require.Equal(t, "m3 APPLIED ", got()[2])
}

// Own telemetry settings the Supervisor can't give the collector are FAILED, and neither applied
// nor saved. They were dropped, and the rest APPLIED.
func TestUnsupportedOwnTelemetrySettingsAreFAILED(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	got := a.connectionStatuses()
	a.agentConfigOwnTelemetrySection.Store("")
	before := a.cfgState.Load()

	for _, tc := range []struct {
		name string
		set  *protobufs.TelemetryConnectionSettings
		want string
	}{
		{"tls", &protobufs.TelemetryConnectionSettings{Tls: &protobufs.TLSConnectionSettings{InsecureSkipVerify: true}}, "own_metrics's tls settings"},
		{"certificate", &protobufs.TelemetryConnectionSettings{Certificate: &protobufs.TLSCertificate{}}, "own_metrics's certificate settings"},
		{"proxy", &protobufs.TelemetryConnectionSettings{Proxy: &protobufs.ProxyConnectionSettings{Url: "http://proxy"}}, "own_metrics's proxy settings"},
	} {
		tc.set.DestinationEndpoint = "https://ingest.example/v1/metrics"
		a.onMessage(t.Context(), &types.MessageData{OfferedConnectionsSettingsHash: []byte(tc.name), OwnMetricsConnSettings: tc.set})
		require.Equal(t, fmt.Sprintf("%s FAILED The Supervisor can't apply %s.", tc.name, tc.want), got()[len(got())-1])
	}
	require.Len(t, got(), 3)
	require.Same(t, before, a.cfgState.Load(), "the collector's config is unchanged")
	require.Empty(t, a.agentConfigOwnTelemetrySection.Load())
	require.Empty(t, a.hasNewConfig)
	_, err := os.Stat(filepath.Join(a.config.Storage.Directory, lastRecvOwnTelemetryConfigFile))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The server's endpoint and header values reach the collector as written: no quote breaks the
// config, no escape changes them, and no ${...} in them is expanded by the collector. They went
// into the YAML as written.
func TestOwnTelemetryValuesReachTheCollectorAsWritten(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	a.agentConfigOwnTelemetrySection.Store("")
	value := `Bearer a"b\x41c ${env:HOME} $${file:/etc/passwd} ` + "\n"
	endpoint := `https://ingest.example/v1/metrics?q=${env:HOME}`
	changed, err := a.processOwnTelemetryConnSettingsMessage(t.Context(), &protobufs.ConnectionSettingsOffers{
		OwnMetrics: &protobufs.TelemetryConnectionSettings{
			DestinationEndpoint: endpoint,
			Headers:             &protobufs.Headers{Headers: []*protobufs.Header{{Key: `Author"ization`, Value: value}}},
		},
	})
	require.NoError(t, err)
	require.True(t, changed)

	// What the collector reads: the merged config, with confmap's $$ escape undone.
	conf, err := config.NewConfFromYAML([]byte(a.cfgState.Load().(*configState).mergedConfig))
	require.NoError(t, err)
	exporter := conf.Get("service::telemetry::metrics::readers").([]any)[0].(map[string]any)["periodic"].(map[string]any)["exporter"].(map[string]any)["otlp"].(map[string]any)
	unescape := func(s string) string { return strings.ReplaceAll(s, "$$", "$") }
	require.Equal(t, endpoint, unescape(exporter["endpoint"].(string)))
	headers := exporter["headers"].(map[string]any)
	require.Equal(t, value, unescape(headers[`Author"ization`].(string)))
}

// With the remote config policy on, the own telemetry destination must be one it allows. It went
// around the policy.
func TestOwnTelemetryIsHeldToTheRemoteConfigPolicy(t *testing.T) {
	a := newApplyTestSupervisor(t, "sleep")
	a.agentConfigOwnTelemetrySection.Store("")
	a.config.Agent.RemoteConfigPolicy = config.RemoteConfigPolicy{
		AllowedComponents:        map[string][]string{"receivers": {"nop"}},
		AllowedExporterEndpoints: []string{"https://ingest.example"},
	}
	offer := func(endpoint string) error {
		_, err := a.processOwnTelemetryConnSettingsMessage(t.Context(), &protobufs.ConnectionSettingsOffers{
			OwnMetrics: &protobufs.TelemetryConnectionSettings{DestinationEndpoint: endpoint},
		})
		return err
	}
	require.ErrorContains(t, offer("https://elsewhere.example/v1/metrics"), "elsewhere.example")
	require.Empty(t, a.agentConfigOwnTelemetrySection.Load())
	require.NoError(t, offer("https://ingest.example/v1/metrics"))
}
