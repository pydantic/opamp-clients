# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""app.py's application, with its SDK set up in code: opamp_otel.configure() in place of
``opentelemetry-instrument``, and the OpAMP server given there, from the application's own
settings (here its own variables) rather than the OTEL_OPAMP_* ones.

    CHECKOUT_OPAMP_URL=https://opamp.example.com/v1/opamp CHECKOUT_OPAMP_KEY=<enrollment key> \\
    CHECKOUT_STATE_DIR=/var/lib/checkout/opamp python in_code.py http://localhost:8080/

The exporters are still the OTEL_* variables' (OTLP by default). The application instruments
itself, so the server's instrumentation switches are answered but not carried out.
"""

import os
import sys
import time
import urllib.request

import opamp_otel
from opentelemetry.instrumentation.urllib import URLLibInstrumentor

opamp_otel.configure(
    endpoint=os.environ["CHECKOUT_OPAMP_URL"],
    headers={"Authorization": f"Bearer {os.environ['CHECKOUT_OPAMP_KEY']}"},  # sent as given, not %-decoded
    state_dir=os.environ.get("CHECKOUT_STATE_DIR"),  # unset: a directory of the user's own
    heartbeat_interval=os.environ.get("CHECKOUT_OPAMP_HEARTBEAT", "30s"),  # or seconds, or a timedelta
    resource_attributes={"service.name": "checkout"},  # the rest are upstream's configurator's
)
URLLibInstrumentor().instrument()  # after configure(), as any instrumentor

try:
    while True:
        try:
            urllib.request.urlopen(sys.argv[1], timeout=2).read()
        except OSError:
            pass  # 404s and a server gone at the end of a run
        time.sleep(0.2)
except KeyboardInterrupt:
    pass  # a clean exit: the goodbye, then the flush
