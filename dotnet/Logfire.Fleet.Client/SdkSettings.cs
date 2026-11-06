// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Logfire.Fleet.Client.Sdk;

/// <summary>
/// Decides what an OpenTelemetry SDK does with the Logfire SDK policy an OpAMP server sends it, the same way
/// in every language: the SDK settings layer, run natively (the native
/// library the core is in). It keeps nothing between calls; the caller keeps
/// the agent flow's state it is handed and passes it back with the next step.
/// Thread-safe. See docs/sdk-settings.md.
/// </summary>
public sealed class SdkSettings : IDisposable
{
    /// <summary>Loads the layer.</summary>
    public SdkSettings() => Native.Check();

    /// <summary>
    /// The agent flow's start, before the SDK is built: the agent to keep and pass to the next
    /// <see cref="Step"/>, and what to do first (the settings to start with, from the last config
    /// applied in the core's saved state). A host naming what is no setting throws.
    /// </summary>
    public AgentOutput Start(AgentStart start) => Do(new Request { AgentStart = start }).AgentOutput;

    /// <summary>
    /// One input to the agent flow (a remote config, a connection, the timer, the exporters built...):
    /// the agent to keep, the effects to carry out in order, and when to step the timer next.
    /// </summary>
    public AgentOutput Step(AgentStep step) => Do(new Request { AgentStep = step }).AgentOutput;

    /// <summary>
    /// The agent description, from the SDK's resource attributes. It also names the policy format the
    /// SDK reads and the controls that <paramref name="host"/> carries out
    /// (<c>logfire.opamp.config_format</c> and <c>logfire.opamp.supported_controls</c>). The host and
    /// the time as for the agent flow: with no host it lists no control, and with no clock it does not list <c>debug</c>.
    /// </summary>
    public AgentDescription Describe(IEnumerable<KeyValue> resource, Host? host = null, ulong nowUnixMs = 0)
    {
        var describe = new Describe { Host = host, NowUnixMs = nowUnixMs };
        describe.Resource.AddRange(resource);
        return Do(new Request { Describe = describe }).Description;
    }

    /// <summary>
    /// The inventory as the agent's available components (docs/available-components.md), for
    /// <c>OpAmpStatus.AvailableComponents</c> or <c>SetAvailableComponents</c>: what the
    /// integration lists, and the settings this host carries out. The hash is left empty: the core
    /// computes it. The host and the time as for the agent flow.
    /// </summary>
    public AvailableComponents Components(Inventory inventory, Host? host = null, ulong nowUnixMs = 0) =>
        Do(new Request { Components = new Components { Inventory = inventory, Host = host, NowUnixMs = nowUnixMs } }).AvailableComponents;

    /// <summary>
    /// What Fleet is told of the exporters an integration found on its SDK's providers, decided the
    /// same in every language: the name each is listed under, where it sends (its own endpoint, else
    /// the <c>OTEL_EXPORTER_*</c> variables in <paramref name="env"/>), the governed and ungoverned
    /// signals, and a warning for each ungoverned exporter. At start and when the providers change.
    /// </summary>
    public ExportersDecided Exporters(IEnumerable<FoundExporter> found, IEnumerable<KeyValuePair<string, string>>? env = null,
        string defaultProtocol = "")
    {
        var call = new Exporters { Found = { found }, DefaultProtocol = defaultProtocol };
        call.Env.AddRange((env ?? []).Where(kv => kv.Key.StartsWith("OTEL_EXPORTER_", StringComparison.Ordinal))
            .Select(kv => new EnvVar { Name = kv.Key, Value = kv.Value }));
        return Do(new Request { Exporters = call }).ExportersDecided;
    }

    /// <inheritdoc/>
    public void Dispose()
    {
        // Nothing is held between calls.
    }

    private static Reply Do(Request request)
    {
        var reply = Reply.Parser.ParseFrom(Native.SdkSettingsCall(request.ToByteArray()));
        if (reply.ResultCase == Reply.ResultOneofCase.Error)
        {
            throw new CoreException($"SDK settings layer: {reply.Error}");
        }

        return reply;
    }
}
