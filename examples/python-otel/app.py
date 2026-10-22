# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
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
until the server says, the flag is its default, ``off``.

With EXAMPLE_PROBE=1 (integrations.py's run 3) each round also probes redaction: a span of its own,
``example.probe``, with the URL it requests (url.full) on an event and on a link to the round
before's probe, and a log record ``example.probe`` with that URL; each with a note
(example.note) that says password, for scrubbing to replace. It probes before or after the
request, at random: under a rate limit of one a second, the first record after the token comes
takes it, and in a fixed order the probe might never be that one."""

import logging
import os
import queue
import random
import sys
import time
import urllib.request

from pydantic_opamp_otel import app_settings

if os.environ.get("EXAMPLE_SETUP"):  # check_truth's setups (setups.py): none otherwise
    import setups

    setups.apply(os.environ["EXAMPLE_SETUP"])

log = logging.getLogger("app")
probing = os.environ.get("EXAMPLE_PROBE") == "1"
if probing:
    from opentelemetry import trace

    tracer = trace.get_tracer("app")
last = None  # the round before's probe, which the next links to
noisy = logging.getLogger("app.noisy")
logging.getLogger().setLevel(logging.INFO)
base = sys.argv[1].rsplit("/", 1)[0]
health = os.environ.get("APP_HEALTH_CHECKS") and base + "/health"
# Told on the OpAMP client's thread: keep it quick, and do the work in the application's own loop.
changes: queue.SimpleQueue = queue.SimpleQueue()
app_settings.on_change(lambda settings, changed: changes.put(sorted(changed)))


#: What the probe carries beside its URL (example.note): it says password, so scrubbing's default
#: patterns replace it.
NOTE = "the password is p4ss"


def probe(url: str) -> None:
    """A span of its own with `url` on an event and on a link to the last probe, and a log record
    with `url`: what redaction may drop or mask. Each carries NOTE too, which scrubbing replaces."""
    global last
    attributes = {"url.full": url, "example.note": NOTE}
    links = [trace.Link(last, attributes)] if last else []
    with tracer.start_as_current_span("example.probe", links=links) as span:
        span.add_event("example.probe", attributes)
        last = span.get_span_context()
    log.info("example.probe", extra=attributes)


def request(url: str) -> None:
    try:
        urllib.request.urlopen(url, timeout=2).read()
    except OSError:
        pass  # 404s and a server gone at the end of a run


try:
    while True:
        early = random.random() < 0.5
        if probing and early:
            probe(sys.argv[1])
        request(sys.argv[1])
        log.info("requested %s", sys.argv[1])
        if probing and not early:
            probe(sys.argv[1])
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
