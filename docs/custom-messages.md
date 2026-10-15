# Custom capabilities and messages

For what OpAMP doesn't cover: an agent and a server that agree on a custom capability (a reverse
FQDN, `io.opentelemetry.pause/v2`, say) can exchange custom messages for it on the OpAMP
connection, with their own types and data.

## What the spec says, and what it leaves open

- Each side declares the custom capabilities it supports (`CustomCapabilities`), and should send
  the list only when it changes. Never sent means none.
- A custom message names its capability, a type, and data whose format the capability defines.
  One for a capability that isn't supported can be ignored.
- Delivery is left open: a message can be lost with the connection, or rejected with the rest of
  the AgentToServer that carried it.

## What the client does

- **Declaring.** The application gives its custom capabilities at start (or later); the client
  sends them in the first message on each connection, in full reports, and when they change, like
  any other status field.
- **Receiving.** A custom message from the server reaches the application
  (`CustomMessage` notification) only for a capability it declared; any other is ignored. The
  application hears the server's custom capabilities when they change
  (`ServerCustomCapabilities`).
- **Sending.** `send_custom_message` queues a message; one goes with each AgentToServer, oldest
  first (over WebSocket all of them at once, over HTTP one per request; over WebSocket not before
  the server has answered the run's first message, or the heartbeat: RULE-CLIENT-22). One whose
  message was lost with the connection or the request, or not processed (UNAVAILABLE, a throttle,
  a reply that isn't one), goes again, ahead of the rest: at least once, so the server may get one
  twice. Over HTTP a message the server answers was delivered. Over WebSocket a push (a
  ServerToAgent the server sends of its own) can't be told from an answer, so a message that seems
  answered may be one the server never read, and the connection may then drop: it counts as
  delivered only once a message sent after that answer is answered too, and until then a drop
  sends it again. Two pushes arriving among the answers can still hide one unread message, which
  is then lost if the connection drops. The client keeps at most 65 custom messages per
  connection that await an answer (or that one round trip): past 64, the next waits in the queue
  until answers release some. One the server rejects (413, BAD_REQUEST) is dropped, and the application hears
  `CustomMessageNotSent` with the reason, as it does at once for a message whose capability it
  didn't declare, one too large for `max_send_bytes`, or one beyond the 64 that may wait
  (RULE-CLIENT-10).

## In each client

| Client | Declare | Send | Hear |
|---|---|---|---|
| Go | `Status.CustomCapabilities`, `SetCustomCapabilities` | `SendCustomMessage` | kinds `CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` |
| Node.js | `status.customCapabilities`, `setCustomCapabilities` | `sendCustomMessage` | `customMessage`, `serverCustomCapabilities`, `customMessageNotSent` |
| Python | `Status.custom_capabilities`, `set_custom_capabilities` | `send_custom_message` | `custom_message`, `server_custom_capabilities`, `custom_message_not_sent` |
| Java | `OpampClient.Status` (the sixth component), `setCustomCapabilities` | `sendCustomMessage` | `Notification.CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` |
| .NET | `OpAmpStatus.CustomCapabilities`, `SetCustomCapabilities` | `SendCustomMessage` | `OpAmpNotification.CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` |

## How it is checked

- The core is checked on its own before it is released: custom messages through every fault it
  knows (lost connections, failed requests, rejections, throttles, UNAVAILABLE with and without a
  message to answer, replies that aren't ones, more unanswered messages than it remembers, a push
  taken for an answer just before the connection drops), each message held to the queue described
  above: one per message, the oldest waiting, resent in order after a loss (those held after a
  seeming answer first), dropped (with the notification) on a rejection, refused when it can never
  go. A custom message whose message seems answered over WebSocket is released exactly when a
  message sent after that answer is answered, and a lost connection hands it back until then.
- The conformance scenario `custom-messages`: the server sends a custom message for the declared
  capability and one for another; on every client over both transports the first reaches the
  application and its answer reaches the server, and the second never reaches it.
