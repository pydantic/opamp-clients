# AGENTS.md

Guidance for agents and contributors working in this repository.

## This repository is public: never put these in it

Everything pushed here, on any branch, in a PR, a commit message, an issue or a release note, is
public at once and stays public. The OpAMP core and our server are not open source, and they live
in a private repository. Never add, quote or describe:

- **The core's source**: Rust (`.rs`, `Cargo.toml`, `Cargo.lock`, `rust-toolchain*`), the TLA+
  models (`.tla`), the Kani and fuzz harnesses, or anything from the private repository's `rust/`,
  `fuzz/`, `tla/` or `verification/`.
- **Pointers into it**: a path such as `rust/opamp-core/src/...`, a function or file name from
  inside a core crate, or a comment that explains how the core is built inside. Say what a thing
  does ("the tap checker's file records"), not where it lives.
- **Our server**: its source, its configuration, its tests and baselines, and the conformance
  results against it.
- **Binaries**, other than the core's WebAssembly modules at the paths `scripts/fetch_core.py`
  puts them. The native libraries are fetched, never committed.
- **Infrastructure**: internal hostnames, accounts, cloud resources, runner names (we use only
  GitHub-hosted runners here) and deployment details.
- **Credentials**: tokens, keys, passwords. Private keys only as made-up test fixtures, under
  `tests/` or `fixtures/`.
- **History**: nothing comes over with git history from the private repository. Code moves as
  fresh commits of chosen files.

`scripts/public_check.py` refuses most of this, and CI runs it on every PR and push. Passing it is
not permission: it can't see a description of the core in prose, so read your diff with this
list in mind before pushing.

## The core

The clients load the core compiled, from a `core/vX.Y.Z` release in this repository, pinned by
`opamp-core.lock.json` (every file's sha256). `scripts/fetch_core.py fetch` puts it in place and
`check` verifies it. Moving to a new core is a PR that changes only the lock.
