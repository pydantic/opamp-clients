# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""An application with no OpenTelemetry code at all: an HTTP request every 200 ms, and a log line
for each. Run under ``opentelemetry-instrument``, its urllib instrumentation traces the requests
(and measures them), the logging handler exports the lines; pydantic-opamp-otel puts the SDK
under the OpAMP server.

With APP_HEALTH_CHECKS set (try.py's next-settings run), each round also requests /health, as a
probe would, and logs a warning on a second logger, ``app.noisy``: what sampling rules and log
levels per scope pick out.

Its one line of OpAMP code reads a setting of its own the server sends (docs/app-settings.md): a
feature flag, ``example.feature``. While the server says ``on``, each round also requests
/feature; and each time its settings change it requests /changed, once. Without a server, or
until the server says, the flag is its default, ``off``."""

import logging
import os
import queue
import sys
import time
import urllib.request

from opamp_otel import app_settings

if os.environ.get("EXAMPLE_SETUP"):  # check_truth's setups (setups.py): none otherwise
    import setups

    setups.apply(os.environ["EXAMPLE_SETUP"])

log = logging.getLogger("app")
noisy = logging.getLogger("app.noisy")
logging.getLogger().setLevel(logging.INFO)
base = sys.argv[1].rsplit("/", 1)[0]
health = os.environ.get("APP_HEALTH_CHECKS") and base + "/health"
# Told on the OpAMP client's thread: keep it quick, and do the work in the application's own loop.
changes: queue.SimpleQueue = queue.SimpleQueue()
app_settings.on_change(lambda settings, changed: changes.put(sorted(changed)))


def request(url: str) -> None:
    try:
        urllib.request.urlopen(url, timeout=2).read()
    except OSError:
        pass  # 404s and a server gone at the end of a run


try:
    while True:
        request(sys.argv[1])
        log.info("requested %s", sys.argv[1])
        if app_settings.get("example.feature", "off") == "on":
            request(base + "/feature")
        while not changes.empty():
            log.info("settings changed: %s", ", ".join(changes.get()))
            request(base + "/changed")
        if health:
            request(health)
            noisy.warning("still here")
        time.sleep(0.2)
except KeyboardInterrupt:
    pass  # a clean exit: the atexit hooks say goodbye, then flush
