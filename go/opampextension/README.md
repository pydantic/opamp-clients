# OpAMP extension for the OpenTelemetry Collector, on the shared core

The Collector's `opamp` extension with the client underneath replaced: upstream's
[`opampextension`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/extension/opampextension)
runs on opamp-go; this one runs on [`go/opamp`](../opamp), the shared core in Go mode (net/http
and gorilla/websocket carry the requests, so the Collector's TLS settings, the environment's
proxies and HTTP auth extensions apply as they do to its other clients). The component type, the
config and the custom-message API (`opampcustommessages`) are upstream's: a collector built with
this extension in place of upstream's runs the same config.

## Building a collector with it

In an [ocb](https://opentelemetry.io/docs/collector/custom-collector/) manifest, in place of
upstream's extension (a collector can have only one `opamp` extension):

```yaml
extensions:
  - gomod: github.com/pydantic/opamp-clients/go/opampextension v0.0.0
replaces:
  - github.com/pydantic/opamp-clients/go => /path/to/opamp/go
  - github.com/pydantic/opamp-clients/go/opampextension => /path/to/opamp/go/opampextension
```

## What it adds

Everything in upstream's README applies, plus:

- `state_file`: where the client keeps its state across restarts (the instance UID it made up,
  and connection settings the server issued). It holds those credentials and is written for the
  collector's user only (0600). Keep it on a local disk only that user can read.
- `state_key` (needs `state_file`): encrypts the state file at rest with a key from the OS
  (`systemd-creds`, `keychain`, `dpapi`, `file:PATH` or `auto`;
  [docs/state-file.md](../../docs/state-file.md#encrypted-at-rest-go)). A key it can't find stops
  the extension at the start.
- `capabilities.accepts_opamp_connection_settings` (default `false`, needs `state_file`): the
  server may issue new OpAMP connection settings, an endpoint and headers: per-agent credentials
  replacing an enrollment token, say. The core tries them, keeps them only once the server has
  accepted a connection with them, goes back to the old ones otherwise, and keeps them in the state
  file, so a restarted collector connects with the issued credentials, not the ones in its config
  ([docs/connection-settings.md](../../docs/connection-settings.md)).

```yaml
extensions:
  opamp:
    server:
      ws:
        endpoint: wss://opamp.example.com/v1/opamp
        headers:
          Authorization: "Bearer ${env:OPAMP_ENROLLMENT_TOKEN}"
    state_file: /var/lib/otelcol/opamp.state
    state_key: auto
    capabilities:
      accepts_opamp_connection_settings: true
```

Without a configured `instance_uid` the client makes up a UUIDv7, and with `state_file` keeps it
across restarts (upstream makes up a new one at every start).

## Differences from upstream

- An `auth` extension works over `http` only: it wraps the client's transport, so it runs on every
  request (a refreshed token included). Over `ws` it is refused: the client has no hook to ask it
  for headers at each upgrade. It can't be combined with `accepts_opamp_connection_settings`
  either, since it would replace the headers the server issued.
- A custom message is handed to the client, which queues it; `SendMessage`'s channel is closed at
  once, and a message the client can't send after all is logged. Before the extension has started,
  `SendMessage` returns an error.
- The available components' hash is the core's.

## License

Derived from opentelemetry-collector-contrib's `extension/opampextension`, under the Apache
License, Version 2.0 ([LICENSE](LICENSE)); each derived file says what changed. The files without
that header are under the repository's license.
