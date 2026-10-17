// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// Says when the client connects, when it can't (once a run of failures, with what to check), and
/// when it's back: setup that works or doesn't is never silent. Notifications come one at a time,
/// on the client's loop. <paramref name="shown"/> is the configured endpoint as it is safe to log,
/// and <paramref name="hints"/> what to check, both as the core's Setup gives them: the hints name
/// what gave the endpoint and the headers (OTEL_OPAMP_*, or the hosting package's settings).
/// </summary>
internal sealed class Reachability(string shown, SetupHints hints, string service, string uid, AgentLog log)
{
    private bool ever;
    private bool failing;

    /// <summary>
    /// An endpoint as it is safe to log (no user info, query or fragment), as the core shows it;
    /// <paramref name="otherwise"/> if the core can't say.
    /// </summary>
    public static string Shown(string endpoint, string otherwise)
    {
        try
        {
            return OpAmpSetup.Start(new SetupStart { Code = new SetupInCode { Endpoint = endpoint } }).ShownEndpoint is { Length: > 0 } s ? s : otherwise;
        }
        catch (CoreException)
        {
            return otherwise;
        }
    }

    /// <summary>What to check when the server can't be reached, by what went wrong, as the core words it.</summary>
    public static string Hint(SetupHints hints, int? status) => status switch
    {
        401 or 403 => hints.Refused,
        404 => hints.NotFound,
        null => hints.Unreachable,
        _ => hints.Error,
    };

    /// <summary>
    /// The endpoint the client uses now, as the client gives it (null, or null from it: the
    /// configured one): after the server moved the agent, the one it moved it to.
    /// </summary>
    public Func<string?>? InUse { get; init; }

    /// <summary>The endpoint the client uses now, as it is safe to log.</summary>
    public string At() => this.InUse?.Invoke() is { Length: > 0 } e ? Shown(e, shown) : shown;

    public void Notified(OpAmpNotification n)
    {
        var endpoint = this.At();
        switch (n)
        {
            case OpAmpNotification.Connected:
                if (!this.ever)
                {
                    var who = service.Length > 0 ? $" as {service}" : string.Empty;
                    log.Info($"connected to {endpoint}; this SDK is managed{who}, instance {uid}");
                }
                else if (this.failing)
                {
                    log.Info($"connected to {endpoint} again");
                }

                this.ever = true;
                this.failing = false;
                break;
            case OpAmpNotification.ConnectFailed f:
                if (!this.failing)
                {
                    var why = string.IsNullOrEmpty(f.Detail) ? $"HTTP {f.Status}" : f.Detail;
                    log.Warn($"can't reach {endpoint} ({why}): {Hint(hints, f.Status)}. Retrying in the background; " +
                             "the SDK keeps its current settings meanwhile");
                }
                else
                {
                    log.Debug?.Invoke($"still can't reach {endpoint}: {f.Detail}");
                }

                this.failing = true;
                break;
        }
    }
}
