# Publishing the SDKs to their registries

The [Registries workflow](../.github/workflows/registries.yml) publishes an SDK release from an
`sdk/vX.Y.Z` tag on `main`, and from nothing else. Run by hand, it takes no version: it runs for
the tag it is started from (Use workflow from: `sdk/vX.Y.Z`) and refuses a branch. Its build job
holds no credentials. It stamps every manifest with the version, builds every package, and checks
that none holds the cores' sources (`scripts/sdk_release.py`, `scripts/native_dist.py`). Then one
job per registry uploads, each in its own environment (`pypi`, `npm`, `maven`, `nuget`), and only
once the repository variable `SDK_REGISTRIES` names that registry, for example `pypi npm maven
nuget`. Until then each job is a dry run that lists what it would publish. On a PR that changes the
workflow, the scripts, the poms or the lock, all of it runs as a dry run at version 0.0.0, outside
the environments, so it waits on no approval.

| Registry | What goes there, in order | How it signs in |
| --- | --- | --- |
| PyPI | the native core (one wheel per platform, no sdist), then the client, then otel | trusted publishing (OIDC) |
| npm | the client, then otel | trusted publishing (OIDC), with provenance |
| Maven Central | one bundle: the client and the Java agent extension (with its `app-settings` jar), each with its sources and javadoc jars, every file signed | a Central Portal user token, and a GPG key |
| NuGet | the native core, the client, the hosting package (`WithOpAmp()`), then the automatic instrumentation plugin | trusted publishing (OIDC): a key that lasts an hour |
| Go | nothing to upload | none |

Each package goes up after the packages it pins, so no moment has a package on a registry that
can't be installed. A rerun is safe: PyPI skips the files it has (`skip-existing`), npm skips a
version it has (`npm view` first), NuGet skips duplicates (`--skip-duplicate`), and Maven skips the
bundle when Central has both artifacts at the version. Central publishes a bundle whole or not at
all; if a rerun finds one artifact there and not the other, the job fails and says so.

For Go, push a tag per module: `go/vX.Y.Z` for the client, then `go/opampotel/vX.Y.Z`,
`go/opampextension/vX.Y.Z` and `go/opampsupervisor/vX.Y.Z` on a commit whose `go.mod` files
require that client version. The module proxy serves them, and caches each tag for good. The
`replace` in each `go.mod` only applies inside this repository. The Supervisor is installed from
its packages, since `go install` refuses a module with a `replace`.

The names are Pydantic's: `pydantic-opamp-*` on PyPI, `@pydantic/opamp-*` on npm, `dev.pydantic`
on Maven Central and `Pydantic.OpAmp.*` on NuGet. Each scope or prefix must be ours on its registry
before the first upload: a name on a registry can't be taken back.

## What the maintainer sets up before the first release

The workflow names these environments, secrets and variables. A session can't create or read any
of them.

**GitHub** (repository settings):

- A tag ruleset on `sdk/v*` (and on `go/*`) that lets only maintainers create, move or delete those
  tags.
- Four environments: `pypi`, `npm`, `maven` and `nuget`. Each gets:
  - Deployment branches and tags: Selected, with one tag rule, `sdk/v*`, and no branches.
  - Required reviewers: the maintainer.
- In `maven`, four secrets:
  - `MAVEN_CENTRAL_USERNAME` and `MAVEN_CENTRAL_PASSWORD`: the two halves of a Central Portal user
    token.
  - `MAVEN_GPG_PRIVATE_KEY`: the signing key, ASCII-armored (`gpg --armor --export-secret-keys`).
  - `MAVEN_GPG_PASSPHRASE`: its passphrase.
- In `nuget`, one variable: `NUGET_USER`, the nuget.org profile name that owns the trusted
  publishing policy (not an email address). There is no NuGet secret: the old `NUGET_API_KEY` is
  no longer read, and can be deleted once the policy works.
- The repository variable `SDK_REGISTRIES`: the registries to really upload to, e.g.
  `pypi npm maven nuget`. Leave a registry out until its setup below is done.

**PyPI** (pypi.org, Account, Publishing): a pending trusted publisher for each of
`pydantic-opamp-core`, `pydantic-opamp-client` and `pydantic-opamp-otel`. Each has owner
`pydantic`, repository `opamp-clients`, workflow `registries.yml` and environment `pypi`. The first
upload creates the project. No token.

**npm** (npmjs.com):

- The `@pydantic` scope must belong to an npm organization the maintainer administers.
- A trusted publisher is added in a package's settings, so the package must exist first. Publish
  the first version of `@pydantic/opamp-client` and `@pydantic/opamp-otel` by hand, from the
  tarballs a run of this workflow built (its `sdk` artifact): `npm publish --access public <tgz>`.
- Then, on each package, add a trusted publisher: GitHub Actions, owner `pydantic`, repository
  `opamp-clients`, workflow `registries.yml`, environment `npm`, with `npm publish` allowed.
- Then set "require two-factor authentication and disallow tokens" on each.
- Trusted publishing needs npm 11.5.1 or later: the job checks the version Node 24 brings.

**Maven Central** (central.sonatype.com):

- Verify the namespace `dev.pydantic` (a DNS TXT record on `pydantic.dev`).
- Generate a user token: it goes into the two `MAVEN_CENTRAL_*` secrets.
- Make a GPG signing key for releases and publish its public key to `keyserver.ubuntu.com` (or
  `keys.openpgp.org`): Central checks every signature against a public keyserver.

**NuGet** (nuget.org):

- Reserve the ID prefix `Pydantic.OpAmp` for the owning account (a request to nuget.org support).
- Add a trusted publishing policy (your username, Trusted Publishing): owner `pydantic`, repository
  `opamp-clients`, workflow file `registries.yml`, environment `nuget`.

## A bad release

A version on a registry is there for good: the fix is the next version. To keep users off a bad
one in the meantime: PyPI, yank it (the project's Manage, Releases page); npm, `npm deprecate
@pydantic/opamp-client@X.Y.Z "<why>"`; NuGet, unlist it (the package's Manage page); Go, a
`retract` directive in the next version's `go.mod`. Maven Central can't hide a version: say what is
wrong in the next release's notes.
