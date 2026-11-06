# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A parent that forks workers, as gunicorn does: run under ``opentelemetry-instrument``, it forks
two workers once the SDK is built and makes no request itself. Each worker requests the URL it
is given every 200 ms, which its urllib instrumentation traces. The parent is the one instance
the server hears; the workers follow the settings it is given (docs/python-otel.md, "Takes forked
workers along"). On SIGINT the parent stops the workers, waits for them, and exits."""

import os
import signal
import sys
import time
import urllib.request

WORKERS = 2


def work(url: str) -> None:
    signal.signal(signal.SIGINT, signal.SIG_DFL)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))  # sys.exit: the SDK flushes at exit
    while True:
        try:
            urllib.request.urlopen(url, timeout=2).read()
        except OSError:
            pass  # 404s and a server gone at the end of a run
        time.sleep(0.2)


children = []
for _ in range(WORKERS):
    pid = os.fork()
    if pid == 0:
        work(sys.argv[1])
    children.append(pid)
try:
    while True:
        time.sleep(1)
except KeyboardInterrupt:
    for pid in children:
        os.kill(pid, signal.SIGTERM)
    for pid in children:
        os.waitpid(pid, 0)
