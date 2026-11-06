# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""app.py's application on the Logfire SDK: set up with logfire_fleet_otel.configure_logfire() in
place of ``logfire.configure()``, with the same keyword arguments, the server from the
OTEL_OPAMP_* variables. Its requests are traced by OpenTelemetry's urllib instrumentation, which
the Logfire SDK's tracer provider takes; its spans go where the Logfire SDK sends them
(LOGFIRE_TOKEN, and here its base URL, the sink).

    OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp OTEL_OPAMP_HEADERS="Authorization=Bearer <key>" \\
    LOGFIRE_TOKEN=<write token> python logfire_app.py http://localhost:8080/

With EXAMPLE_SETUP=manage the application keeps ``logfire.configure()`` as it is and calls
logfire_fleet_otel.manage() after it (docs/python-otel.md, "Already using OpenTelemetry or Logfire?").
"""

import os
import sys
import time
import urllib.request

import logfire
from opentelemetry.instrumentation.urllib import URLLibInstrumentor

import logfire_fleet_otel

if os.environ.get("EXAMPLE_SETUP") == "manage":
    logfire.configure(service_name="checkout", console=False, metrics=False)
    logfire_fleet_otel.manage()
else:
    logfire_fleet_otel.configure_logfire(service_name="checkout", console=False, metrics=False)
URLLibInstrumentor().instrument()

try:
    while True:
        try:
            urllib.request.urlopen(sys.argv[1], timeout=2).read()
        except OSError:
            pass  # 404s and a server gone at the end of a run
        time.sleep(0.2)
except KeyboardInterrupt:
    pass  # a clean exit: the goodbye, then the flush
