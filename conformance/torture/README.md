# Torture

The conformance suite tests one fault at a time for a few seconds. The torture harness runs one
client for as long as you like — ten minutes locally, an hour nightly — through a seeded
schedule of everything that can go wrong at once, and checks that it held up.

```
T="uv run --quiet --no-project --python 3.13 --with psutil conformance/torture/torture.py"
$T --client go-core --transport ws                          # 10 minutes, seed from the clock (printed)
$T --client go --transport http --minutes 60 --seed 42      # an hour; the same seed gives the same schedule
$T --client go-core --transport ws --server rust            # against the Rust server instead of the Go one
```

Each run lives in `conformance/out/torture/<client>-<transport>-<seed>/`: the schedule, the
server, driver and harness logs, and `result.json` with every verdict and resource sample.

## What happens

The run is a series of epochs (120 s by default): 45 s of chaos, then a quiet stretch in which
the client must recover. During the chaos, from the seed:

| Where | Faults |
|---|---|
| server | refuses requests (429 / 503 with Retry-After), rejects messages (413 / BAD_REQUEST), UNAVAILABLE with retry info, asks for the full state, assigns new UIDs, offers configs, restarts and forgets the agent, sends Close frames, drops TCP connections |
| network | a proxy in front of the server adds latency (up to 2 s), splits writes into 1-64 byte pieces, stalls, resets every connection (RST), refuses new ones |
| process | the driver is frozen with SIGSTOP (or `docker pause`) for up to 10 s; in half the chaotic stretches, a client of ours is killed (SIGKILL, or `docker kill`) and started again at once on its state file, with the status the application had reached |
| application | the status changes every few seconds |

The server's faults fire per message with a probability drawn from the seed (`-seed`), so a
schedule replays; the network and the client's own timing make runs differ in detail. Every
run reports which faults actually hit the client, not just which were planned.

### Crashes

A client of ours that keeps its instance UID (`instance_uid_kept` in `clients.toml`) runs with a
state file (`DRIVER_STATE_FILE`) and makes its own UID (`DRIVER_UID=none`), and in half the
chaotic stretches, at a moment the seed picks (from a generator of its own, so the seed's other
faults are the same with crashes or without), the harness kills it. It resets the proxy's
connections at once, so that nothing the dead process sent reaches the server afterwards (a real
crash could deliver what a middlebox still held; this keeps each message's run plain), judges what
the kill left of the state file against the run's core tap (`opamp-tap judge`), and starts the
driver again for the rest of the schedule, with `DRIVER_FIRST_CHANGE` set to the last status the
application made: an application that restarts reports where it is, not where it began. Each run
taps its core to a file of its own, and a killed run's tap may end in a call cut off. The
resources of a process just started are not sampled for its first 30 s. `--no-crashes` leaves
them out; the conformance suite's `crash-mid-*` scenarios crash the machine, losing what wasn't
flushed, which torture doesn't.

Across a crash, the checks below allow for what the client could not know: the run after it has
never heard of a status the server rejected before, a Close frame the dead process never got to
answer is not held against it, and a UID assigned in an answer with a crash before the next
message is `uid_kept_across_crash`'s to judge.

## What must hold

`invariants.py`, each tested to pass one trace and fail another (`test_invariants.py`):

| Invariant | Meaning |
|---|---|
| `status_never_goes_back` | in sequence order, the status a client reports never returns to an older one, and is never one the application hadn't set yet |
| `converges_when_quiet` | at the end of every quiet stretch, the server holds the application's latest status, even after a restart that lost it |
| `alive_when_quiet` | once quiet, the client talks to the server at least every two heartbeat intervals |
| `no_storm` | connections plus messages stay under 40 in any 10 s, however the server misbehaves |
| `calm_when_quiet` | once quiet, about one message per heartbeat interval (at most two, and three more, in each window's checked part), however the server answers: one that asks for the full state in every answer gets no message per round trip (#81) |
| `resources_bounded` | memory, open files and threads in the last third of the run are not far above the first (below); a JVM's memory is judged as the heap after a GC and its non-heap memory, a .NET client's as the GC heap live after a forced GC and its non-GC memory |
| `server_config_converges` | (Rust server) at the end of every quiet stretch, the server knows the agent runs the config it wanted |
| `uid_kept_across_crash` | after a crash, the client's first message carries the instance UID its last one did, or one the server assigned it since: never a new one, nor one it had given up for an assigned one |
| `state_kept_across_crash` | after a crash, the state file is whole and holds the last save the core was told was stored (or a later one), and the next run starts from exactly it (the conformance suite's `crash_state_consistent`) |

It also runs the conformance checks that hold for any trace (instance UID, capabilities,
agent_disconnect last, goodbye before reconnect, rejected status not resent, assigned UID
adopted, Content-Type, Close answered, driver survives). Those know nothing of the network
faults, so they see the trace without what the network kept from the client: a Close frame sent
while the proxy stalled or the driver was frozen for all of the server's wait for the answer, or
while the proxy was slow (a latency of 800 ms or more, or data sent in pieces with a pause
between: messages queue behind it, and the answer can reach the server after its 5 s wait); and
a UID assignment whose reply a reset, a stall or a slow proxy held before the next request, when
the driver (which logs the UIDs it gets) never got it.

### Memory

Every 5 s the harness samples the client's memory, open files and threads (`psutil` for a
process, `docker stats` for a container: its memory and PIDs). From the end of the first quiet
stretch on, `resources_bounded` fails if the median of the last third of the samples is above
max(first third x 1.25, first third + 20 MB) for memory, or the first third + 20 for files and
threads.

A container's memory is a poor measure of a JVM's: the heap is committed up front, and the
container's memory follows eden's high-water mark as those pages are touched for the first time,
while JIT-compiled code and metaspace grow as the JVM warms up (java-core: 115 -> 152 MB in an
hour with the committed heap flat at 254 MB, 9-14 MB live after a GC, and JIT code and metaspace
level after 20 minutes). So for a client whose driver runs `java` in a container (`java`,
`java-core`, `java-core-rust`), the harness starts the JVM with native memory tracking
(`JAVA_TOOL_OPTIONS=-XX:NativeMemoryTracking=summary`), and once a minute, after the container's
sample, forces a GC and reads the heap and native memory (`docker exec ... jcmd 1` with `GC.run`,
`GC.heap_info`, `VM.native_memory summary`). Each such sample in `result.json` carries `heap_mb`,
the heap still in use after the GC, and `nonheap_mb`, what the JVM has committed besides the heap
(the container's memory less the committed heap, without native memory tracking).
`jvm_memory.py` judges them by the same rule:

- the heap after a GC, from the end of the first quiet stretch;
- the non-heap memory from a third of the way into the run on: the first third is the JVM's own
  warm-up.

Open files and threads are judged as for any other client. A JVM probed fewer than six times
after warm-up is judged by its container's memory, like every other client, whose judgement this
doesn't change.

A container's memory is a poor measure of a .NET client's too: the runtime sizes gen0's budget
from the L3 cache, and a client that allocates little takes most of an hour to fill it, so gen0
is hardly ever collected and the container's memory climbs with dead objects (dotnet-core http,
Nightly run 38112161610: 65 -> 90 MB against the Go server, and it failed against ours; no leak
by dotnet-counters and dotnet-gcdump). So for a client whose driver runs `dotnet` in a container
(`dotnet`, `dotnet-core`, `dotnet-core-rust`), once a minute, after the container's sample, the
harness forces a full blocking GC in it and reads what survived, through the runtime's
diagnostics port (open by default) with Microsoft's own tools: `dotnet-gcdump collect -p 1` (a
heap dump, which the runtime takes by inducing the GC and walking the live objects) and
`report`, then `dotnet-counters collect` for the GC's committed bytes after it. The tools
(pinned, installed once in the .NET SDK image under `conformance/out/dotnet-diag`) are copied
into the container and run there on its own runtime as the driver's user; the driver's command
and its runtime settings (GC ones included) are left as they are. Each such sample carries
`heap_mb`, the GC heap live after the GC, and `nonheap_mb`, the container's memory after it less
what the GC has committed. `dotnet_memory.py` judges them as `jvm_memory.py` does a JVM's: the
GC heap from the end of the first quiet stretch, the non-GC memory from a third of the way in
(the runtime's own warm-up: JIT code). A .NET client probed fewer than six times after warm-up
(the tools couldn't be installed: `dotnet_probe_unavailable` in `harness.jsonl`) is judged by
its container's memory.

## Against the Rust server

`--server rust` runs the same client against the Rust server (where it is built from the core's
sources: `run.py`'s `SERVERS`), which decides for itself what to offer. The Go server's protocol
faults exist to test clients, so they are left out. The server faults are the events a real server has: it restarts and forgets every agent,
sends Close frames, drops TCP connections, and is overloaded for a while (503 with Retry-After,
UNAVAILABLE with retry_info). The operator changes the desired config
mid-chaos, sometimes to none. The network, the pauses and the status changes are unchanged.
Runs go to `conformance/out/torture/rust-server/`.

The Rust server says in every answer what it holds (`held_change`, `held_config`), and the
invariants trust that over what it received. A stale message the server rightly ignored doesn't
count as held. Three more checks run:

- `server_config_converges`: at the end of every quiet stretch, the server knows the agent runs
  the config it wanted at the stretch's start;
- the conformance suite's checks of the server: one offer per connection, APPLIED recorded, and
  every assigned UID a UUID v7.

## Results

Our clients must pass every run, the five WebAssembly SDKs in both modes (`go-core-rust`,
`node-rust`, `python-core-rust`, `java-core-rust` and `dotnet-core-rust` are Rust mode). The
nightly job runs an hour per client and transport; the upstream clients' runs are recorded, not
required to pass. A MUST failure of an upstream client
that the conformance baseline of the same server lists (failing or flaky), or that
`baseline.toml` here lists, prints as `known` and doesn't fail the run; anything else does, so a
red upstream job means something new. A failure of one of our clients, or of a check of the
server, is excused only by an entry of `baseline.toml` that gives its `reason`: a bug of ours
owned and tracked elsewhere, the entry removed when the fix lands.

What torture showed about the upstream clients, beyond the conformance baseline:

- **.NET, WebSocket**: never reconnects after a dropped connection. One TCP drop 18 s into a
  four-minute run left it offline for the remaining 220 s.
- **Java, WebSocket**: reconnects after about 30 s, but then sends an empty message and
  ignores the server's ReportFullState, so status set while it was disconnected only arrives
  with the next change. It sends no heartbeats over WebSocket. (We use Java over HTTP.)
- Python, Java (HTTP) and .NET (HTTP) fail only what they fail in the conformance suite:
  missing ReportsStatus, a rejected status resent, agent_disconnect sent twice.
- Go passes every run so far.
