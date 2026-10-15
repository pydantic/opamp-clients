// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Client.Sdk;

/// <summary>
/// Decides what an OpenTelemetry SDK does with the settings an OpAMP server sends it, the same way
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
    /// <paramref name="host"/>: the settings the SDK carries out, at once or at the next start; one it
    /// leaves out is set aside, never in force (null: none carried out), and one that is no setting
    /// throws. <paramref name="nowUnixMs"/>: the wall clock in milliseconds since the Unix epoch, which debug
    /// mode is decided by (0: none, and debug mode is ignored).
    /// </summary>
    public Loaded Load(byte[]? saved, Host? host = null, ulong nowUnixMs = 0)
    {
        var load = new Load { Host = host, NowUnixMs = nowUnixMs };
        if (saved != null)
        {
            load.Saved = ByteString.CopyFrom(saved);
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

    /// <summary>The agent description, from the SDK's resource attributes.</summary>
    public AgentDescription Describe(IEnumerable<KeyValue> resource)
    {
        var describe = new Describe();
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
