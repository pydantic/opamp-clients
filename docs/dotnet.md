# .NET

Use the upstream client, `OpenTelemetry.OpAmp.Client`, unchanged. Checked against
0.7.0-alpha.2, over plain HTTP and WebSocket.

## Answer ReportFullState yourself

The client hands the server's flags to the application instead of acting on them. Subscribe to
`FlagsMessage` and, when `ReportFullState` is set, send a full state report:

```csharp
sealed class FlagsListener(OpAmpClient client) : IOpAmpListener<FlagsMessage>
{
    public void HandleMessage(FlagsMessage message)
    {
        if (message.Flags.HasFlag(ServerSentFlags.ReportFullState))
        {
            client.SendFullStateReport(new FullStateReport { RemoteConfigStatus = lastStatus });
        }
    }
}
```

Without this, a server that restarted or missed a message never gets the agent's status back.

## Known gaps

These show up in the conformance suite (`conformance/`):

- Capabilities are only set on the first message; later messages send 0. The spec says the
  field must always be set.
- A server-assigned instance UID (`agent_identification.new_instance_uid`) is ignored, and
  the `OpAMP-Instance-UID` header has its bytes in the wrong order. Both are fixed on the
  upstream main branch but not released yet.
- `SendRemoteConfigStatus`, called from a remote-config listener (even on another thread),
  didn't return within an 8 s run, so the status never reached the server.
- Over WebSocket, a Close frame from the server is not answered, and the client doesn't
  reconnect after losing the connection: in a torture run it stayed offline for the last
  220 s of four minutes after one TCP drop.
- `Retry-After` and `retry_info` are ignored.
- There are no connect or error callbacks, and the agent description can't be changed after
  start.

The client does follow the heartbeat interval the server offers, and sends `agent_disconnect`
on `StopAsync()` (not on `Dispose()`).
