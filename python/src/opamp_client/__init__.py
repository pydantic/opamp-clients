# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""An OpAMP client for Python (asyncio).

Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After, size
limits) is the shared OpAMP client core, run natively (opamp_client._native). By default the
core speaks HTTP and WebSocket itself too (Rust mode), over asyncio's TCP connections and TLS
(``_streams.py``); with ``Config(mode="python")`` this package carries out the core's requests
with aiohttp instead. Either way it feeds back what happened, so it behaves exactly like the
other languages' clients on the same core.
"""

from ._client import Client, Config, Notification, Status
from ._core import CoreError
from ._executor import save_state
from ._transport import TLS, Proxy
from ._proto.opamp.v1 import anyvalue_pb2, opamp_pb2

__all__ = ["TLS", "Client", "Config", "CoreError", "Notification", "Proxy", "Status", "anyvalue_pb2", "opamp_pb2",
           "save_state"]
