// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"context"

	"github.com/pydantic/opamp-clients/go/opamp/internal/core"
	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// The core's Setup call: an OpenTelemetry integration's start-up decisions (where the server is,
// the headers, the heartbeat interval, the state directory, the instance UID, what is safe to log),
// made by the core, the same in every language. The integration keeps only the I/O: reading the
// variables, making and locking a state directory's slot, reading and writing its instance-uid
// file, and logging what it is told to. It needs no client and keeps no state: the same call
// always gives the same answer.
type (
	SetupStart      = corepb.SetupStart
	SetupInCode     = corepb.SetupInCode
	SetupStarted    = corepb.SetupStarted
	SetupHints      = corepb.SetupHints
	SetupIdentity   = corepb.SetupIdentity
	SetupIdentified = corepb.SetupIdentified
	SetupOs         = corepb.SetupOs
	SetupUidFrom    = corepb.SetupUidFrom
	Variable        = corepb.Variable
	Header          = corepb.Header
)

// The operating systems, for where the user's state directory is.
const (
	SetupOsUnix    = corepb.SetupOs_SETUP_OS_UNIX
	SetupOsMacOS   = corepb.SetupOs_SETUP_OS_MACOS
	SetupOsWindows = corepb.SetupOs_SETUP_OS_WINDOWS
)

// Where an instance UID came from.
const (
	SetupUidMade  = corepb.SetupUidFrom_SETUP_UID_FROM_MADE
	SetupUidSaved = corepb.SetupUidFrom_SETUP_UID_FROM_SAVED
	SetupUidGiven = corepb.SetupUidFrom_SETUP_UID_FROM_GIVEN
)

// SetupLockFile and SetupUIDFile are the files in a state directory's slot: the lock a process
// holds while it runs, and the instance UID.
const (
	SetupLockFile = "lock"
	SetupUIDFile  = "instance-uid"
)

// CoreUnavailable is why the client core can't be loaded here (its module doesn't compile or
// doesn't instantiate), or nil when it can. An integration asks before StartSetup, and runs
// unmanaged when the core can't be loaded. (The SDK settings layer's: sdksettings.Unavailable.)
func CoreUnavailable(ctx context.Context) error {
	return core.Unavailable(ctx)
}

// StartSetup is where the server is and how to reach it, from the environment and what the code
// gives. Off (no endpoint): Started.Off. An error is a failure of the core itself.
func StartSetup(ctx context.Context, start *SetupStart) (*SetupStarted, error) {
	reply, err := core.Setup(ctx, &corepb.SetupCall{Call: &corepb.SetupCall_Start{Start: start}})
	if err != nil {
		return nil, err
	}
	return reply.GetStarted(), nil
}

// IdentifySetup is the instance UID, once the state directory is known (and its slot locked).
// An error is a failure of the core, or a call it can't use (less entropy than it needs).
func IdentifySetup(ctx context.Context, identity *SetupIdentity) (*SetupIdentified, error) {
	reply, err := core.Setup(ctx, &corepb.SetupCall{Call: &corepb.SetupCall_Identity{Identity: identity}})
	if err != nil {
		return nil, err
	}
	return reply.GetIdentified(), nil
}
