// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"net/http"
	"testing"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// A Supervisor that can't reach its server says so once a run of failures, as an error naming what
// to check in its config; the retries go at debug, and getting through again is said once. Before,
// each retry was an error of its own, with the core's detail alone.
func TestConnectFailuresAreSaidOnceARunWithWhatToCheck(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var failed []error
	c := &coreClient{logger: zap.New(core), callbacks: types.Callbacks{
		OnConnectFailed: func(_ context.Context, err error) { failed = append(failed, err) },
	}}
	c.reach = newReachability(c.logger, "wss://user:secret@opamp.example.com/v1/opamp?token=x")

	for range 3 {
		c.onNotification(nil, opamp.Notification{Kind: opamp.ConnectFailed, Detail: "dial tcp: lookup opamp.example.com: no such host"})
	}
	require.Len(t, failed, 1, "one error for the run of failures")
	msg := failed[0].Error()
	assert.Contains(t, msg, "check server.endpoint")
	assert.Contains(t, msg, "no such host")
	assert.NotContains(t, msg, "secret", "no credentials in the log")
	assert.NotContains(t, msg, "token=x")
	assert.Equal(t, 2, logs.FilterMessage("Still can't reach the OpAMP server").Len(), "the repeats at debug")

	c.onNotification(nil, opamp.Notification{Kind: opamp.Connected})
	assert.Equal(t, 1, logs.FilterMessage("Connected to the OpAMP server again").Len())
	c.onNotification(nil, opamp.Notification{Kind: opamp.Connected})
	assert.Equal(t, 1, logs.FilterMessage("Connected to the OpAMP server again").Len(), "said once")

	// A new run of failures after a connect is said again; the server was reachable, so the hint
	// says so.
	c.onNotification(nil, opamp.Notification{Kind: opamp.ConnectFailed, Detail: "connection reset"})
	require.Len(t, failed, 2)
	assert.Contains(t, failed[1].Error(), "reachable before")
}

func TestConnectHintsNameTheSupervisorsSettings(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized:        "server.headers",
		http.StatusForbidden:           "server.auth",
		http.StatusNotFound:            "the path in server.endpoint",
		http.StatusInternalServerError: "the server answered HTTP 500",
		0:                              "HTTPS_PROXY",
	} {
		assert.Contains(t, connectHint(status, false), want, "HTTP %d", status)
	}
	r := newReachability(zap.NewNop(), "https://opamp.example.com/v1/opamp")
	assert.Contains(t, r.failed(http.StatusUnauthorized, "").Error(), "(HTTP 401)", "the status when there is no detail")
}
