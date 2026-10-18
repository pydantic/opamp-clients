# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The core's Setup call: an OpenTelemetry integration's start-up decisions (where the server is,
the headers, the heartbeat interval, the state directory, the instance UID, what is safe to log),
made by the core, the same in every language. The integration keeps only the I/O: reading the
variables, making and locking a state directory's slot, reading and writing its instance-uid
file, and logging what it is told to. It needs no client and keeps no state: the same call always
gives the same answer."""

from __future__ import annotations

from . import _native
from ._core import CoreError
from ._proto.opamp.core.v1 import core_pb2
from ._proto.opamp.core.v1.core_pb2 import (Header, SetupHints, SetupIdentified, SetupIdentity, SetupInCode,
                                            SetupStart, SetupStarted, Variable)

__all__ = ["LOCK_FILE", "UID_FILE", "Header", "SetupHints", "SetupIdentified", "SetupIdentity", "SetupInCode",
           "SetupStart", "SetupStarted", "Variable", "identify", "start", "unavailable"]

#: The files in a state directory's slot: the lock a process holds while it runs, and the UID.
LOCK_FILE = "lock"
UID_FILE = "instance-uid"

OS_UNIX = core_pb2.SETUP_OS_UNIX
OS_MACOS = core_pb2.SETUP_OS_MACOS
OS_WINDOWS = core_pb2.SETUP_OS_WINDOWS
UID_MADE = core_pb2.SETUP_UID_FROM_MADE
UID_SAVED = core_pb2.SETUP_UID_FROM_SAVED
UID_GIVEN = core_pb2.SETUP_UID_FROM_GIVEN


def unavailable() -> str | None:
    """Why the core can't be loaded here (no build for this platform, a file that isn't one, a
    mismatched interface), or None when it can. An integration asks before it calls start, and
    runs unmanaged when it can't: the calls below raise ImportError then."""
    return _native.unavailable()


def _call(call: core_pb2.SetupCall) -> core_pb2.SetupReply:
    try:
        raw = _native.setup_call(call.SerializeToString())
    except _native.Trap as e:
        raise CoreError(f"trap: {e}") from e
    except AttributeError as e:  # a library from before Setup
        raise CoreError("the OpAMP core library has no Setup: install the pydantic-opamp-core this package asks for") from e
    reply = core_pb2.SetupReply.FromString(raw)
    if reply.WhichOneof("result") == "error":
        raise CoreError(reply.error)
    return reply


def start(start: SetupStart) -> SetupStarted:
    """Where the server is and how to reach it, from the environment and what the code gives.
    ``off`` without an endpoint. Raises CoreError on a failure of the core."""
    return _call(core_pb2.SetupCall(start=start)).started


def identify(identity: SetupIdentity) -> SetupIdentified:
    """The instance UID, once the state directory is known (and its slot locked). Raises CoreError
    on a failure of the core, or a call it can't use."""
    return _call(core_pb2.SetupCall(identity=identity)).identified
