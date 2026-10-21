// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package logfirepolicyprocessor

import (
	"context"
	"errors"
)

// Config is the processor's: the policy, as the server sent it.
type Config struct {
	// Policy is a Logfire SDK policy document (logfire-sdk-policy/v1), the JSON a server sends in
	// the logfire-sdk-policy.json entry. Under the OpAMP Supervisor it is set from that entry.
	Policy string `mapstructure:"policy"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// Validate refuses a config without a policy, or with one the settings layer refuses (for a
// control this processor doesn't carry out, say).
func (c *Config) Validate() error {
	if c.Policy == "" {
		return errors.New("policy: a Logfire SDK policy is required")
	}
	_, err := Settings(context.Background(), []byte(c.Policy))
	return err
}
