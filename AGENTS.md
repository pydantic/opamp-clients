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
`check` verifies it. Moving to a new core is one PR holding the lock, the module copies checked in
beside each client and the types regenerated from it: `fetch_core.py lock <version>`, `fetch`,
then `scripts/gen_core.py` and `scripts/gen_sdk_settings.py`.

The compiled core isn't MIT: it has its own license, `licenses/core/LICENSE.txt`, with
`THIRD_PARTY_NOTICES.txt` beside it. Both come in the release's tree, and `fetch` puts a copy beside
the core in each package that ships it (`fetch_core.py`'s `LICENSE_COPIES`); `check` holds every
copy to the lock. A package that carries the core ships both files, and its metadata names the
core's license as well as MIT (`sdk_release.py check` and `native_dist.py check` refuse one that
doesn't).

## The Supervisor fork

`go/opampsupervisor` is upstream's `cmd/opampsupervisor` (opentelemetry-collector-contrib) at the
release the collector pins name, with its OpAMP client swapped for the core and the changes its
README lists under "What changes". It tracks upstream: each upstream release is merged in, so
every change here is one that must merge again, and is made the way that keeps that easy.

Allowed:

- New files of our own (MIT), holding the new code.
- A small hook in an upstream function: a line or a few that call into our file, marked with a
  `// Changed here:` comment.
- A fix to upstream's code, with a test that fails without it.

Each one is named in the changed file's header ("Changed here: ...") and in the README's "What
changes".

Not allowed:

- Rewriting or splitting an upstream function where a hook would do.
- Deleting or renaming upstream's files, types, functions or fields.
- Reformatting or reordering upstream's code.
- A behavior change the README doesn't list.

Anything beyond this says in its PR why the divergence is worth maintaining, and what else was
tried. A fix that isn't ours alone (upstream's bug, not our client's) goes upstream too.

`python3 scripts/collector_release.py diverged` lists how each file differs from upstream's at the
pinned release, and fails on a changed file whose header doesn't say so or a missing upstream file
`DROPPED` doesn't explain; CI runs it. When a new contrib release is out (`latest` fails nightly),
`python3 scripts/collector_release.py port vX.Y.Z` three-way merges what upstream changed since the
pinned release into the fork; resolve its conflicts, then move the pins (`pins`) in the same PR.
