# OpAMP clients

OpAMP client SDKs for Python, Go, Node.js, Java and .NET, on one shared core.

The core is the Rust state machine every client wraps. It is built, tested and proved in
[pydantic/opamp](https://github.com/pydantic/opamp) and arrives here compiled: the WebAssembly
modules Go, Node.js and Java load, the native library Python and .NET load, and the interface
(`.proto`) files the clients generate their types from. `opamp-core.lock.json` pins one core
release and the sha256 of every file in it.

```bash
scripts/fetch_core.py fetch                  # download what the lock pins, verify it, put it in place
scripts/fetch_core.py fetch --all-platforms  # and every platform's native library, for packaging
scripts/fetch_core.py check                  # what is in place is what the lock pins
scripts/fetch_core.py lock 0.2.0             # move to another core release
```

Moving to a new core is a PR that changes only the lock.

The SDKs are moving here from pydantic/opamp; see its `bridge/opamp-clients/README.md` for the plan.
