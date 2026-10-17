# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Runs the upstream Python OpAMP client (opentelemetry-opamp-client) under the conformance driver
contract (conformance/DRIVERS.md). HTTP only: the client has no WebSocket transport.

Not supported by the client, so not emitted: status changes (no API to change the agent
description after start) and choosing the instance UID.
"""
from __future__ import annotations

import json
import os
import signal
import sys
import threading
import time

from opentelemetry._opamp.agent import OpAMPAgent
from opentelemetry._opamp.callbacks import OpAMPCallbacks
from opentelemetry._opamp.client import OpAMPClient
from opentelemetry._opamp.proto import opamp_pb2

_lock = threading.Lock()


def emit(event: str, **fields) -> None:
    fields.update(event=event, wall=time.time())
    with _lock:
        print(json.dumps(fields, default=str), flush=True)


class Callbacks(OpAMPCallbacks):
    def on_connect(self, agent, client):
        emit("connect")

    def on_connect_failed(self, agent, client, error):
        emit("connect_failed", error=str(error))

    def on_error(self, agent, client, error_response):
        emit("error_response", type=opamp_pb2.ServerErrorResponseType.Name(error_response.type),
             message=error_response.error_message)

    def on_message(self, agent, client, message):
        fields = {}
        remote = getattr(message, "remote_config", None)
        if remote is not None:
            fields["remote_config_hash"] = remote.config_hash.hex()
            status = client.update_remote_config_status(
                remote_config_hash=remote.config_hash,
                status=opamp_pb2.RemoteConfigStatuses_APPLIED,
            )
            if status is not None:
                agent.send(payload=client.build_remote_config_status_response_message(status))
        emit("message", **fields)


def main() -> int:
    endpoint = os.environ["OPAMP_ENDPOINT"]
    run_s = float(os.environ.get("DRIVER_RUN_S", "30"))
    poll_s = float(os.environ.get("DRIVER_POLL_S", "2"))
    client = OpAMPClient(
        endpoint=endpoint,
        agent_identifying_attributes={"service.name": "conformance-python"},
        agent_non_identifying_attributes={"driver.change": "0"},
    )
    agent = OpAMPAgent(interval=poll_s, callbacks=Callbacks(), client=client)
    agent.start()
    emit("started", client="opentelemetry-opamp-client")

    done = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: done.set())
    done.wait(run_s)
    emit("stopping")
    agent.stop(timeout=5)
    emit("stopped")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # report, then fail the run
        emit("fatal", error=repr(exc))
        sys.exit(1)
