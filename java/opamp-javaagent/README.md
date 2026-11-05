# opamp-javaagent

An extension for the upstream OpenTelemetry Java agent that puts the agent under an OpAMP server.

## Quickstart

Download the extension jar from Maven Central (`dev.pydantic:logfire-fleet-javaagent-extension`) next to
the upstream agent, then start the application with both, with the server's address and key:

```bash
V=0.2.0   # the latest: https://central.sonatype.com/artifact/dev.pydantic/logfire-fleet-javaagent-extension
curl -sSfLO https://repo1.maven.org/maven2/dev/pydantic/logfire-fleet-javaagent-extension/$V/logfire-fleet-javaagent-extension-$V.jar
curl -sSfLo opentelemetry-javaagent.jar https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/2.31.1/opentelemetry-javaagent-2.31.1.jar
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<key>" \
java -javaagent:opentelemetry-javaagent.jar \
     -Dotel.javaagent.extensions=logfire-fleet-javaagent-extension-$V.jar \
     -jar checkout.jar
```

In Logfire, **Fleet → Connect an application** gives the endpoint and the key.

When it works, the logger `dev.pydantic.logfire.fleet.javaagent.Extension` says, at INFO (shown by
default):

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check. The instance keeps its UID across
restarts with no configuration ([default-state-dir.md](https://github.com/pydantic/opamp-clients/blob/main/docs/default-state-dir.md)).

The server can also send the application settings of its own (a feature flag, a limit). The
application reads them with `dev.pydantic.logfire.fleet.AppSettings`, from the extension's second jar,
`dev.pydantic:logfire-fleet-javaagent-extension` with the classifier `app-settings`, which it compiles
against and runs with; without the agent every read is its default:

```java
if (AppSettings.get("checkout.new_flow", "off").equals("on")) { ... }
AutoCloseable subscription = AppSettings.onChange((settings, changed) -> queue.add(changed));
```

Everything else is in [docs/javaagent.md](../../docs/javaagent.md), this in
[its own section](../../docs/javaagent.md#the-applications-own-settings).
