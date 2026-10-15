# opampotel

`github.com/pydantic/opamp-clients/go/otel`: the OpenTelemetry SDK for Go, set up from the `OTEL_*`
variables and managed by an OpAMP server.

## Quickstart

Until it is published, point the application's module at a checkout of the repository:

```bash
go mod edit -require=github.com/pydantic/opamp-clients/go/otel@v0.0.0 \
    -replace=github.com/pydantic/opamp-clients/go/otel=../opamp/go/otel \
    -replace=github.com/pydantic/opamp-clients/go=../opamp/go
```

Set up the SDK with `Setup` in `main`:

```go
import "github.com/pydantic/opamp-clients/go/otel"

func main() {
	sdk, err := opampotel.Setup(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(context.Background()) // goodbye to the server, then flush
	// ...
}
```

Run it with the server's address and enrollment key:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
./checkout
```

Or give them in code: `opampotel.Setup(ctx, opampotel.WithEndpoint(...), opampotel.WithHeaders(...))`.

When it works, `slog`'s default logger says, at INFO (shown by default):

```
opamp: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check.

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

The application reads settings of its own the server sends (a feature flag, say) with
`opampotel.AppSetting("checkout.new_flow", "off")`, and is told of changes with
`opampotel.OnAppSettingsChange` ([app-settings.md](../../docs/app-settings.md)).

Everything else is in [docs/go-otel.md](../../docs/go-otel.md).
