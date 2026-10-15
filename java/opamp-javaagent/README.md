# opamp-javaagent

An extension for the upstream OpenTelemetry Java agent that puts the agent under an OpAMP server.

## Quickstart

Build the extension jar from the repository root (until it is published), then start the
application with the agent and the extension, with the server's address and enrollment key:

```bash
mvn -q -B -DskipTests install -f java/opamp/pom.xml
mvn -q -B -DskipTests package -f java/opamp-javaagent/pom.xml
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
java -javaagent:opentelemetry-javaagent.jar \
     -Dotel.javaagent.extensions=java/opamp-javaagent/target/opamp-javaagent-extension-0.1.0-SNAPSHOT.jar \
     -jar checkout.jar
```

When it works, the logger `dev.pydantic.opamp.javaagent.Extension` says, at INFO (shown by
default):

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check. The instance keeps its UID across
restarts with no configuration ([default-state-dir.md](../../docs/default-state-dir.md)).

The server can also send the application settings of its own (a feature flag, a limit). The
application reads them with `dev.pydantic.opamp.AppSettings`, from the second jar the build makes,
`target/opamp-javaagent-extension-0.1.0-SNAPSHOT-app-settings.jar`, which it compiles against and
runs with; without the agent every read is its default:

```java
if (AppSettings.get("checkout.new_flow", "off").equals("on")) { ... }
AutoCloseable subscription = AppSettings.onChange((settings, changed) -> queue.add(changed));
```

Everything else is in [docs/javaagent.md](../../docs/javaagent.md), this in
[its own section](../../docs/javaagent.md#the-applications-own-settings).
