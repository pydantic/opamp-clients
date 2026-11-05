# @pydantic/opamp-otel

OpenTelemetry auto-instrumentation for Node.js, under an OpAMP server: upstream's `register`, with
the SDK managed by the server.

## Quickstart

Install it into the application, then start the application with it in place of upstream's
`register`, with the server's address and key:

```bash
npm install @pydantic/opamp-otel
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<key>" \
node --import @pydantic/opamp-otel/register app.js
```

It brings upstream's auto-instrumentations and OTLP exporters with it. In Logfire, **Fleet →
Connect an application** gives the endpoint and the key. When it works, it says so once, on
stderr, with no logging setup:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check.

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](https://github.com/pydantic/opamp-clients/blob/main/docs/default-state-dir.md)).

The application can read settings of its own the server sends (a feature flag, a limit), with a
default for when there are none ([app-settings.md](https://github.com/pydantic/opamp-clients/blob/main/docs/app-settings.md)):

```js
import { appSettings } from "@pydantic/opamp-otel";

if (appSettings.get("checkout.new_flow", "off") === "on") { /* ... */ }
const unsubscribe = appSettings.onChange((settings, changed) => console.log("changed:", changed));
```

Everything else is in [docs/node-otel.md](https://github.com/pydantic/opamp-clients/blob/main/docs/node-otel.md).
