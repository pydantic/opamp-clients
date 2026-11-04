# pydantic-opamp-otel

OpenTelemetry auto-instrumentation for Python, under an OpAMP server.

## Quickstart

From the repository root (until it is published), fetch the core, install it with the client and
this package next to the instrumentations the application needs, then run the application under
`opentelemetry-instrument` with the server's address and connection key:

```bash
scripts/fetch_core.py fetch               # the core release the lock pins, verified
pip install ./python/core ./python ./python/otel opentelemetry-instrumentation-flask
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<key>" \
opentelemetry-instrument python app.py
```

When it works, the `pydantic_opamp_otel` logger says so once, shown with no logging setup:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check.
The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

An application on the Logfire SDK calls `pydantic_opamp_otel.configure_logfire()` in place of
`logfire.configure()` (install `'./python/otel[logfire]'`). One that sets its OpenTelemetry SDK up
in code already keeps that setup and calls `pydantic_opamp_otel.manage()` after it.

Everything else, including setting the SDK up in code with `pydantic_opamp_otel.configure()`, is in
[docs/python-otel.md](../../docs/python-otel.md).
