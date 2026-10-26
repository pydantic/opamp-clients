// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// reachability says when the Supervisor can't reach its OpAMP server, once a run of failures, with
// what to check in its own config; repeats go at debug, and getting through again is said once.
// The SDK integrations say the same with the core's hints, which name their environment
// variables; the Supervisor's settings are its config file's server section.
type reachability struct {
	logger   *zap.Logger
	endpoint string // the configured one, as it is safe to log
	ever     bool   // connected once
	failing  bool   // since the last connect
}

func newReachability(logger *zap.Logger, endpoint string) *reachability {
	return &reachability{logger: logger, endpoint: shownEndpoint(endpoint)}
}

// connected: the first connect is said by the Supervisor itself; getting through after failures
// is said here.
func (r *reachability) connected() {
	if r.failing {
		r.logger.Info("Connected to the OpAMP server again", zap.String("endpoint", r.endpoint))
	}
	r.ever, r.failing = true, false
}

// failed is the error to hand the Supervisor's OnConnectFailed for the first failure of a run,
// with what to check; nil for a repeat, which is logged here at debug.
func (r *reachability) failed(status int, detail string) error {
	if r.failing {
		r.logger.Debug("Still can't reach the OpAMP server", zap.String("endpoint", r.endpoint),
			zap.Int("http_status", status), zap.String("detail", detail))
		return nil
	}
	r.failing = true
	why := detail
	if why == "" {
		why = fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Errorf("can't reach %s (%s): %s. Retrying in the background; the collector keeps its current config meanwhile",
		r.endpoint, why, connectHint(status, r.ever, detail))
}

// connectHint is what to check, by the HTTP status the server answered with (0: none) and the
// error the connection failed with.
func connectHint(status int, ever bool, detail string) string {
	if status == 0 && strings.Contains(detail, "x509: ") {
		return "this host doesn't trust the server's certificate: enroll again with --ca-file naming the CA that " +
			"signed it (server.tls.ca_file), or add that CA to the system's trust store"
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the server refused the credentials: check the Authorization header in server.headers (or the auth extension " +
			"server.auth names) and that the key isn't revoked or expired"
	case http.StatusNotFound:
		return "nothing answers OpAMP at that path: check the path in server.endpoint (Logfire's is /v1/opamp)"
	case 0:
		if ever {
			return "the network or the server is down; nothing to change if it was reachable before"
		}
		return "check server.endpoint, and that this host can reach it (DNS, a proxy in HTTPS_PROXY, a firewall)"
	}
	return fmt.Sprintf("the server answered HTTP %d: check server.endpoint", status)
}

// shownEndpoint is the endpoint as it is safe to log (no user info, query or fragment), by the
// core's rule; "the configured endpoint" when the core can't say.
func shownEndpoint(endpoint string) string {
	s, err := opamp.StartSetup(context.Background(), &opamp.SetupStart{Code: &opamp.SetupInCode{Endpoint: &endpoint}})
	if err != nil || s.GetShownEndpoint() == "" {
		return "the configured endpoint"
	}
	return s.GetShownEndpoint()
}
