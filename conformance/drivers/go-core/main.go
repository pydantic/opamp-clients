// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Command driver runs our Go client (go/opamp: the shared core in wazero) under the conformance
// driver contract (conformance/DRIVERS.md).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp"
)

var outMu sync.Mutex

func emit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = event
	fields["wall"] = float64(time.Now().UnixNano()) / 1e9
	line, _ := json.Marshal(fields)
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Println(string(line))
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

// instanceUID is the 32 hex characters given, or none (random, none, zero): the core makes one,
// a UUIDv7. (All zeros is "none" to the Go client.)
func instanceUID() [16]byte {
	var uid [16]byte
	switch v := os.Getenv("DRIVER_UID"); v {
	case "", "random", "zero", "none":
	default:
		b, _ := hex.DecodeString(v)
		copy(uid[:], b)
	}
	return uid
}

func description(change int) *protobufs.AgentDescription {
	str := func(k, v string) *protobufs.KeyValue {
		return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}}
	}
	nonIdentifying := []*protobufs.KeyValue{str("driver.change", strconv.Itoa(change))}
	// DRIVER_DESCRIPTION_PAD: a description that long (with DRIVER_MAX_SEND_BYTES, too large to send).
	if pad, err := strconv.Atoi(os.Getenv("DRIVER_DESCRIPTION_PAD")); err == nil {
		nonIdentifying = append(nonIdentifying, str("driver.pad", strings.Repeat("x", pad)))
	}
	return &protobufs.AgentDescription{
		IdentifyingAttributes:    []*protobufs.KeyValue{str("service.name", "conformance-go-core")},
		NonIdentifyingAttributes: nonIdentifying,
	}
}

func main() {
	endpoint := os.Getenv("OPAMP_ENDPOINT")
	runFor := time.Duration(envFloat("DRIVER_RUN_S", 30) * float64(time.Second))
	poll := time.Duration(envFloat("DRIVER_POLL_S", 2) * float64(time.Second))

	cfg := opamp.Config{
		Endpoint:          endpoint,
		InstanceUID:       instanceUID(),
		Capabilities:      uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
		HeartbeatInterval: poll,
	}
	if n, err := strconv.ParseUint(os.Getenv("DRIVER_MAX_SEND_BYTES"), 10, 64); err == nil {
		cfg.MaxSendBytes = n
	}
	// The mode is always asked for by name, never left to the client's default: go-core is core
	// mode, where the core speaks HTTP and WebSocket itself over Go's connections and crypto/tls;
	// go-core-platform (OPAMP_GO_MODE=platform) platform mode (net/http and gorilla).
	platform := os.Getenv("OPAMP_GO_MODE") == "platform"
	cfg.Mode = opamp.ModeCore
	if platform {
		cfg.Mode = opamp.ModePlatform
	}
	if token := os.Getenv("DRIVER_TOKEN"); token != "" {
		cfg.Header = http.Header{"Authorization": {"Bearer " + token}}
	}
	if err := connectionOptions(&cfg); err != nil {
		emit("fatal", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	cfg.StateFile = os.Getenv("DRIVER_STATE_FILE")
	cfg.AcceptConnectionSettings = os.Getenv("DRIVER_ACCEPT_CONNECTION_SETTINGS") == "1"
	cfg.AcceptPackages = os.Getenv("DRIVER_PACKAGES") == "1"
	cfg.RequestInstanceUID = os.Getenv("DRIVER_REQUEST_UID") == "1"
	if os.Getenv("DRIVER_OWN_TELEMETRY") == "1" {
		cfg.Capabilities |= uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnMetrics |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnTraces |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnLogs)
	}
	// The status to start from, a restart after a crash going on from where the application was
	// (DRIVER_FIRST_CHANGE): the changes count on from it.
	first, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("DRIVER_FIRST_CHANGE")))
	status := opamp.Status{
		Description:     description(first),
		Health:          &protobufs.ComponentHealth{Healthy: true, StartTimeUnixNano: uint64(time.Now().UnixNano())},
		EffectiveConfig: &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{}},
	}
	if os.Getenv("DRIVER_COMPONENTS") == "1" {
		status.AvailableComponents = components()
	}
	if os.Getenv("DRIVER_CUSTOM") == "1" {
		status.CustomCapabilities = &protobufs.CustomCapabilities{Capabilities: []string{echo}}
	}

	// The callback applies remote config and telemetry settings. Other notifications reach
	// the main loop.
	notes := make(chan opamp.Notification, 64)
	// DRIVER_THROW_ON_CONFIG: the callback itself panics on the first remote config (RULE-SHELL-02).
	throwOnConfig := os.Getenv("DRIVER_THROW_ON_CONFIG") == "1"
	thrown := false
	// Closed once "started" is said: a run's events begin there, so a config the callback hears
	// first (Start has returned, the driver not yet said so) waits for it.
	said := make(chan struct{})
	client, err := opamp.Start(context.Background(), cfg, status, func(c *opamp.Client, n opamp.Notification) {
		if throwOnConfig && !thrown && n.Kind == opamp.RemoteConfig {
			thrown = true
			emit("throwing", nil)
			panic("callback-throws: the driver's callback panicked on the first remote config")
		}
		// Apply saved settings before the client's connection can open and report their status.
		if n.Kind == opamp.RemoteConfig || n.Kind == opamp.ConnectionSettings {
			<-said
			handle(c, notes, n)
			return
		}
		notes <- n
	})
	if err != nil {
		emit("fatal", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	uid := client.InstanceUID()
	name := "opamp-go-core (ours)"
	if platform {
		name = "opamp-go-core (ours, platform mode)"
	}
	emit("started", map[string]any{"uid": hex.EncodeToString(uid[:]), "client": name})
	close(said)

	var changes []time.Duration
	for _, s := range strings.Split(os.Getenv("DRIVER_CHANGES_AT_S"), ",") {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			changes = append(changes, time.Duration(f*float64(time.Second)))
		}
	}
	started := time.Now()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	deadline := time.After(runFor)
	next := 0
loop:
	for {
		var change <-chan time.Time
		if next < len(changes) {
			change = time.After(time.Until(started.Add(changes[next])))
		}
		select {
		case <-deadline:
			break loop
		case <-sig:
			break loop
		case <-change:
			next++
			emit("status_change", map[string]any{"change": first + next})
			client.SetDescription(description(first + next))
		case n := <-notes:
			handle(client, notes, n)
		}
	}
	emit("stopping", nil)
	// Bounded by the core (Config.StopTimeout).
	err = client.Stop(context.Background())
	emit("stopped", map[string]any{"error": errString(err)})
	// Stay a while, doing nothing: the server sees anything the client left running after stop.
	if linger := envFloat("DRIVER_LINGER_S", 0); linger > 0 {
		time.Sleep(time.Duration(linger * float64(time.Second)))
	}
}

// lingerThenExit stays DRIVER_LINGER_S seconds after a fatal, still reporting what the client says
// (nothing, if it keeps RULE-SHELL-02: a second Stopped is a second fatal), then exits 1.
func lingerThenExit(client *opamp.Client, notes <-chan opamp.Notification) {
	deadline := time.After(time.Duration(envFloat("DRIVER_LINGER_S", 0) * float64(time.Second)))
	for {
		select {
		case <-deadline:
			os.Exit(1)
		case n := <-notes:
			if n.Kind == opamp.Stopped {
				emit("fatal", map[string]any{"error": errString(n.Err)})
				continue
			}
			handle(client, notes, n)
		}
	}
}

// answerConfig reports a remote config and answers it APPLIED.
func answerConfig(client *opamp.Client, n opamp.Notification) {
	emit("message", map[string]any{"remote_config_hash": hex.EncodeToString(n.RemoteConfig.ConfigHash)})
	client.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: n.RemoteConfig.ConfigHash,
		Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
	})
}

func handle(client *opamp.Client, notes <-chan opamp.Notification, n opamp.Notification) {
	switch n.Kind {
	case opamp.Stopped:
		if n.Err != nil {
			// The client stopped on its own.
			emit("fatal", map[string]any{"error": n.Err.Error()})
			lingerThenExit(client, notes)
		}
	case opamp.Connected:
		emit("connect", nil)
	case opamp.ConnectFailed:
		emit("connect_failed", map[string]any{"error": n.Detail, "status": n.Status})
	case opamp.ErrorResponse:
		emit("error_response", map[string]any{"type": n.ErrorType.String(), "message": n.Message})
	case opamp.RemoteConfig:
		answerConfig(client, n)
	case opamp.CustomMessage:
		m := n.CustomMessage
		emit("custom_message", map[string]any{"capability": m.Capability, "type": m.Type, "data": hex.EncodeToString(m.Data)})
		if m.Type == "ping" {
			client.SendCustomMessage(&protobufs.CustomMessage{Capability: echo, Type: "pong", Data: m.Data})
		}
	case opamp.CustomMessageNotSent:
		emit("custom_message_not_sent", map[string]any{"reason": n.Detail})
	case opamp.ServerCustomCapabilities:
		emit("server_custom_capabilities", map[string]any{"capabilities": n.CustomCapabilities})
	case opamp.ConnectionSettings:
		s := n.ConnectionSettings
		var metrics any
		if s.OwnMetrics != nil {
			metrics = s.OwnMetrics.DestinationEndpoint
		}
		emit("telemetry_settings", map[string]any{"hash": hex.EncodeToString(s.Hash), "metrics_endpoint": metrics})
		client.SetConnectionSettingsStatus(&protobufs.ConnectionSettingsStatus{
			LastConnectionSettingsHash: s.Hash,
			Status:                     protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED,
		})
	case opamp.InstallPackage:
		file, err := os.ReadFile(n.Path)
		sum := sha256.Sum256(file)
		emit("install_package", map[string]any{"name": n.PackageName, "version": n.Package.Version,
			"sha256": hex.EncodeToString(sum[:]), "error": errString(err)})
		client.PackageInstalled(n.PackageName, n.Package.Hash, err)
	case opamp.RemovePackage:
		emit("remove_package", map[string]any{"name": n.PackageName})
		client.PackageRemoved(n.PackageName)
	case opamp.InstanceUIDChanged:
		emit("message", map[string]any{"new_instance_uid": hex.EncodeToString(n.InstanceUID[:])})
	case opamp.HeartbeatInterval:
		emit("connection_settings", map[string]any{"heartbeat_interval_seconds": int(n.HeartbeatInterval / time.Second)})
	}
}

// connectionOptions sets the TLS and proxy the driver was given (DRIVERS.md): the CA to trust
// instead of the system's, the client certificate and key to present, and the proxy. Package
// files come the same way as messages: Config.TLS and Config.Proxy carry downloads too.
func connectionOptions(cfg *opamp.Config) error {
	read := func(env string) ([]byte, error) {
		path := os.Getenv(env)
		if path == "" {
			return nil, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		return b, nil
	}
	var err error
	if cfg.TLS.CAPEM, err = read("DRIVER_CA_FILE"); err != nil {
		return err
	}
	if cfg.TLS.CertPEM, err = read("DRIVER_CLIENT_CERT"); err != nil {
		return err
	}
	if cfg.TLS.KeyPEM, err = read("DRIVER_CLIENT_KEY"); err != nil {
		return err
	}
	cfg.Proxy.URL = os.Getenv("DRIVER_PROXY")
	return nil
}

// echo is the custom capability DRIVERS.md names: ping is answered with pong.
const echo = "io.opentelemetry.echo"

// components are the ones DRIVERS.md names: an OTLP receiver (with its version) and a debug
// exporter.
func components() *protobufs.AvailableComponents {
	version := &protobufs.KeyValue{Key: "version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "1.0.0"}}}
	group := func(id string, c *protobufs.ComponentDetails) *protobufs.ComponentDetails {
		return &protobufs.ComponentDetails{SubComponentMap: map[string]*protobufs.ComponentDetails{id: c}}
	}
	return &protobufs.AvailableComponents{Components: map[string]*protobufs.ComponentDetails{
		"receivers": group("otlp", &protobufs.ComponentDetails{Metadata: []*protobufs.KeyValue{version}}),
		"exporters": group("debug", &protobufs.ComponentDetails{}),
	}}
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
