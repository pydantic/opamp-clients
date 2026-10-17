// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp_test

import (
	"context"
	"testing"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// The core's Setup call, through the module: off without an endpoint; the endpoint, the headers
// and the heartbeat as it reads them; the instance UID saved, or made and to be saved.
func TestSetupIsTheCoresCall(t *testing.T) {
	ctx := context.Background()
	off, err := opamp.StartSetup(ctx, &opamp.SetupStart{})
	if err != nil || !off.GetOff() {
		t.Fatalf("no endpoint: off; got %v, %v", off, err)
	}
	s, err := opamp.StartSetup(ctx, &opamp.SetupStart{Env: []*opamp.Variable{
		{Name: "OTEL_OPAMP_ENDPOINT", Value: "https://user:pw@opamp.example.com/v1/opamp?k=v"},
		{Name: "OTEL_OPAMP_HEADERS", Value: "Authorization=Bearer%20k"},
		{Name: "OTEL_OPAMP_HEARTBEAT_INTERVAL", Value: "30s"},
		{Name: "OTEL_OPAMP_STATE_DIR", Value: "none"},
	}})
	if err != nil || s.GetOff() || s.GetShownEndpoint() != "https://opamp.example.com/v1/opamp" ||
		len(s.GetHeaders()) != 1 || s.GetHeaders()[0].GetValue() != "Bearer k" || s.GetHeartbeatIntervalMs() != 30000 ||
		s.GetStateDir() != "" || len(s.GetDefaultSlots()) != 0 {
		t.Fatalf("%v, %v", s, err)
	}
	const saved = "0199e3a0-0000-7000-8000-000000000007"
	id, err := opamp.IdentifySetup(ctx, &opamp.SetupIdentity{StateDir: true, Saved: saved + "\n", Entropy: make([]byte, 16)})
	if err != nil || id.GetFrom() != opamp.SetupUidSaved || id.GetSave() || id.GetText() != saved {
		t.Fatalf("%v, %v", id, err)
	}
	made, err := opamp.IdentifySetup(ctx, &opamp.SetupIdentity{StateDir: true, Entropy: make([]byte, 16), UnixMs: 1})
	if err != nil || made.GetFrom() != opamp.SetupUidMade || !made.GetSave() || len(made.GetInstanceUid()) != 16 {
		t.Fatalf("%v, %v", made, err)
	}
	if _, err := opamp.IdentifySetup(ctx, &opamp.SetupIdentity{}); err == nil {
		t.Fatal("no entropy: the core refuses the call")
	}
}
