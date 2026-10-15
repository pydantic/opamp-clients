# Python

Use the upstream client, `opentelemetry-opamp-client`, unchanged. Checked against 0.3b0.

The client is plain HTTP only.

## Install `requests` too

0.3b0 uses `requests` for its default transport but doesn't declare it, so importing the
client fails with `ModuleNotFoundError: No module named 'requests'`. Install it alongside:

```
pip install opentelemetry-opamp-client==0.3b0 requests
```

This is fixed on the upstream main branch but not released yet.

## Known gaps

These show up in the conformance suite (`conformance/`):

- The agent description can't be changed after the client starts.
- A server-assigned instance UID (`agent_identification.new_instance_uid`) is ignored.
- The client declares `ReportsHeartbeat` but ignores the heartbeat interval the server offers.
- Queued messages keep the `sequence_num` they were given when queued, so numbers can repeat
  or skip.
- A message the server rejected with 413 is retried.
- `Retry-After` is ignored; requests are not sent with the `OpAMP-Instance-UID` header.
- On stop, a queued heartbeat can go out after the `agent_disconnect` message.
