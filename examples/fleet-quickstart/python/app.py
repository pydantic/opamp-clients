# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A shop that takes an order a second, for trying Logfire Fleet (see ../README.md).

Each order is a span with an INFO log inside it; every fifth order is slow and logs a WARNING
as well. Set the policy's minimum log level to Warning in Fleet and, in Live view, the INFO lines
stop while the warnings and spans keep coming."""

import itertools
import time

import logfire

import logfire_fleet_otel

# In place of `logfire.configure()`. The console is off so the terminal shows only the app's own
# `order N sent` lines; Live view is where the telemetry goes.
logfire_fleet_otel.configure_logfire(service_name="fleet-quickstart", console=False)

for order in itertools.count(1):
    with logfire.span("checkout order {order}", order=order):
        logfire.info("order {order} paid", order=order)
        if order % 5 == 0:
            logfire.warn("order {order} took longer than 2 s", order=order)
    print(f"order {order} sent", flush=True)
    time.sleep(1)
