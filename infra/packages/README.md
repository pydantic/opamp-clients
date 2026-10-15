<!-- Copyright (c) 2026 Bill Easton. All rights reserved. -->
<!-- SPDX-License-Identifier: LicenseRef-All-Rights-Reserved -->

# The package publishing side, in Pulumi

This program makes what the Packages workflow (`.github/workflows/packages.yml`) publishes to,
as planned in [`docs/package-hosting.md`](../../docs/package-hosting.md):

- the publishing bucket, readable by anyone, served at `https://packages.pydantic.dev` through
  Cloud CDN with a managed certificate, keeping the `Cache-Control` that `publish.sh` sets;
- the DNS record, only when `dnsZone` names a Cloud DNS zone (otherwise point the domain at the
  `address` output by hand);
- a service account that can write that bucket alone, reached keylessly by the workflow through
  Workload Identity Federation, from this repository's `packages` environment only;
- the `packages` GitHub environment, entered by release tags only (`go/opampsupervisor/v*`,
  `bundle/v*`), with the variables the workflow reads.

The signing key is not here: Pulumi state would hold it. It is set by hand as the
`PACKAGES_SIGNING_KEY` secret.

Nothing has been created yet. `pulumi up` waits on the GCP project and the state backend
(the plan's open questions).

## Settings

All optional, set with `pulumi config set <name> <value>`:

| Name | Default |
| --- | --- |
| `bucket` | `pydantic-opamp-packages` |
| `location` | `US` |
| `domain` | `packages.pydantic.dev` |
| `repository` | `pydantic/opamp-clients` |
| `dnsZone` | none, so no record is made |

## Tests

`go test ./...` runs the program against Pulumi's mocks, with no cloud and no credentials. The
tests check who can read and write the bucket, what the identity provider trusts, how the domain
is served, and what the workflow is told. CI's `infra` job runs them with `go vet` and `gofmt`.
