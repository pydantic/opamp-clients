// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"errors"
	"strings"

	"github.com/open-telemetry/opamp-go/protobufs"
)

type connectionUpdate struct{ hash []byte }

// connectionCredentials accepts only explicitly configured variable bindings. Errors contain no headers.
func connectionCredentials(bindings map[string]string, settings map[string]*protobufs.OtherConnectionSettings) (map[string]string, error) {
	env := make(map[string]string, len(settings))
	for name, connection := range settings {
		variable, allowed := bindings[name]
		if !allowed || variable == "" || strings.ContainsAny(variable, "=\x00") {
			return nil, errors.New("the offered connection has no valid environment binding")
		}
		token := ""
		for _, header := range connection.GetHeaders().GetHeaders() {
			if strings.EqualFold(header.GetKey(), "Authorization") {
				scheme, value, ok := strings.Cut(header.GetValue(), " ")
				if !ok || !strings.EqualFold(scheme, "Bearer") || token != "" || value == "" || strings.ContainsAny(value, " \t\r\n\x00") {
					return nil, errors.New("the offered connection requires one bearer credential")
				}
				token = value
			}
		}
		if token == "" {
			return nil, errors.New("the offered connection has no bearer credential")
		}
		env[variable] = token
	}
	return env, nil
}

func (s *Supervisor) receiveConnectionCredentials(hash []byte, settings map[string]*protobufs.OtherConnectionSettings) {
	env, err := connectionCredentials(s.config.Agent.ConnectionEnv, settings)
	if err != nil {
		s.reportConnectionSettings(hash, err)
		return
	}
	// The client's saved settings own persistence. Every subsequent child reads this immutable snapshot.
	s.connectionEnv.Store(&env)
	update := connectionUpdate{hash: append([]byte(nil), hash...)}
	select {
	case <-s.connectionUpdates:
	default:
	}
	select {
	case s.connectionUpdates <- update:
	case <-s.doneChan:
	}
}

func (s *Supervisor) reportConnectionSettings(hash []byte, err error) {
	status := &protobufs.ConnectionSettingsStatus{LastConnectionSettingsHash: hash, Status: protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED}
	if err != nil {
		status.Status = protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED
		status.ErrorMessage = "The Supervisor could not apply the connection credentials."
	}
	_ = s.opampClient.SetConnectionSettingsStatus(status)
}
