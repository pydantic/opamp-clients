# Security

## Reporting a vulnerability

Please don't open a public issue or pull request. Report it privately on this repository's
[Security tab](https://github.com/pydantic/opamp-clients/security) ("Report a vulnerability"), as
[Pydantic's security policy](https://github.com/pydantic/.github/blob/main/SECURITY.md) says.

Say which package and version (or commit) is affected, what an attacker needs, and how to
reproduce it. A minimal reproduction helps us fix it faster.

## What's covered

Everything this repository releases: the OpAMP clients and SDK integrations for Go, Node.js,
Python, Java and .NET, the Supervisor and its installers, the collector extension and processor,
and the compiled core every package carries.

Some things are by design, not vulnerabilities:

- An OpAMP server manages what its agents run. What a server may do to a host running the
  Supervisor is limited by the host's local policy; the
  [Supervisor's packaging README](go/opampsupervisor/packaging/README.md#what-the-server-can-do-to-this-host)
  says what it can and can't do.
- Setting `OTEL_OPAMP_DISABLED=true` takes an application out of management.

## What happens next

We confirm the report and tell you what we found, fix it in private, and release the fix as a new
version of each affected package. Then we publish a GitHub security advisory here (with a CVE when
it applies) naming the affected packages and versions, and credit you unless you'd rather we
didn't.

## Supported versions

Before 1.0, only the latest release of each package gets fixes; a fix ships as a new release, not a
backport. [CHANGELOG.md](CHANGELOG.md) lists what each release changes.

## Dependencies

Every lockfile and manifest here is checked against the [OSV](https://osv.dev) database on every
pull request and push to main, and every night, by `scripts/deps_scan.py`. A finding fails the
run: we update the dependency, or, when no fix exists yet or it can't apply to how we use it,
ignore it for at most 90 days with the reason written in `osv-scanner.toml`.
