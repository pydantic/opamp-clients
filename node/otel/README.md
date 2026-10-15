# @pydantic/opamp-otel

OpenTelemetry auto-instrumentation for Node.js, under an OpAMP server: upstream's `register`, with
the SDK managed by the server.

## Quickstart

Build it in a checkout of this repository (until it is published) and install it into the
application, then start the application with it in place of upstream's `register`, with the
server's address and enrollment key:

```bash
OPAMP=/path/to/opamp   # this repository
(cd $OPAMP/node/opamp && npm ci && npm run build) && (cd $OPAMP/node/otel && npm ci && npm run build)
npm install $OPAMP/node/otel   # in the application's directory
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_LOG_LEVEL=info \
node --import @pydantic/opamp-otel/register app.js
```

When it works, it logs, at INFO:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

INFO lines show only with `OTEL_LOG_LEVEL=info`. When it can't connect, it warns once, with what to
check; warnings go to stderr without it.

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

The application can read settings of its own the server sends (a feature flag, a limit), with a
default for when there are none ([app-settings.md](../../docs/app-settings.md)):

```js
import { appSettings } from "@pydantic/opamp-otel";

if (appSettings.get("checkout.new_flow", "off") === "on") { /* ... */ }
const unsubscribe = appSettings.onChange((settings, changed) => console.log("changed:", changed));
```

Everything else is in [docs/node-otel.md](../../docs/node-otel.md).
