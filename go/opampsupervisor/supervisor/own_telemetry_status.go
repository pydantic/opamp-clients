// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"errors"
	"regexp"
	"sync"

	"go.uber.org/zap"
)

// ownTelemetryRefusals logs a refusal of the server's own telemetry settings at error once for
// each reason, and at debug when a reason it has logged comes again: a server can offer the
// same settings again at each connection, and the refusal is in the status
// (Status.OwnTelemetry). Settings applied start it afresh.
type ownTelemetryRefusals struct {
	mu   sync.Mutex
	seen map[string]bool
}

// maxOwnTelemetryReasons bounds the reasons kept: past it, they are forgotten and start again.
const maxOwnTelemetryReasons = 16

func (r *ownTelemetryRefusals) log(logger *zap.Logger, err error) {
	why := shownURLs(err.Error())
	r.mu.Lock()
	again := r.seen[why]
	if !again {
		if r.seen == nil || len(r.seen) >= maxOwnTelemetryReasons {
			r.seen = map[string]bool{}
		}
		r.seen[why] = true
	}
	r.mu.Unlock()
	if again {
		logger.Debug("Refusing the own telemetry settings again, for a reason already logged", zap.String("error", why))
		return
	}
	logger.Error("Refusing the own telemetry settings", zap.String("error", why))
}

func (r *ownTelemetryRefusals) applied() {
	r.mu.Lock()
	r.seen = nil
	r.mu.Unlock()
}

// quotedURL is a URL in an error, quoted as %q quotes it.
var quotedURL = regexp.MustCompile(`"[a-zA-Z][a-zA-Z0-9+.-]*://[^"]*"`)

// shownURLs is s with each quoted URL in it as it is safe to show (shownEndpoint: no user info,
// query or fragment). The policy's errors quote the endpoints they refuse, as the server gave
// them.
func shownURLs(s string) string {
	return quotedURL.ReplaceAllStringFunc(s, func(q string) string { return `"` + shownEndpoint(q[1:len(q)-1]) + `"` })
}

// ownTelemetryOutcome is the status's word for err: "REFUSED" by the remote config policy,
// "FAILED" for anything else (the settings couldn't be put in the collector's config), "APPLIED"
// without an error (in the collector's config; whether the collector runs it is its health).
func ownTelemetryOutcome(err error) string {
	var policy ownTelemetryPolicyError
	switch {
	case err == nil:
		return "APPLIED"
	case errors.As(err, &policy):
		return "REFUSED"
	}
	return "FAILED"
}

// refuseOwnTelemetry logs why the server's own telemetry settings are refused.
func (s *Supervisor) refuseOwnTelemetry(err error) {
	s.ownTelemetryRefusals.log(s.telemetrySettings.Logger, err)
}

// noteOwnTelemetry puts the outcome of the server's own telemetry settings in the status: applied,
// or refused and why (err), so `opampsupervisor status` says it.
func (s *Supervisor) noteOwnTelemetry(err error) {
	if err == nil {
		s.ownTelemetryRefusals.applied()
	}
	if c, ok := s.opampClient.(*coreClient); ok {
		c.report.ownTelemetry(s.telemetrySettings.Logger, err)
	}
}

// ownTelemetryPolicyError is the remote config policy's refusal of the own telemetry settings.
// The server is told so in words of the Supervisor's own (errOwnTelemetryPolicy): the policy's
// error can quote the settings, and they hold a credential.
type ownTelemetryPolicyError struct{ err error }

func (e ownTelemetryPolicyError) Error() string { return e.err.Error() }
func (e ownTelemetryPolicyError) Unwrap() error { return e.err }

// errOwnTelemetryPolicy is what the server is told of such a refusal.
const errOwnTelemetryPolicy connectionSettingsError = "The host's remote config policy (agent::remote_config_policy) doesn't allow the own telemetry settings, most often their endpoint: `opampsupervisor status` on the host says why."
