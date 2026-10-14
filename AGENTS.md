# AGENTS.md

Guidance for agents and contributors working in this repository.

## Public by allowlist

This repository is public: anything pushed here, on any branch, in a PR, commit message, issue or
release note, is public at once and stays public. It holds only what `PUBLIC_ALLOWLIST` names, and
`scripts/public_check.py` (run by CI on every PR and push) refuses any other file, any binary but
the core's modules, and any credential.

- Add a path to `PUBLIC_ALLOWLIST` only in a PR that says why it belongs here.
- Write about this repository's own code. Don't name or describe code, paths, services or
  infrastructure that live anywhere else; if a comment needs to explain the core, say what it does
  for the clients, not how or where it is built.
- The check reads files, not prose meaning, so passing it isn't permission: read your diff, PR
  body and commit messages before pushing.

## The core

The clients load the core compiled, from a `core/vX.Y.Z` release of this repository, pinned by
`opamp-core.lock.json` (every file's sha256). `scripts/fetch_core.py fetch` puts it in place and
`check` verifies it. Moving to a new core is a PR that changes only the lock.
