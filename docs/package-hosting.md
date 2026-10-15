# Hosting the apt and yum repositories with Pulumi: the plan

The Packages workflow (`.github/workflows/packages.yml`) publishes each Supervisor release to
signed apt and yum repositories in a bucket, with `go/opampsupervisor/packaging/publish.sh`.
Nothing it publishes to exists yet. This page is the plan for making it with Pulumi
(`infra/packages`), reviewed like code, rather than by a hand-run script that drifts. It covers the publishing side, and the
agent releases each platform release carries into its deployments, on-prem included. Nothing
here is created yet.

## Two kinds of bucket

- **The publishing bucket**, one, Pydantic's: what the Packages workflow writes each release to,
  served to the internet at `https://packages.pydantic.dev`. Most agents upgrade from it. Beside
  the apt and yum repositories, it holds the `.pkg`, the `.msi`, the Java agent extension and the
  .NET plugin as plain files, with a signed checksum list.
- **A distribution bucket for each deployment** of the platform, ours or a customer's on-prem.
  It holds the agent releases that match that deployment's platform version, so its agents can
  upgrade without reaching the internet. A deployment opts its agents into upgrading from it,
  which an on-prem or air-gapped deployment needs.

Nothing syncs from one to the other. A distribution bucket is filled from the platform release
the deployment runs.

## The release bundle

We publish the bundle ourselves, as a release of its own, and the platform pins one bundle. The
platform never builds or picks agent versions itself: it names a bundle, and gets every agent
artifact that goes with it.

- **Its own version.** A bundle release (`bundle/v2026.10.1`) names the agent versions it
  carries. Several agent releases can go into one bundle, and a bundle can be cut again with a
  fix without moving the platform.
- **What's in it:** the agents installed on hosts as files, at those versions: the
  Supervisor's deb and rpm (amd64, arm64), its macOS `.pkg` and Windows `.msi`, the Java agent
  extension jar (`opamp-javaagent-extension`), and the .NET automatic instrumentation plugin
  (`Pydantic.OpAmp.AutoInstrumentation`). It also holds the apt and yum repositories over them, signed with the
  same key as `packages.pydantic.dev`, and a manifest of every file's hash.
- **Built and published here.** This repository's release workflow builds the bundle and attaches
  it to the bundle's GitHub release, from the same binaries that go to the publishing bucket.
- **Pinned by digest.** The platform pins the bundle's version and its SHA-256, in one file of
  its own. Its release CI fetches that bundle, checks the digest and the signature, and ships it
  with the platform's own artifacts. Bumping agents in the platform is a one-line PR there.
- **Not the SDK libraries.** The Python, Node, Go, Rust, Java and .NET clients are libraries
  that apps take as dependencies, from PyPI, npm, the Go proxy, crates.io, Maven Central and
  NuGet. On-prem sites mirror those registries the way they already do.
- **Signed once, in CI.** Deployments never hold the signing key: apt, dnf and the bundle check
  all use the published public key.

## Importing a bundle

A deployment's upgrade imports the bundle of its new platform version into its distribution
bucket. For ours, that's a step in the deploy. On-prem, it's a Helm hook (or one command beside
the upgrade), and for an air-gapped site the bundle is carried across with the platform's images.
The import (`go/opampsupervisor/packaging/import.sh`, or a small Go command):

1. Checks the bundle's signature and every file's hash against its manifest, and refuses it if
   anything is off.
2. Uploads in `publish.sh`'s order: packages, then indexes, then signatures. An agent upgrading
   mid-import sees the old release or the new one, never a mix. It writes to a bucket, any
   S3-compatible store such as MinIO, or a directory.
3. Keeps the previous platform version's packages, so rolling the platform back leaves its agent
   versions installable.

`publish.sh` and the import share the upload step, so the order holds in both.

## How an agent picks its source

- **apt and dnf hosts:** the source file (`pydantic-opamp.sources`, `pydantic-opamp.repo`) names
  the URL. A deployment's enroll step can write its own distribution URL there instead of
  `packages.pydantic.dev`, with the same key.
- **Upgrades over OpAMP:** the server doesn't offer packages yet (`PackagesAvailable`). When it
  does, each deployment has a setting for where the files it offers come from: `internet`
  (packages.pydantic.dev, the default) or `deployment` (its distribution bucket, through URLs it
  signs, so the bucket can stay private). Either way, the versions offered are the ones the
  platform version pins, and the agent checks the same signature.

## What has to exist

| What | Where | Why | Made by |
| --- | --- | --- | --- |
| The publishing bucket, readable by anyone | Pydantic, once | holds every release | Pulumi |
| An HTTPS load balancer with Cloud CDN, a managed certificate and a static address | Pydantic, once | serves it at `https://packages.pydantic.dev` | Pulumi |
| The DNS record for `packages.pydantic.dev` | Pydantic, once | points at that address | Pulumi, if the zone's provider has one |
| A service account that can write the publishing bucket alone, reached with Workload Identity Federation | Pydantic, once | what the Packages workflow publishes as, with no long-lived key | Pulumi |
| The `packages` GitHub environment, its variables, and protection (tags only) | Pydantic, once | where the workflow reads them | Pulumi (GitHub provider) |
| The signing key | Pydantic, once | signs the repositories | by hand, offline, never in Pulumi |
| A distribution bucket, private | each deployment | that platform version's agent releases | Pulumi component (ours), Helm chart or docs (on-prem) |
| An identity that writes that bucket alone, for the import | each deployment | what the upgrade imports the bundle as | the same |

The signing key stays out of Pulumi. Its state would hold the private key, and every machine
that installs from the repositories trusts that one key for good. It is generated once, offline,
kept in a password manager, and set as the `PACKAGES_SIGNING_KEY` secret by hand (`gh secret set`).

## Decisions taken

- **Pulumi, in Go.** Go is the language the Supervisor and its packaging already use. The
  program lives in `infra/packages/` in this repository, beside what it serves, with its own
  `go.mod` so the Supervisor's modules don't pull in the Pulumi SDK. It has two parts: a stack
  for the publishing side, and a `Distribution` component each deployment's stack instantiates.
- **Workload Identity Federation, not an HMAC key.** The workflow's GitHub OIDC token, limited
  to this repository's `packages` environment, becomes the service account. That leaves no secret
  to store except the signing key. `publish.sh` uses `gcloud storage`, which takes those Google
  credentials for `gs://` and AWS ones for `s3://`, so the same script (and the import) also
  writes to S3-compatible stores like MinIO. Its tests run against both `fake-gcs-server` and
  moto, with the same checks: the upload order, the caching headers, and the refusals.
- **The CDN keeps the bucket's `Cache-Control`** (`USE_ORIGIN_HEADERS`). Packages are cached
  for good and indexes not at all, as `publish.sh` sets them.
- **A preview on each PR, `pulumi up` by hand at first.** CI runs `pulumi preview` on PRs that
  touch `infra/`. Applying stays a person's step until the stack has settled, then moves to a
  workflow on main.

## Open questions, for Bill

1. **Which GCP project.** A public, production package host suggests a production project, or a
   project of its own for the packages.
2. **Where Pulumi state lives.** Pulumi Cloud, if Pydantic already uses it, or else a GCS state
   bucket with a KMS key (`gs://…`, `--secrets-provider gcpkms://…`).
3. **Who runs DNS for `pydantic.dev`.** With Cloud DNS or Cloudflare, Pulumi makes the record.
   Otherwise it prints the address, and the record is added by hand.
4. **Does an existing Pulumi repository own Pydantic's infrastructure?** If so, the program goes
   there instead, and this repository keeps only the workflow, the bundle and the import.
5. **What "a deployment" is in Pulumi today.** The `Distribution` component goes into whatever
   stack each platform deployment has. If none exists yet, it waits for that stack.
6. **Where the platform keeps its bundle pin.** It needs one file in the platform repository
   that names the bundle's version and digest, which its release CI reads.

## The PRs

1. **The Pulumi program's publishing side** (`infra/packages/`), tested in CI against Pulumi's
   mocks; `pulumi preview` joins once the stack exists.
   It doesn't touch publishing, so it can merge before anything exists.
2. **`publish.sh` on GCS with Workload Identity Federation**, with its tests moved to
   `fake-gcs-server`, and the workflow authenticating with `google-github-actions/auth`.
   This PR also removes `gcp-setup.sh`, which Pulumi replaces.
3. **The first release.** `pulumi up`, the signing key set by hand, then a
   `go/opampsupervisor/v0.1.0` tag. The workflow's last step installs that release from
   `https://packages.pydantic.dev` in a fresh Debian container. That step is the check that the
   whole chain works.
4. **The release bundle**, its own release (`bundle/vYYYY.MM.N`) naming the agent versions it
   carries. It comes with tests that apt and dnf install from the bundle alone with the published
   key, and that the manifest catches a changed file.
5. **The import and the `Distribution` component**, with tests that import into moto and a
   directory. They check that apt and dnf install from the result, that a bundle with a changed
   file or another key is refused, the upload order, and that the previous version is kept. The
   on-prem Helm hook and its docs come with it.
6. **Offering packages over OpAMP**, with the `internet` or `deployment` source setting. This is
   later, and has its own plan.
