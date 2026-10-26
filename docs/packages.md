# Packages

A server can offer an agent packages to download and install: addons, or a new version of the
agent itself. The client does the bookkeeping the spec asks for (what to fetch, checking the files,
reporting each package's status, resuming after a crash); the application installs.

## What the spec says, and what it leaves open

- The server offers every package the agent should have (`PackagesAvailable`), each with a
  version, a hash, and a file (URL, content hash, optional signature and headers), plus an
  aggregate hash of them all. The agent downloads the packages it doesn't have or has in another
  version, skips files it already has, and deletes packages no longer offered.
- The agent reports every package it has or is working on (`PackageStatuses`): the version and
  hash it has, the version and hash offered, and INSTALL_PENDING, DOWNLOADING, INSTALLING,
  INSTALLED or INSTALL_FAILED, with the aggregate hash of the last offer it received.
- Left open: the content hash's algorithm, what happens on a crash mid-install, retries, and
  whether a failed version is tried again.

## What the client does

- **Declaring.** With `accept_packages` (and storage: without it a crash would forget what the
  agent has) the client declares AcceptsPackages and ReportsPackageStatuses.
- **One package at a time.** On an offer with a new aggregate hash the client works through the
  packages in name order: first those to install, then those to remove. A version that failed
  is not tried again while it is the one offered: offering another version (even the same one
  again afterwards) gives a fresh try.
- **Downloading.** The client downloads the file (HTTP GET with the offered headers) to a file of
  its own, computing its SHA-256 as it goes, up to `max_package_bytes` and `download_timeout_ms`.
  A file whose SHA-256 differs from the content hash fails the package at once, INSTALL_FAILED (it
  is the wrong file: TCP and TLS rule out damage on the way). A download that fails is tried three
  times, 10 s apart; if every try fails, the network did, not the file: the package fails for now
  (below) and is tried again later. A file the agent already has (the content hash
  of what it installed) isn't downloaded again: the package just takes the new version. Files
  go beside the state file (its path and `.downloads`), for the agent's user only (see
  [connection-settings.md](connection-settings.md#secrets-at-rest)); what is there at start is deleted.
- **Malformed files.** Before asking for a download the core checks the URL and headers, as it
  does an OpAMP endpoint ([connection-settings.md](connection-settings.md#what-the-core-refuses)):
  an absolute `http` or `https` URL with a host, no userinfo, whitespace, control characters or
  fragment, at most 8000 octets; headers that are tokens with values of printable ASCII, none the
  HTTP stack sets itself. A package that fails is INSTALL_FAILED at once, with the reason ("the
  download URL has no host"; a header's value is never in it), and not tried again: it would be
  malformed next time too. A relative URL is not resolved against the OpAMP server: that would
  send the offered headers somewhere the server never named. The next package is worked on at
  once (RULE-CLIENT-12).
- **Installing.** The application gets `InstallPackage` with the package (name, type, version,
  hash, signature) and the file's path, installs it (checking the signature is its job), and
  answers `package_installed` (or with an error, saying how it failed: below). The client deletes
  the file after the answer: move it if you keep it.
- **Removing.** The application gets `RemovePackage` for a package no longer offered, removes
  it, and answers `package_removed`, with an error if it couldn't: the package is then reported
  failed, and not removed again until the server offers it and drops it again (unless it failed
  for now).
- **How it failed.** An install's or a removal's error says which of these it is:

  | Failed | Means | The client |
  |---|---|---|
  | For good (the default) | it won't work: the file, the version | reports the version failed (INSTALL_FAILED, with the error) and waits for the server to offer another |
  | For now | it may work later: an outage, a timeout, the agent busy; nothing was changed | reports nothing failed (INSTALL_PENDING), and tries again after 30 s, doubling each time in a row to at most 10 minutes, while the other packages go on; a restart tries at once |
  | Leaving what it has unknown | it failed and can't tell what it left (a rollback that failed) | as for good, and what the agent has of the package is uncertain from then on (see "A crash") |

  So far only Go's answer says which (wrap the error with `opamp.FailedForNow` or
  `opamp.FailedLeavingUnknown`); every other client's error is a failure for good.
- **No offer.** A package offered with no hash names no version: it is never installed or
  removed, and is reported INSTALL_FAILED. An offer with no aggregate hash is told apart from the
  last by its packages; the statuses report the aggregate hash the server sent, empty too.
- **The server's TLS settings stay with the server.** The client certificate configured for the
  OpAMP server is presented only when a file is at the server's own origin (its scheme, host and
  port): a file anywhere else (a CDN, storage) is fetched without it, still checked against the
  CAs configured (Go also stops skipping verification there, if its TLS config skips it for the
  server). Each redirect hop is judged again. This holds in both modes in every client. An
  HTTP client of the application's own is used as it is, except in Go when its transport is an
  `*http.Transport`: a copy of it, without the certificates, fetches files elsewhere. In core mode
  the core says, for each TLS handshake, whether it is with the server's own origin, and keeps a
  connection for the one or the other; only the server's gets the client certificate (and Go's
  skipped verification). TLS to an https:// proxy in core mode gets neither.
- **A crash.** Before the application is told to install or remove a package, the client saves
  that the package is uncertain: the application may change it. After a restart, an uncertain
  package is downloaded and installed again, or removed again, whatever the saved record says
  the agent has. Installing and removing must therefore be idempotent. Everything else resumes
  from what was saved; downloads start over.
- **A new offer while working.** The package in progress finishes; a download of a version no
  longer wanted is discarded rather than installed. A package the agent doesn't have and that is
  no longer offered is forgotten.
- **Reporting.** Statuses go out as they change (and in full reports), from memory: a restarted
  agent reports what it saved, and the server sends the offer again if its hash differs.

## In each client

| Client | Turn on | Install | Remove |
|---|---|---|---|
| Go | `Config.StateFile`, `Config.AcceptPackages` | kind `InstallPackage`: `PackageName`, `Package`, `Path`; answer `PackageInstalled` (an error wrapped by `FailedForNow` or `FailedLeavingUnknown` says how it failed) | kind `RemovePackage`; answer `PackageRemoved` (the same) |
| Node.js | `stateFile`, `acceptPackages` | `installPackage`: `name`, `package`, `path`; answer `packageInstalled` | `removePackage`; answer `packageRemoved` |
| Python | `state_file`, `accept_packages` | `install_package`: `package_name`, `package`, `path`; answer `package_installed` | `remove_package`; answer `package_removed` |
| Java | `stateFile`, `acceptPackages` | `Notification.InstallPackage`: `name`, `pkg`, `path`; answer `packageInstalled` | `Notification.RemovePackage`; answer `packageRemoved` |
| .NET | `StateFile`, `AcceptPackages` | `OpAmpNotification.InstallPackage`: `Name`, `Package`, `Path`; answer `PackageInstalled` | `OpAmpNotification.RemovePackage`; answer `PackageRemoved` |

Downloads are limited by `max_package_bytes` (1 GiB) and `download_timeout_ms` (10 minutes). No
client follows a redirect itself: the core decides (below).

## Redirects

The client reports every download's response, whatever its status: it
writes the body to the file as it would the package, and reports the status and the SHA-256. For
a 3xx it adds the `Location`, resolved against the URL it requested by its own URL library, and
that URL's origin (scheme, host, port) as the library reads it. It judges nothing. The core
decides, and discards the file of anything but the package:

- **A 2xx is the file**, checked against the content hash as always.
- **301, 302, 303, 307 and 308 are followed**, at most 5 times in one attempt. One hop is the
  common case (a download URL that sends the agent to storage or a CDN); signed URLs take two.
  Java's `HttpClient` stops at 5, the strictest of our clients' stacks, so no client of ours
  followed more before; browsers allow 20.
- **Never from https to http.** Once a hop is https, every later one must be.
- **Only to a URL the core would take from the server** (as for the offer's own URL,
  [connection-settings.md](connection-settings.md#what-the-core-refuses)),
  and only if the core reads its origin as the client did. The client resolves the Location, so
  a relative one works; the core then checks the result, and a client library that read the URL
  differently (a parser differential) fails the download rather than follow it.
- **The offer's headers go only to the file's own origin**: the scheme, host and port of the
  offer's URL, default ports spelled out or not, names in any case, addresses as addresses.
  Elsewhere the request carries none (the offer's credentials don't leak to a storage host).
  Back at the file's origin, they go again.
- **Anything else fails the download, with the status in the reason** ("HTTP 404"). A status
  counts as a failed attempt (a 503 may pass: three attempts, 10 s apart, each from the offer's
  URL); a redirect the core won't follow fails the package at once ("the download failed: a
  redirect from https to http"): it would happen again.

Each hop is a new download, with an id of its own.

## How it is checked

- The core is checked on its own before it is released, against a model of the flow with two
  packages, changing offers, failed and corrupt downloads, failed installs, failed saves and
  crashes at any point. What holds: a record saved as certain is what the agent has; every
  package the agent has is reported; only verified files are installed; the agent ends up with
  what the server offers (or reports the version that failed). The aggregate hash may be reported
  before the offer is saved: a restarted agent reports what it saved, and the server sends its
  offer again. Beyond the model: a file whose content hash is empty is installed unchecked, a file
  the agent has takes the new version without a download, and packages are worked on installs
  first, by name. The redirect decision is held, for every status, hop count and target, to the
  rules above.
- Each client's downloads, in its own tests: the hash, the size limit, a failed status,
  redirects, discarding.
- The conformance scenario `packages`: three packages, one with the wrong content hash, then a
  new version of one and another dropped. On every client over both transports the good files
  reach the application with the offered credentials and their hash, the bad one never does
  and is reported INSTALL_FAILED, the upgrade is installed, the dropped package is removed, and
  every offer's hash comes back in the statuses (RULE-CLIENT-11).
- The conformance scenarios `packages-redirects` and `packages-redirects-refused` (every client,
  over both transports) put the files behind real redirects: to the same origin (absolute,
  path-absolute, `../`), to another port and back, from http to https, a chain of exactly 5 using
  every status followed, and a file on an https origin; and redirects to refuse: a sixth, https
  to http (at once, and after an upgrade), a Location with a space in the host, another scheme,
  or userinfo. The allowed ones are installed with the file at the chain's end; the others are
  INSTALL_FAILED at once, and nothing reaches their target; the offer's `Authorization` reaches
  every request at the file's origin and none elsewhere.
- The conformance scenarios `hostile-packages` and `hostile-packages-mix` offer packages with
  malformed URLs and headers beside a good one, which must still be installed; no download is
  ever asked for with a URL or header the core refuses.
- `crash-mid-download` and `restart-mid-download` ([state-file.md](state-file.md#how-it-is-checked)).
