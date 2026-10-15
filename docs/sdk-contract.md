# The SDK contract

What an application can rely on from every OpAMP client in this repository (Go `go/opamp`, Python
`python/`, Node.js `node/opamp`, Java `java/opamp`, .NET `dotnet/Pydantic.OpAmp.Client`): when its
callback runs, what a callback that fails does, which calls are allowed from where, and stop. The
five run one core ([how-it-works.md](how-it-works.md)), so they keep one contract; where an SDK
differs, this page says how.

This is the application's view. The precise rules, what tests each and the breaks known today are
in [spec/shell-contract.md](../spec/shell-contract.md) (RULE-SHELL-01 to -03, -07 and -10).

## The callback

The application gives the client one callback when it starts it. The client calls it with each
notification: a remote config offered, connection settings, a custom message, a package to
install, the instance UID changed, `stopped`, and the rest.

- **One at a time.** Two calls of the callback never overlap, on any thread. A callback may
  keep state of its own without a lock, as long as only the callback touches it.
- **In order.** Notifications come in the order the core emitted them.
- **Never before `start` returns.** A notification the client
  has as it starts (a remote config saved before a restart is handed over again at once) is held
  until `start` has returned. The caller may still not have stored what `start` returned when the
  first callback runs: answer through the client the callback is given (Go, .NET's two-argument
  overload), or through something the caller completes once it has the client (Java: a
  `CompletableFuture<OpampClient>`).
- **Never after `stopped`.** `stopped` is the last notification, and comes exactly once, whether
  the application stopped the client or it stopped on its own.
- **Not re-entered.** A call made from the callback (an answer, a status, stop) is queued: it
  reaches the core after the step that called the callback has finished. No call ever runs the
  callback again from inside itself.
- **Answered first.** What the callback asks before it returns is the application's answer to that
  notification: it reaches the core before anything the client takes in next (what the server
  says, a connection that opened, a timer), behind only the calls the application made before it.
  So a remote config answered APPLIED in the callback is never reported APPLYING after it: a
  saved one handed over again at a restart is APPLIED in the first message over WebSocket. An
  answer given later, from another thread or task, takes its place among what the client hears
  meanwhile, and the server may see APPLYING first.
- **Keep it quick.** The client does nothing else while its callback runs: no heartbeat, no
  answer to the server, no stop. Long work (applying a config that takes seconds) belongs on
  another thread or task, answering the client when it is done. A callback that blocks anyway
  holds up only its client (once it returns the client goes on, and a stop asked for meanwhile
  returns within the stop timeout of then), except where the callback runs on a thread others
  share: Python's and Node's event loop, where nothing else on that loop runs until it returns.

| SDK | Where the callback runs | Before `start` returns? | The client, in the callback |
|---|---|---|---|
| Go | the client's goroutine | never | the `*Client` argument |
| Python | the event loop the client was started on | never | the application's own reference (`start` is synchronous: the callback runs on a later turn of the loop) |
| Node.js | the event loop | never | the application's own reference (the first callback is a microtask: it runs once the code that called `start` yields) |
| Java | the client's thread (`opamp-client`) | never | a future the caller completes with what `start` returned |
| .NET | the client's loop (a thread-pool task, one at a time) | never | the `OpAmpClient` argument of `Start(options, status, (client, n) => ...)` |

## A callback that fails

A callback that throws, raises or panics **stops the client**. The process is not taken down, and
nothing is left running: the client ends as after a stop, without the goodbye (no
`agent_disconnect`).

- The application hears **`stopped` with the error, exactly once.** If the callback fails again
  on that `stopped`, the second failure is dropped; the first is the one reported.
- `stop` (called before or after) returns or raises **that error**, every time it is called.
- Without a failure, `stopped` comes once too, with no error, and `stop` succeeds.

| SDK | What counts as a failure | How it is reported |
|---|---|---|
| Go | a panic (recovered on the client's goroutine) | `Notification{Kind: Stopped, Err: ...}`; `Stop` returns the error |
| Python | any exception the callback raises (`BaseException`) | `stopped` with `error`; `await stop()` raises it |
| Node.js | anything thrown (a thrown `undefined` becomes an `Error`), and the rejection of the promise an async callback returns (it isn't waited for) | `stopped` with `error`; `stop()` rejects with it |
| Java | any `Throwable`; a `VirtualMachineError` (out of memory, say) is rethrown on the client's thread after the client has ended | `Notification.Stopped` with the error; `stop()` completes exceptionally with it |
| .NET | any `Exception` | `OpAmpNotification.Stopped` with `Error`; `StopAsync()` faults with it |

## Calls

Every method may be called **from the callback and from any thread** (any goroutine, task or
thread the SDK has), at any time after `start` returns. None of them blocks: each is queued for
the client's loop. Calls from one caller reach the core in that caller's order. After the client
has stopped, a call does nothing.

| SDK | From other threads | Notes |
|---|---|---|
| Go | any goroutine | |
| Python | any thread: a call from another thread is handed to the loop (`call_soon_threadsafe`), so it returns before the core has seen it | `stop()` is a coroutine: await it on the client's loop |
| Node.js | one thread: Node's own | |
| Java | any thread | |
| .NET | any thread | |

### Stop from the callback

Stop may be asked for from inside the callback, as long as the callback doesn't wait for it: stop
is over only when the client is, and the client is waiting for the callback. The stop goes ahead
once the callback has returned, and `stopped` comes after it. Waiting for it in the callback
deadlocks; Go's `Stop`, which waits, doesn't wait when called there.

| SDK | From the callback | Not |
|---|---|---|
| Go | `c.Stop(ctx)`: called on the client's goroutine (in the callback), it returns at once, `nil` (or, on `stopped` itself, the error the client stopped with) | waiting in the callback for a `Stop` called on another goroutine |
| Python | `asyncio.ensure_future(client.stop())` | awaiting it in a synchronous callback can't be done; blocking on it deadlocks |
| Node.js | `void client.stop()` | |
| Java | `client.stop()` (a future) | `client.stop().join()` |
| .NET | `_ = client.StopAsync()` | `client.StopAsync().Wait()` |

`stop` itself is bounded: it returns within the stop timeout (5 s by default) and a small margin,
whatever the client was doing (RULE-SHELL-07), but for its callback: one running when stop is
asked for is waited for, and the stop timeout runs from its return.

## What changed

For the changelog of each SDK:

- Go, Java and .NET hold notifications until `start` returns, as Python and Node.js did. Nothing
  is lost, but an application that saw a notification before `start` returned now sees it after.
- Go recovers a panic in the callback, and ends the client with it, instead of the process dying.
- Java catches any `Throwable` from the callback, not only `RuntimeException` (an `Error` used to
  skip the rest of the step and stop the timer, unreported).
- Python's methods may be called from other threads (they used not to be safe).
- The Go and .NET OpenTelemetry integrations (`go/otel`, `Pydantic.OpAmp.AutoInstrumentation`)
  no longer queue answers until `start` returns: they answer through the client their callback
  is given.

## Secrets

What an SDK prints of the options it is given never holds a secret: the headers' values sent to
the server (Authorization, say), a password in the endpoint's or the proxy's URL, a proxy's
CONNECT headers' values, and a client key or its password. Everything else is printed, so a log
of the options still says what they are. Two options that differ only in their secrets' values,
each as long in both, print the same text.

- Python: the `repr` of `Config`, `Proxy` and `TLS`.
- Go: `Config`, `TLS` and `Proxy` with any of fmt's verbs (`%v`, `%+v`, `%#v`, ...) and through
  slog (`String`, `Format`, `LogValue`).
- .NET: `ToString` of the `OpAmpClientOptions` and `OpAmpTlsOptions` records (their
  `PrintMembers`), which is what `$"{options}"` and structured loggers write.
- Node.js: the options are the application's own object; the client keeps none of them where
  `util.inspect` of it shows them (its fields are private).
- Java: `OpampClient.Config` and `Tls` print no fields (`Object.toString`).

The client core's own types are held to the same rule.

## What changed since

- Go's `Stop` may be called from the callback: there it asks for the stop and returns at once,
  and the client stops once the callback has returned. It used to wait for the client, which was
  waiting for the callback: it never returned (or returned the context's error).

## How it is checked

- A callback model test in each SDK, in its own property framework, with a printed seed that replays
  a run: Go `go/opamp/callback_properties_test.go`, Python
  `python/tests/test_callback_properties.py`, Node.js `node/opamp/test/callback.properties.test.ts`,
  Java `java/opamp/src/test/java/dev/pydantic/opamp/CallbackPropertiesTest.java`, .NET
  `dotnet/Pydantic.OpAmp.Client.Tests/CallbackPropertyTests.cs`; 100 runs on a pull request,
  5,000 nightly, in each mode of each SDK. Each runs random starts (with a saved remote config or none),
  calls from outside, from another thread and from the callback, a callback that fails on its k-th
  call, and stop from outside or from the callback, and checks the rules above after every step.
- Secrets: a noninterference test in each SDK whose options types print what they hold (Python
  `python/tests/test_redaction.py`, Go `go/opamp/redact_test.go`, .NET
  `dotnet/Pydantic.OpAmp.Client.Tests/RedactionTests.cs`) and the core's
  (`secrets_change_nothing_shown`): random secrets of one length, two runs, the same text. Each
  redaction dropped in turn must fail its test (each SDK's, by `scripts/redact_mutants.py`).
- The schedule tests and the adversary of [spec/shell-contract.md](../spec/shell-contract.md).
- The conformance scenario `callback-throws` checks it in every SDK from outside. The driver's
  callback throws (raises, panics) on the first remote config; the check `callback_failure_stops`
  then asks for one `fatal` carrying that error, nothing heard by the application after it, and
  exit 1. The client must send no goodbye, and nothing more than one and a half heartbeat
  intervals after the throw: the driver stays alive a while after the `fatal`, so a client that
  kept going would show.
