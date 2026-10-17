# Publishing the SDKs to their registries

The [Registries workflow](../.github/workflows/registries.yml) publishes an SDK release on an
`sdk/vX.Y.Z` tag. It stamps every manifest with the version, builds every package, and checks
that none holds the cores' sources (`scripts/sdk_release.py`, `scripts/native_dist.py`). Then each
registry's job uploads, but only once the repository variable `SDK_REGISTRIES` names it, for
example `pypi npm nuget`. Until then each job is a dry run that lists what it would publish. On a
PR that changes the workflow, all of it runs as a dry run at version 0.0.0, outside the registries'
environments, so it waits on no approval.

| Registry | What goes there | Before its name goes in `SDK_REGISTRIES` |
| --- | --- | --- |
| PyPI | the client, its native core (one wheel per platform, no sdist), otel | Add a trusted publisher on PyPI for each project: this repository, workflow `registries.yml`, environment `pypi`. No token. |
| npm | the client, otel | Make the scope on npm, and add a trusted publisher for each package: this repository, workflow `registries.yml`, environment `npm`. No token. |
| Maven Central | the client, the Java agent extension (with its `app-settings` jar) | Verify the groupId's namespace on Central, and make a Central token and a signing key. The poms also need `developers` and `scm`, plus sources and javadoc jars. Until then, the job fails if it is enabled. |
| NuGet | the client, its native core, the hosting package (`WithOpAmp()`), the automatic instrumentation plugin | Reserve the id prefix on nuget.org, and set an API key as the `nuget` environment's secret `NUGET_API_KEY`. |
| Go | nothing to upload | Push a tag per module: `go/vX.Y.Z` for the client, then `go/otel/vX.Y.Z`, `go/opampextension/vX.Y.Z` and `go/opampsupervisor/vX.Y.Z` on a commit whose `go.mod` files require that client version. The module proxy serves them, and caches each tag for good. The `replace` in each `go.mod` only applies inside this repository; the Supervisor is installed from its packages, since `go install` refuses a module with one. |
| crates.io | not published | The Rust client isn't published. |

The names are Pydantic's: `pydantic-opamp-*` on PyPI, `@pydantic/opamp-*` on npm, `dev.pydantic`
on Maven Central and `Pydantic.OpAmp.*` on NuGet. Each scope or prefix must be ours on its registry
before the first upload: a name on a registry can't be taken back.
