# pydantic-opamp-otel

OpenTelemetry auto-instrumentation for Python, under an OpAMP server.

## Quickstart

From the repository root (until it is published), install it next to the instrumentations the
application needs, then run the application under `opentelemetry-instrument` with the server's
address and enrollment key:

```bash
pip install ./python ./python/otel opentelemetry-instrumentation-flask
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
opentelemetry-instrument python app.py
```

When it works, the `opamp_otel` logger says, at INFO:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

INFO shows where the application's logging shows it (`logging.basicConfig(level=logging.INFO)`,
say). When it can't connect, it warns once, with what to check; warnings show without any setup.
The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

Everything else, including setting the SDK up in code with `opamp_otel.configure()`, is in
[docs/python-otel.md](../../docs/python-otel.md).
