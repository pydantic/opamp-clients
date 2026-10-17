// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Client.Sdk;

/// <summary>
/// Decides what an OpenTelemetry SDK does with the Logfire SDK policy an OpAMP server sends it, the same way
/// in every language: the SDK settings layer, run natively (the native
/// library the core is in). It keeps nothing between calls; the caller keeps
/// the settings documents it is handed (<c>Current</c>, and the one it saves) and passes them back.
/// Thread-safe. See docs/sdk-settings.md.
/// </summary>
public sealed class SdkSettings : IDisposable
{
    /// <summary>Loads the layer.</summary>
    public SdkSettings() => Native.Check();

    /// <summary>
    /// At start, before the SDK is built: the settings to start with, from the document saved at the
    /// last change (null: nothing saved). Put all of them in force, next-start ones too.
    /// <paramref name="host"/>: the settings the SDK carries out, at once or at the next start. A policy
    /// that overrides one it leaves out is refused (null: none carried out), and a name that is no
    /// setting throws. <paramref name="nowUnixMs"/>: the wall clock in milliseconds since the Unix epoch,
    /// which debug mode is decided by (0: none, and the SDK cannot carry out debug mode).
    /// <paramref name="coreState"/>: the OpAMP
    /// core's saved state (what it asked the host to store; null: none). The settings then come from
    /// the last config applied in it, and <paramref name="saved"/> is read only when it holds none (a
    /// document kept before the core kept the config applied).
    /// </summary>
    public Loaded Load(byte[]? saved, Host? host = null, ulong nowUnixMs = 0, byte[]? coreState = null)
    {
        var load = new Load { Host = host, NowUnixMs = nowUnixMs };
        if (saved != null)
        {
            load.Saved = ByteString.CopyFrom(saved);
        }

        if (coreState != null)
        {
            load.CoreState = ByteString.CopyFrom(coreState);
        }

        return Do(new Request { Load = load }).Loaded;
    }

    /// <summary>
    /// How to answer a remote config. <paramref name="current"/> is the document in force (from the
    /// last Load or Decide), <paramref name="atStart"/> the one loaded at start; the host and the time
    /// as for Load.
    /// </summary>
    /// <remarks>
    /// The layer decides what to keep for the next start; <paramref name="save"/> keeps it (whole or
    /// not at all, where Load reads it then), and throws if it can't; the layer then answers from how
    /// that went: a config whose save failed is refused, saying why. Null: the SDK keeps no state
    /// across restarts, so a config that changes a next-start setting is refused.
    /// </remarks>
    public Decision Decide(
        AgentRemoteConfig config, ByteString current, ByteString atStart, Host? host = null, ulong nowUnixMs = 0, Action<byte[]>? save = null)
    {
        var decide = new Decide { Config = config, Current = current, AtStart = atStart, Host = host, NowUnixMs = nowUnixMs };
        var plan = Do(new Request { Decide = decide }).Plan;
        var commit = new Commit { Decide = decide };
        if (plan.HasSave && save != null)
        {
            try
            {
                save(plan.Save.ToByteArray());
                commit.Saved = Commit.Types.Saved.Done;
            }
            catch (Exception e)
            {
                // Whatever stopped the save, the config is refused for it.
                commit.Saved = Commit.Types.Saved.Failed;
                commit.Error = e.Message;
            }
        }

        return Do(new Request { Commit = commit }).Decision;
    }

    /// <summary>
    /// What is in force at <paramref name="nowUnixMs"/>, at the time a load, decision or refresh said to
    /// ask again (<c>RefreshAtUnixMs</c>): the end of debug mode. <paramref name="current"/> and
    /// <paramref name="atStart"/> as for Decide.
    /// </summary>
    public Refreshed Refresh(ByteString current, ByteString atStart, ulong nowUnixMs, Host? host = null) =>
        Do(new Request
        {
            Refresh = new Refresh { Current = current, AtStart = atStart, NowUnixMs = nowUnixMs, Host = host },
        }).Refreshed;

    /// <summary>
    /// Export health, from the gates' counts since start: whether to send it now, and the entry for the
    /// agent health's <c>component_health_map["exporters"]</c>. <paramref name="previous"/>: what the
    /// last call answered (null at the first); <paramref name="settings"/>: those in force.
    /// </summary>
    public HealthReport Health(ExportCounts counts, HealthReport? previous, ulong nowUnixMs, Settings? settings = null) =>
        Do(new Request
        {
            Health = new Health { Counts = counts, Previous = previous, NowUnixMs = nowUnixMs, Settings = settings },
        }).HealthReport;

    /// <summary>
    /// The agent description, from the SDK's resource attributes. It also names the policy format the
    /// SDK reads and the controls that <paramref name="host"/> carries out
    /// (<c>logfire.opamp.config_format</c> and <c>logfire.opamp.supported_controls</c>). The host and
    /// the time as for Load: with no host it lists no control, and with no clock it does not list <c>debug</c>.
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
    /// computes it. The host and the time as for Load.
    /// </summary>
    public AvailableComponents Components(Inventory inventory, Host? host = null, ulong nowUnixMs = 0) =>
        Do(new Request { Components = new Components { Inventory = inventory, Host = host, NowUnixMs = nowUnixMs } }).AvailableComponents;

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
