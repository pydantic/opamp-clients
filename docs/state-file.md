# The state file

The core's saved state (the `store` effect, [how-it-works.md](how-it-works.md#the-interface)) is
the only copy of the credentials the server offered, of the instance UID it assigned, and of what
the agent has installed. Every client writes it the same way, and is held to it the same way.

## The recipe

A save of `data` to the state file `S`:

1. delete `S.tmp` if it is there (left by a save that failed, or a run that crashed: it could be
   anyone's file, with any mode);
2. make `S.tmp` new (exclusively: `O_EXCL`, create-new), readable by the agent's user only
   (mode 0600; on Windows, an ACL of the user alone in Java and .NET);
3. write `data` to it, and flush it (`fsync`);
4. rename it over `S`;
5. flush `S`'s directory, so the rename reaches the disk (not on Windows, where a directory can't
   be flushed);
6. only then feed the core `stored`; on any failure, `store_failed`, and delete `S.tmp`.

On macOS `fsync` only hands the data to the drive, which may keep it in its cache: steps 3 and 5
flush with `fcntl(F_FULLFSYNC)`, falling back to `fsync` where the file system refuses it. Go
(`File.Sync`), Node (libuv) and Java (`FileChannel.force`) get that from
their runtimes, as .NET does for the file (`Flush(true)`); Python's `os.fsync` doesn't, so
`_files.py` calls `fcntl` itself, as .NET does for the directory (`FileOps.cs`).

Saves run beside the client's loop, one at a time, in the order the core asked for them, so a slow
disk holds up no heartbeat, answer or stop, and an older save never lands after a newer one. A
client started on the same state file in the same process waits for a save still writing.

A save has no deadline. The core can't give up on one that hangs (a disk that stopped answering,
a network file system gone away): the write could still land, after a newer save, and put older
state on disk. So it waits, saves nothing else meanwhile, and keeps what changed owed. After 30 s it
tells the application, once (the notification `save_stalled`, with `stalled` and how long the save
has run), and tells it again when the save ends at last (not `stalled`); then it saves what is
owed. A disk that hangs is for the application, or its operator, to see to.

A download's file (`S.downloads/...`) is made new too, written, and flushed before its result is fed
to the core; it is deleted when the download fails or is cancelled (by the code that writes it,
never under a write still going), or when the core says to discard it. The downloads left by a run
before are deleted at start. A file that was already there is never written over, handed to the
application, or deleted by a download that couldn't make its own.

The clients: Go `go/opamp/transport.go` (`saveState`, `fetch`), Python
`python/src/pydantic_opamp_client/_executor.py`, Node `node/opamp/src/executor.ts`, Java
`java/opamp/src/main/java/dev/pydantic/opamp/StateFile.java`, .NET
`dotnet/Pydantic.OpAmp.Client/StateFile.cs`.

## How it is checked

- **The file records.** With `OPAMP_CORE_TAP` set, each shell records every file operation it
  carries out for the core in the tap, after it succeeded. `opamp-tap`, a tool that comes with the
  core release, replays them on a model of a disk that crashes at any point, losing
  what wasn't flushed: a file's data since its last flush (all of it, part of it, a write cut
  short), and any prefix of a directory's changes since its last flush. That is what LazyFS's
  `clear-cache` fault does, without FUSE, and what ext4 can do after a power loss. At every point
  of every conformance run:
  - `torn`: every state a crash could leave the state file in is whole: what it held at start,
    or the whole data of one of the core's saves;
  - `not-durable`: once the shell fed the core `stored`, none is older than that save (stored
    before the save reached the disk, or an older save landing after it);
  - `reused-file`: every file written is one the shell made new.

  So the mutants (no file flush, no directory flush, a write in place) fail the step that says
  stored, whether or not anything crashed then.
- **Real crashes.** The conformance scenarios `crash-mid-save` and `crash-mid-download` kill the
  driver at a seeded moment, write what a crash of the machine could have left (`opamp-tap
  crash`), and restart it there: `crash_state_consistent` asks that it be whole, no older than
  the last save answered stored, and exactly what the restarted client's core starts from; the
  restarted client must download again into a file of its own (`packages_installed_verified`).
  `restart-mid-download` restarts a client in process, on the same state file, while the first
  lingers.
- **Real crashes on a real file system** (nightly, `conformance/lazyfs/run.sh`). The same crash
  scenarios, with every driver's state file on a mount of [LazyFS](https://github.com/dsrhaslab/lazyfs)
  of its own: a FUSE file system that keeps what isn't flushed in a cache of its own. At the
  seeded moment the runner kills the driver and LazyFS drops every byte not flushed
  (`clear-cache`); in `crash-mid-save`, on a third of the seeds, LazyFS instead tears the first or
  second write of the state's temporary file in two, keeps one half on the disk and dies (the
  crash is then, if that write comes before the moment), as a machine losing power
  in the middle of a write would. The file system is then mounted again on the same disk, and
  `opamp-tap judge` says what the state file holds against the run's tap, for
  `crash_state_consistent`: whole, no older than the last save answered stored, and what the
  restarted client's core starts from. One driver per runtime (go-core-platform,
  node-core-platform, python-core-platform, java-core-platform, dotnet-core-platform), 25 seeds a night (200 crash points each); then the scenarios that save
  (`remote-config-restart`, `uid-assigned-restart`, `request-uid-restart`, `credential-offer`,
  `packages`, `restart-mid-download`) run once on LazyFS. A failure names its seed:
  `LAZYFS_SEED_BASE=<seed> LAZYFS_SEEDS=1 conformance/lazyfs/run.sh` replays it.

  It runs in a container (`conformance/lazyfs/Dockerfile`: LazyFS and a toolchain per runtime,
  the Java and .NET drivers run natively there, `conformance/lazyfs/native.toml`), which needs
  `/dev/fuse` from the host (the `fuse` module loaded), `CAP_SYS_ADMIN` to mount, and no AppArmor
  confinement of `mount`: `docker run --device /dev/fuse --cap-add SYS_ADMIN --security-opt
  apparmor:unconfined`. On runners whose containers can't have that (a Docker-in-Docker daemon
  that isn't privileged, or a node without `/dev/fuse`), `run.sh` mounts nothing, says why, and
  the job passes as skipped (`LAZYFS_REQUIRED=1` makes that a failure). Docker Desktop and OrbStack
  give it locally.

  LazyFS keeps file data only: a create, a rename or an unlink reaches the disk under it at once.
  So it catches a save that doesn't flush its temporary file (the file renamed over the state is
  empty or short after the crash) and one that writes the state in place (torn), but not one that
  doesn't flush the directory: the model of the core taps does (`not-durable`).
- **Faults, per SDK.** Each shell's file operations go through a seam its tests replace: saves
  that fail (an error, a write cut short), are slow, or land but report failure; a downloads
  directory that can't be made; a discard that fails. The client survives, answers each save
  once, stops within its bound, and the next client starts from a whole state file.

## Encrypted at rest (Go)

The state holds the credentials a server issued, so the Go SDK can seal it: `opamp.Config.StateSealer`
seals each save and opens the file at `Start`, and `go/statekey` provides one with AES-256-GCM
(`OPAMPSK1`, a 12-byte nonce, then the ciphertext and tag). The data key comes from the OS:

| `state_key` | Where the key is | Made by |
| --- | --- | --- |
| `systemd-creds[:NAME]` | `$CREDENTIALS_DIRECTORY/NAME` (default `opamp-state-key`), from `LoadCredentialEncrypted=`: encrypted to the host, and its TPM if it has one | `systemd-creds encrypt`, at install |
| `keychain` | the macOS Keychain, a generic password `opamp-state-key` named after the state file | the agent, the first time |
| `dpapi` | `<state file>.key`, encrypted with Windows DPAPI for the agent's user | the agent, the first time |
| `file:PATH` | a key file readable by the agent's user only (refused if group or others can read it) | the agent, the first time |
| `auto` | `keychain` on macOS, `dpapi` on Windows, `systemd-creds` elsewhere | |

A key that can't be found or made stops the client at the start, rather than it writing the
credentials in the clear. A state file written before a key was set (no `OPAMPSK1`) is read as it
is and sealed at its next save: whoever can write the file could read the key anyway, so this
protects what is on the disk (a backup, an image, a stolen disk), not the file against its owner.

On systemd:

```sh
head -c32 /dev/urandom | systemd-creds encrypt --name=opamp-state-key - /etc/otelcol/opamp-state-key.cred
```

```ini
[Service]
LoadCredentialEncrypted=opamp-state-key:/etc/otelcol/opamp-state-key.cred
```

## Not covered

- Windows: a directory can't be flushed, and the ACLs and `MoveFileEx` aren't modelled. The seamed
  tests run where each SDK's Windows CI does.
- A disk that lies (a flush that returns before the data is on it): no client can tell.
- On a real file system, a lost rename (a directory not flushed): LazyFS doesn't drop metadata;
  only the core taps' model of the disk checks it.
