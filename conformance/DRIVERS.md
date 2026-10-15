# Driver contract

A driver is a small program that runs one OpAMP client the way an application would, and
reports what the client told it. The runner starts it once per scenario and transport.

## Input: environment variables

| Variable | Meaning |
|---|---|
| `OPAMP_ENDPOINT` | Full server URL. `http://…/v1/opamp` for plain HTTP, `ws://…/v1/opamp` for WebSocket; `https://` and `wss://` when the server listens with TLS. |
| `DRIVER_RUN_S` | Seconds to run before stopping the client cleanly and exiting 0. |
| `DRIVER_POLL_S` | The client's own polling or heartbeat interval, where it has one. |
| `DRIVER_UID` | `random` (default), `zero`, `none` (pass none: the client makes one up), or 32 hex characters: the instance UID to start with, if the client lets you choose. |
| `DRIVER_CHANGES_AT_S` | Comma-separated seconds after start at which to change the agent's status (set the non-identifying attribute `driver.change` to 1, 2, …). Skip if the client can't change its description after start. |
| `DRIVER_FIRST_CHANGE` | The status to start with: `driver.change` set to this number (unset: 0), and the changes of `DRIVER_CHANGES_AT_S` count on from it. Torture restarts a driver it crashed with the status the application had reached (clients with the `instance_uid_kept` feature). |
| `DRIVER_TOKEN` | If set, send `Authorization: Bearer <token>` with every request or upgrade. |
| `DRIVER_ACCEPT_CONNECTION_SETTINGS` | `1`: accept offers of new connection settings (clients with the `connection_settings` feature). |
| `DRIVER_OWN_TELEMETRY` | `1`: declare `ReportsOwnMetrics`, `ReportsOwnTraces` and `ReportsOwnLogs`, and answer every set of telemetry settings with `APPLIED` and its hash (clients with the `telemetry_settings` feature). |
| `DRIVER_COMPONENTS` | `1`: report two available components, leaving the hash to the client: `receivers` holding `otlp` (metadata `version` = `1.0.0`) and `exporters` holding `debug` (clients with the `available_components` feature). |
| `DRIVER_CUSTOM` | `1`: declare the custom capability `io.opentelemetry.echo`, answer each custom message of type `ping` with one of type `pong` and the same data (clients with the `custom_messages` feature). |
| `DRIVER_PACKAGES` | `1`: accept packages; install each by reading its file (report its SHA-256) and answering installed, and answer each removal (clients with the `packages` feature). |
| `DRIVER_REQUEST_UID` | `1`: ask the server for an instance UID (`RequestInstanceUid`), the UID started with being the temporary one (clients with the `request_instance_uid` feature). |
| `DRIVER_CA_FILE` | With TLS: a PEM file holding the CA to trust for the server's certificate, instead of the system's (clients with the `tls` feature). A scenario whose package files are served over TLS names their CA here too, for downloads only: the OpAMP endpoint stays plain (clients with the `core` feature). Empty otherwise. |
| `DRIVER_CLIENT_CERT`, `DRIVER_CLIENT_KEY` | When the server asks for client certificates: PEM files of the certificate (the CA's, subject `CN=conformance-agent`) and its PKCS #8 private key, to present in every handshake (clients with the `mtls_configured` feature). Empty otherwise. |
| `DRIVER_PROXY` | A proxy to reach the server through: `http://user:pass@host:port` (an HTTP proxy: CONNECT, or requests in absolute form for `http://` endpoints) or `socks5://user:pass@host:port`, credentials included when the proxy needs them (clients with the `proxy_configured` feature). Empty otherwise. |
| `DRIVER_LINGER_S` | Seconds to stay alive after `stopped` before exiting 0 (unset or 0: at once), calling nothing: a socket or download the client left running after stop shows in the server's log (scenarios `stop-mid-*`, clients with the `core` feature). With `DRIVER_THROW_ON_CONFIG`, also after the `fatal` of a client that stopped on its own, before exiting 1: the driver still reports anything the client tells it then (a second `stopped` with an error is a second `fatal`). |
| `DRIVER_THROW_ON_CONFIG` | `1`: the notification callback itself throws (raises, panics) on the first remote config, with a message containing `callback-throws`, after saying `throwing`; it does not answer that config. The throw must come from inside the callback the SDK calls, not from code the driver runs later. The client must then stop on its own (RULE-SHELL-02): the driver reports `fatal` with the error it is told (in Rust, the one `Client::stop` returns: `Notification::Stopped` carries none), never stops the client itself, and exits 1 (scenario `callback-throws`, clients with the `callback_failure` feature). |
| `DRIVER_STATE_FILE` | Where the client keeps its state, if it can. A scenario may run the driver more than once with the same file: a restart. |
| `DRIVER_MAX_SEND_BYTES` | If set, the client's `max_send_bytes`: the largest message it sends (clients with the `max_send_bytes` feature). |
| `DRIVER_DESCRIPTION_PAD` | If set, add the non-identifying attribute `driver.pad` to the description, that many `x` characters long: with `DRIVER_MAX_SEND_BYTES`, a description too large to send (scenario `description-too-large`). |

Use `service.name = conformance-<language>` as the identifying attribute. Enable remote
config (capabilities: `AcceptsRemoteConfig` alone, plus the own-telemetry ones with
`DRIVER_OWN_TELEMETRY`; the core adds those of what the client reports), and answer every offered
config with `APPLIED` and its hash, in the notification callback or right after it on the client's
own loop: not from another thread's loop, whose answer can come after the client has gone on (after
a restart, the first message then says `APPLYING` for the config restored). With `DRIVER_UID` `random` or `none`, let the client make the UID
up. Every driver configures its client alike, so the SDKs' core taps of a scenario compare
(`conformance/differential/shells.py`).

## Output: JSON lines on stdout

One object per line, each with `event` and `wall` (Unix time in seconds, as a float). Anything
else on stdout is kept as an `output` event; put logs on stderr.

| `event` | When | Fields |
|---|---|---|
| `started` | the client has been started | `client`, optionally `uid` |
| `connect` | the client's on-connect callback | |
| `connect_failed` | the client's on-connect-failed callback | `error` |
| `error_response` | the client reported a `ServerErrorResponse` | `type`, `message` |
| `message` | the client's on-message callback | `remote_config_hash` when a config came with it |
| `connection_settings` | the client handed a connection-settings offer to the application | `heartbeat_interval_seconds` |
| `telemetry_settings` | the client handed telemetry settings to the application | `hash` (hex), `metrics_endpoint` (or null) |
| `custom_message` | the client handed a custom message to the application | `capability`, `type`, `data` (hex) |
| `custom_message_not_sent` | a custom message the driver sent won't be | `reason` |
| `server_custom_capabilities` | the server's custom capabilities changed | `capabilities` |
| `install_package` | the client asked the application to install a package | `name`, `version`, `sha256` (hex, of the file), `error` (reading it, or null) |
| `remove_package` | the client asked the application to remove a package | `name` |
| `status_change` | just before the driver changes the status | `change` |
| `stopping` | just before the driver stops the client | |
| `throwing` | just before the callback throws (`DRIVER_THROW_ON_CONFIG`) | |
| `stopped` | the client has stopped | |
| `fatal` | the driver cannot continue, or the client stopped on its own (then exit 1) | `error` |

A driver exits 0 after `stopped`. Any other exit, or no `stopped`, fails `driver_survives`: a
client that takes the process down with it is a conformance failure. The runner gives a driver
120 s to say `started`, then its run, its linger and 30 s; once it said `stopped`, its linger and
30 s from then: a driver still running after that is killed (and fails). How long each took to end
after `stopped`, beyond its linger, is in the run's `result.json` (`exit_after_stopped_s`), and
named in the run's line when over 5 s: something the client left running past stop shows there
first.

## Adding a client

Write the driver, then add it to `clients.toml` with the transports it supports, how to build
it, and how to run it. List the `features` it supports (`connection_settings`, `remote_config_lifecycle`, `core`, `instance_uid_kept`, `telemetry_settings`, `available_components`, `custom_messages`, `packages`, `request_instance_uid`, `callback_failure`, `tls`, `mtls_configured`, `proxy_configured`, `proxy_socks5`, `number_zero_until_answered`, `max_send_bytes`): a scenario whose
`needs` names a feature runs only on clients that list it.

`callback_failure`: the driver honours `DRIVER_THROW_ON_CONFIG`, and its SDK keeps RULE-SHELL-02
(spec/shell-contract.md): a callback that fails stops the client, and only the client, and the
application hears `stopped` with that error once. The `callback-throws` scenario's check,
`callback_failure_stops`, expects one `fatal` with the error, nothing heard after it, exit 1, no
goodbye, and no message more than one and a half heartbeat intervals after the throw.

`core`: the client runs the shared core and carries out its effects exactly: its requests as given
(no redirect followed: the `redirect` scenario), its cancels and its stop (nothing it started
outlives stop: `stop-mid-*`, and the driver honours `DRIVER_LINGER_S`), and every download's
response, whatever its status, for the core to judge; the driver downloads trusting
`DRIVER_CA_FILE` when it is set.

The last four are about the driver's configuration of the client, over both transports: `tls`, it
trusts `DRIVER_CA_FILE`; `mtls_configured`, it presents `DRIVER_CLIENT_CERT` and `DRIVER_CLIENT_KEY`;
`proxy_configured`, it goes through an HTTP proxy given as `DRIVER_PROXY`; `proxy_socks5`, a SOCKS5
one too. List one only if the client does it over
plain HTTP and WebSocket alike. A driver in a container reads the files under `/out` (the runner
passes the paths as the container sees them) and reaches the proxy as `host.docker.internal`, so
pass the four variables with `-e`. Set `upstream = true` for a client we
use unchanged, and `docker = true` if the driver runs in a container.
