# Own-telemetry and other connection settings

How the core handles a server's offer of where the agent sends its own telemetry
(`own_metrics`, `own_traces`, `own_logs`) and of settings for its other connections
(`other_connections`): the spec's "Connection Settings Management" for everything but the OpAMP
connection itself ([connection-settings.md](connection-settings.md) covers that).

This is how a server tells an SDK or agent where to export its telemetry, and rotates the
credentials it uses.

## Supervisor exporter credentials

The Supervisor can bind a named connection's bearer credential to a Collector
environment variable. Enable `accepts_other_connection_settings` and configure
`agent.connection_env`, for example `logfire/otlp: LOGFIRE_TOKEN`.
The server supplies an `Authorization: Bearer ...` header for that connection.
The Collector policy references `${env:LOGFIRE_TOKEN}`.

The Supervisor accepts only configured bindings. It restarts the Collector with
the new environment before it reports the settings as applied. The client saves
the offered settings and restores them after a restart. Set `storage.state_key`
to encrypt those settings at rest. The enrollment helpers configure this binding
and preserve the key that protects previously issued credentials.

## What the spec says, and what it leaves open

- An offer's `hash` covers all of its settings, including parts left out because they are
  unchanged, and the agent reports one `connection_settings_status` for it.
- A part left unset is unchanged. For `other_connections`, a field left unset in a named
  connection keeps its old value.
- The server offers settings when they change and SHOULD NOT send them otherwise. After a
  restart the core reports the status it saved, so the server sees the hash it already has and
  sends nothing: the agent must still have the settings.
- The spec doesn't say whether a new `other_connections` map replaces the old one or adds to
  it. Here it **replaces** it: a name missing from the new map is gone, and a name in both keeps
  the fields the new offer leaves unset. The hash covers all settings, so the server's map is the
  whole set.

## The flow

1. **Offer.** A response carries `connection_settings` with a hash the core hasn't seen, and
   one or more of the telemetry parts. Parts the agent hasn't asked for (the capabilities below)
   are ignored. An offer without a hash is named by its content, one name for both parts.
2. **Check.** A telemetry endpoint must be a well-formed `http(s)://` URL (the spec: a full
   OTLP/HTTP URL), or empty: a signal with no endpoint is switched off, which is how the server
   stops own telemetry it no longer wants (and what a key's offer carries to an agent that may
   hold some). Every header (of own telemetry, other connections and proxies) well
   formed; an other connection's endpoint and a proxy's URL must hold no control character
   ([connection-settings.md](connection-settings.md#what-the-core-refuses)). An offer that fails
   the check is answered FAILED at once, and the application never sees it. The refusal is saved
   with the offer: the settings saved are still the ones before, so without it a restart would
   report the offer by its OpAMP part alone (APPLIED once that is proven, or APPLYING for good).
3. **Merge and save.** The core applies the offer to the settings it has (parts replaced,
   other connections merged as above) and saves the result with the offer's hash.
4. **Hand over, one at a time.** The application gets the full current settings (the
   `ConnectionSettings` notification) when it is free; a newer offer that arrives while it is
   busy replaces any that was waiting, as for remote configs ([remote-config.md](remote-config.md)).
5. **The answer.** The application points its exporters at the new settings and answers with
   `set_connection_settings_status`: APPLIED, or FAILED with the reason. An answer about an offer
   that has since been replaced is dropped.
6. **After a restart**, the saved settings go to the application at start, before connecting,
   as for remote configs, reported APPLYING until it answers. Parts it no longer asks for are
   left out. A refusal saved with the latest offer is reported again: that offer stays FAILED. A
   candidate for the OpAMP connection back from disk counts as APPLYING until it is resolved.

## One status for an offer with both kinds

An offer can carry OpAMP settings and telemetry settings together. Each part reaches its own
outcome: the OpAMP part when the new connection is proven or dropped
([connection-settings.md](connection-settings.md)), the telemetry part when the application
answers. The status reported for the offer combines them:

- FAILED as soon as either part fails (with that part's reason);
- APPLIED once every part the offer carries is applied;
- APPLYING otherwise.

Nothing new is reported about an older offer once a newer one has arrived: a status still waiting
to go is withdrawn, and until the newer offer has a status of its own (the OpAMP part reports
APPLYING once saved), a full report repeats the last one sent. A part still in
progress from an earlier offer counts towards the newest offer's status too: the newest hash
covers every setting, including ones carried over unchanged. So does an OpAMP part still being
saved (it reports APPLYING only once saved), and an attempt that carries on while its part
refuses a later offer at once (new OpAMP settings while others are being tried, say). An OpAMP
part that waits because the core is busy ([connection-settings.md](connection-settings.md#an-offer-while-busy))
counts as APPLYING only when its offer is the latest: its telemetry part was taken in, or it took
the place of a latest offer whose OpAMP part waited or failed to save. Otherwise it waits unseen,
holding nothing up, and the status stays about the offer before it until it is taken. A later
offer replaces the one waiting, and a restart keeps it.

## What the reference server offers

A scenario's `server.own_telemetry` gives each signal (`metrics`, `traces`, `logs`) as
`{"endpoint", "headers": {name: value}, "tls": {"ca_pem", "include_system_ca_certs",
"insecure_skip_verify", "min_version", "max_version", "cipher_suites": [...]}, "certificate":
{"cert_pem", "private_key_pem", "ca_cert_pem"}, "proxy": {"url", "headers": {name: value}}}`; each
part left out is left out of the offer. It offers no
`other_connections` yet.

## How it is checked

The combined status is a rule (RULE-CLIENT-08). The core is checked against it on its own before
it is released, with random offers, each part carried, carried quietly, refused at once or left
out, random outcomes, failed saves and crashes, and against a model of the flow above. Here,
the conformance scenario `own-metrics-restart` (RULE-CLIENT-07) runs on every client over both
transports: the server offers own metrics, the application gets them and answers, and after a
restart gets them again at start without the server resending them.

## Capabilities

The application says what it can use, and the core offers only that to the server:
`ReportsOwnMetrics`, `ReportsOwnTraces`, `ReportsOwnLogs` for telemetry,
`AcceptsOtherConnectionSettings` for other connections. With any of them the core also declares
`ReportsConnectionSettingsStatus`.

## Who does what

| | Does |
|---|---|
| The core | the flow above: the check, the merge, saving, handing over one at a time, the combined status, restoring at start; carries `connection_settings_status`, as for the OpAMP connection |
| The client | the `ConnectionSettings` notification and `set_connection_settings_status` (in each language's own style: `SetConnectionSettingsStatus`, `setConnectionSettingsStatus`); saving, as for everything else ([state-file.md](state-file.md)) |
