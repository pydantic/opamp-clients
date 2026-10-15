# The default state directory

An instance keeps its UID across restarts only with a state directory
([instance-uid.md](instance-uid.md)). Asking every application to name one meant most didn't, and
showed up as a new instance at every restart. So each SDK integration keeps one of the user's own
by default. The rules are the same in every language.

## When

Only when `OTEL_OPAMP_ENDPOINT` is set and `OTEL_OPAMP_STATE_DIR` is not. It is not used:

- when `OTEL_OPAMP_STATE_DIR` names a directory (that one is used, as before);
- when `OTEL_OPAMP_STATE_DIR=none` (each start is a new instance, as before this change);
- when `OTEL_RESOURCE_ATTRIBUTES` has a `service.instance.id` that is a UUID: the identity the
  application chose stays its choice.

A `service.instance.id` that isn't a UUID can't be the instance UID, so the SDK replaces it, in the
telemetry too. The default directory keeps the UID that replaces it, so it is the same at every start.

## Where

Under the user's state directory:

| | Base |
|---|---|
| Linux and other Unix | `$XDG_STATE_HOME`, else `$HOME/.local/state` |
| macOS | `$HOME/Library/Application Support` |
| Windows | `%LOCALAPPDATA%` |

then `opamp/<service>-<hash>/<slot>`:

- `<service>` is `OTEL_SERVICE_NAME`, else `service.name` from `OTEL_RESOURCE_ATTRIBUTES`, else the
  program's name; anything but letters, digits, `.`, `_` and `-` becomes `_`, cut to 64 characters.
- `<hash>` is the first 8 hex digits of the SHA-256 of the service, `service.namespace`, the
  program's name and the working directory, joined with NUL. Two checkouts of one service on a
  host, or two services sharing a program, don't share an identity.
- `<slot>` is `1` to `16`. A process takes the first slot whose `lock` file it can lock (the
  operating system's lock, without waiting; held until the process ends, never inherited by a
  process it starts). So the processes of a program running at once each keep their own identity,
  and a restarted one takes back the slot it had (the lowest free one). Go, Node.js and Rust also
  let it go when the SDK is shut down, so an SDK set up again in the same process takes the same
  slot, and the same identity, back.

  Node.js has no such lock in core, and its integration takes no native dependency for one: there
  the lock file is made exclusively (`O_EXCL`), names the process holding it (its pid and when it
  started), and is removed when the process exits. One left by a process that ended without exiting
  (killed, crashed) names a process that no longer runs, or this process's own pid from an earlier
  start (a container's application is often pid 1 each time), and is taken over. Two processes
  starting in the same instant can both take over the same stale lock; the operating system's lock
  has no such case.

Directories are made readable by the user only, as a named state directory is.

The program's name is the language's own:

| | Program |
|---|---|
| Python | the script or module run (without `.py`), else `python` |
| Java | the main class, or the jar run with `-jar` (without `.jar`), as the first word of the `sun.java.command` property; else `java` |

## When it can't

No base (no `HOME`), a base that can't be written, or all 16 slots taken: no state directory, and
each start is a new instance, exactly as before. That is never an error; the last case is logged
at INFO.

## How it is checked

Each integration's unit tests: the same UID after a restart, another slot (and UID) for a second
process at once, another UID for another service, and each case above that turns it off.
