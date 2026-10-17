// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Command driver runs the opamp-go client under the conformance driver contract
// (conformance/DRIVERS.md).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
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

func instanceUID() types.InstanceUid {
	var uid types.InstanceUid
	switch v := os.Getenv("DRIVER_UID"); v {
	case "", "random":
		rand.Read(uid[:])
		uid[6] = 0x70 | (uid[6] & 0x0f)
		uid[8] = 0x80 | (uid[8] & 0x3f)
	case "zero":
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
	return &protobufs.AgentDescription{
		IdentifyingAttributes:    []*protobufs.KeyValue{str("service.name", "conformance-go")},
		NonIdentifyingAttributes: []*protobufs.KeyValue{str("driver.change", strconv.Itoa(change))},
	}
}

// tlsConfig is the TLS the driver was asked for (DRIVERS.md): the CA to trust instead of the
// system's, and a client certificate to present. Nil when neither is set.
func tlsConfig() (*tls.Config, error) {
	ca, cert, key := os.Getenv("DRIVER_CA_FILE"), os.Getenv("DRIVER_CLIENT_CERT"), os.Getenv("DRIVER_CLIENT_KEY")
	if ca == "" && cert == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates", ca)
		}
	}
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

func main() {
	endpoint := os.Getenv("OPAMP_ENDPOINT")
	runFor := time.Duration(envFloat("DRIVER_RUN_S", 30) * float64(time.Second))
	poll := time.Duration(envFloat("DRIVER_POLL_S", 2) * float64(time.Second))

	var c client.OpAMPClient
	if strings.HasPrefix(endpoint, "ws") {
		c = client.NewWebSocket(nil)
	} else {
		h := client.NewHTTP(nil)
		h.SetPollingInterval(poll)
		c = h
	}
	caps := protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat

	uid := instanceUID()
	tlsCfg, err := tlsConfig()
	if err != nil {
		emit("fatal", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	settings := types.StartSettings{
		OpAMPServerURL:    endpoint,
		InstanceUid:       uid,
		Capabilities:      caps,
		HeartbeatInterval: &poll,
		TLSConfig:         tlsCfg,
		ProxyURL:          os.Getenv("DRIVER_PROXY"),
		Callbacks: types.Callbacks{
			OnConnect:       func(context.Context) { emit("connect", nil) },
			OnConnectFailed: func(_ context.Context, err error) { emit("connect_failed", map[string]any{"error": err.Error()}) },
			OnError: func(_ context.Context, e *protobufs.ServerErrorResponse) {
				emit("error_response", map[string]any{"type": e.Type.String(), "message": e.ErrorMessage})
			},
			OnMessage: func(ctx context.Context, msg *types.MessageData) {
				fields := map[string]any{}
				if msg.RemoteConfig != nil {
					fields["remote_config_hash"] = hex.EncodeToString(msg.RemoteConfig.ConfigHash)
					c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
						LastRemoteConfigHash: msg.RemoteConfig.ConfigHash,
						Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
					})
				}
				if msg.AgentIdentification != nil {
					fields["new_instance_uid"] = hex.EncodeToString(msg.AgentIdentification.NewInstanceUid)
				}
				emit("message", fields)
			},
			OnOpampConnectionSettings: func(_ context.Context, s *protobufs.OpAMPConnectionSettings) error {
				emit("connection_settings", map[string]any{"heartbeat_interval_seconds": s.GetHeartbeatIntervalSeconds()})
				return nil
			},
			GetEffectiveConfig: func(context.Context) (*protobufs.EffectiveConfig, error) {
				return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{}}, nil
			},
		},
	}
	if err := c.SetAgentDescription(description(0)); err != nil {
		emit("fatal", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	c.SetHealth(&protobufs.ComponentHealth{Healthy: true, StartTimeUnixNano: uint64(time.Now().UnixNano())})
	if err := c.Start(context.Background(), settings); err != nil {
		emit("fatal", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	emit("started", map[string]any{"uid": hex.EncodeToString(uid[:]), "client": "opamp-go"})

	// Status changes the scenario asks for, as seconds after start, one after another on one
	// goroutine: with a timer each, two that come due together (the process was frozen) would race,
	// and the application could end on an older change than its last.
	started := time.Now()
	go func() {
		for i, at := range strings.Split(os.Getenv("DRIVER_CHANGES_AT_S"), ",") {
			at, err := strconv.ParseFloat(strings.TrimSpace(at), 64)
			if err != nil {
				continue
			}
			n := i + 1
			time.Sleep(time.Until(started.Add(time.Duration(at * float64(time.Second)))))
			emit("status_change", map[string]any{"change": n})
			c.SetAgentDescription(description(n))
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
	case <-time.After(runFor):
	}
	emit("stopping", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.Stop(ctx)
	emit("stopped", map[string]any{"error": errString(err)})
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
