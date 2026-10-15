# The instance UID across restarts

An agent's instance UID is its identity to the server. The spec asks the agent to make its own,
as a UUIDv7 (OPAMP-UID-01, -03), or to ask the server for one (OPAMP-UID-04), and to use a UID the
server assigns "for all further communication" (OPAMP-UID-05, -06). A client that forgets it on restart looks like a new agent
every time, and undoes whatever UID the server assigned.

## The rules

With a state file (`persistence`), the core keeps the UID in use in its saved state, with the UID
the application chose (if it chose one). At start:

| The application | Saved | The UID used |
|---|---|---|
| passes none: the core makes one (a UUIDv7 of the wall clock and the client's random bytes) | a UID | the saved one |
| passes none | nothing | the one the core made, saved at once |
| chooses a UID | a UID that started from the same choice | the saved one (the server may have assigned it since) |
| chooses a UID | a UID from another choice, or nothing | the application's: its choice wins |
| asks the server for one (`request_instance_uid`), with either of the above | the UID used, which the server assigned | that UID, without asking again |
| asks the server for one | anything else | the UID the rows above give, as the temporary value, asking until the server assigns one |

When the server assigns a UID, the core saves it (a save that fails is tried again until one lands:
lost, the agent would come back under the old UID and ask again). If a saved UID is used instead of the one the
application passed, the application hears at start (the `InstanceUidChanged` notification), so it
can name the instance the same way elsewhere (`service.instance.id`, say).

Without a state file nothing is saved or read: the application's UID, or a new one the core
makes each start.

## Asking the server for a UID

With `request_instance_uid`, the client asks the server to assign the instance UID
(`RequestInstanceUid`, OPAMP-UID-04). The UID the client starts with, made by the core or chosen, is the
temporary value; it is never all zeros.

- Every message but the goodbye (`agent_disconnect`) carries the flag, until a response assigns a
  usable UID (16 bytes, not all zeros). Any such UID ends the request: one equal to the temporary
  UID, and one sent with an error (a duplicate, say).
- Over WebSocket, while asking, one message at a time: the next waits for the answer to the last.
  A server that honours the flag gives a new UID to every message that asks, so a
  second one in flight under the temporary UID would get a second UID, and leave an agent on the
  server that nobody is. (Over plain HTTP there is only ever one request in flight.) Over
  WebSocket the server may also send a message of its own at any time (a push), which the client
  can't tell from an answer, so a response doesn't end the wait: what settles the request does (a
  UID assigned, an error, a new connection), or `UID_ANSWER_MS` (30 s) passing after the message
  asking went (a server answers within it).
- A server that ignores the flag answers without a UID, which settles nothing: over WebSocket it is
  asked once every `UID_ANSWER_MS`, the changes meanwhile going with the next message; over HTTP
  with every message, no sooner than the client would send anyway.
- With a state file, the UID the server assigned is saved as assigned (`uid_assigned`), and a
  restart goes on with it without asking. A restart before the server answered asks again, under
  the same temporary UID. State saved by an older version reads as not assigned.
- **Without a state file** the option still works, but nothing is remembered: every start asks
  again, and the server assigns a new UID each time. The agent is a new agent to the server after
  every restart.

| Client | Ask the server for a UID |
|---|---|
| Go | `Config.RequestInstanceUID` |
| Node | `requestInstanceUid: true` |
| Python | `Config.request_instance_uid` |
| Java | `Config.builder(endpoint).requestInstanceUid(true)` |
| .NET | `OpAmpClientOptions.RequestInstanceUid` |

## In each client

Pass no instance UID to let the core choose one and keep it (RULE-CLIENT-19): the client gives it
random bytes from the platform's CSPRNG and the wall clock, and the core makes a UUIDv7 of them.

| Client | No UID |
|---|---|
| Go | `Config.InstanceUID` left zero |
| Node | `instanceUid` omitted |
| Python | `Config.instance_uid` left `None` |
| Java | `Config.builder(endpoint)` |
| .NET | `OpAmpClientOptions.InstanceUid` left null |

Each UID the core makes is a UUIDv7: the time in milliseconds, then random bits.

## How it is checked

- The core is checked on its own before it is released: each row of the tables above, a
  server-assigned UID kept across a restart, nothing saved or read without a state file, and,
  asking for a UID, the flag on every message exactly while asking and one message in flight over
  WebSocket.
- The conformance scenarios `uid-made-up-restart` and `uid-assigned-restart` restart each client
  and check it comes back under the UID it had (RULE-CLIENT-06). Upstream opamp-go comes back
  under its original UID, forgetting the one the server assigned.
- The conformance scenarios `request-uid`, `request-uid-ignored` and `request-uid-restart`.
