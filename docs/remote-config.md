# Remote config

How the core handles the configs a server offers (the spec's "Configuration" section), and what
the application does with them.

## What the spec leaves to the client

The server puts `remote_config` in its responses while the hash the agent last reported
(`remote_config_status.last_remote_config_hash`) differs from the config's: every response,
until the agent reports on it. The spec says nothing more about the agent side, so without
care an application:

- gets the same config again with every response until its answer reaches the server, and
  reapplies it (restarts, reloads) for nothing;
- runs nothing after a restart until it connects and the server resends;
- can report on a config that has since been replaced, telling the server the wrong thing.

## The flow

1. **A new config.** A response carries a config whose hash differs from the last the core
   received. The core saves it (a `Store`), and reports APPLYING for it. The same config again
   changes nothing.
2. **One at a time.** If the application is free, the core hands it the config (the
   `RemoteConfig` notification). If it is still working on another, the new one waits; a
   later one replaces it. The application never gets a config that was replaced before it
   could start on it.
3. **The answer.** The application answers with `set_remote_config_status`: APPLIED or
   FAILED, with the config's hash. If the config is still the latest, the core reports the
   answer. If a newer one arrived meanwhile, the answer is dropped and the core hands over the
   newer one.
4. **After a restart.** If a config was saved, the core hands it to the application at start,
   before connecting, and reports APPLYING for it until the application answers. The
   application runs its last config even if the server is unreachable, and the server
   doesn't need to resend it.

A config is named by its `config_hash`, or by a fingerprint of its content if the server sends
none. Without persistent storage, step 4 doesn't happen: after a restart the core has nothing,
reports nothing, and the server resends (the spec's behaviour).

## What the application must do

- Answer every `RemoteConfig` notification it gets, once, with that config's hash.
- Work on one config at a time. The core doesn't send another until it has the answer.
- Report later changes (the config stopped working, say) with `set_remote_config_status` for
  the latest config; the core takes those too.

## What holds

The core is checked against these rules on its own before it is released, against a model of the
flow, with and without storage, with configs changing and rolling back, crashes anywhere, and
saves that fail:

- **The status is about the last config received**, never an older one.
- **APPLIED is true:** the application runs that config.
- **Once each:** a config is handed over once, not again with every resend.
- **It converges:** once the operator stops changing the config, the server hears the outcome
  for its config (a config the application rejects stays FAILED until the server changes it).
- **The last config is saved:** with no save on its way or owed, the disk holds the last config
  received. A save holds the whole state as it was when it began, one at a time, so a config
  received meanwhile is saved by the next. A save that fails leaves the state owed: the core
  saves the whole state again after a backoff, until one lands.

Why one at a time: if every new config were handed over at once, in a queue, and an answer taken
as the latest config's when the hashes matched, then after a rollback (config a, then b, then a
again, while the application was still on the first a) the answer for the first a would be
reported as the answer for the latest a. The application would then finish b, and run b while the
server heard APPLIED for a. Handing over one config at a time makes every answer about the config
in flight.

The conformance scenarios `remote-config`, `remote-config-resent` and `remote-config-restart` run
the flow on every client over both transports.

## Who does what

| | Does |
|---|---|
| The core | the flow above; carries `remote_config_status` as a status field; the last config and the one in flight; saving the last config in the state (with the connection settings) and handing it over at start |
| The client | the `RemoteConfig` notification and `set_remote_config_status`; the state file ([state-file.md](state-file.md)) |

## Saved state

The last config is saved with the connection settings, in the same versioned state. It is written
when a new config arrives, not when the application answers: after a restart the application is
given the config and answers again.
