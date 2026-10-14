# OpAMP clients

OpAMP client SDKs for Python, Go, Node.js, Java and .NET, on one shared core.

The core is the state machine every client wraps. It arrives here compiled, as this repository's
`core/vX.Y.Z` releases: the WebAssembly modules Go, Node.js and Java load, the native library
Python and .NET load, and the interface (`.proto`) files the clients generate their types from.
`opamp-core.lock.json` pins one core release and the sha256 of every file in it.

```bash
scripts/fetch_core.py fetch                  # download what the lock pins, verify it, put it in place
scripts/fetch_core.py fetch --all-platforms  # and every platform's native library, for packaging
scripts/fetch_core.py check                  # what is in place is what the lock pins
scripts/fetch_core.py lock 0.2.0             # move to another core release
```

Moving to a new core is a PR that changes only the lock.

This repository is public and holds only what `PUBLIC_ALLOWLIST` names; see `AGENTS.md`.
