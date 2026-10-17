// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Pydantic.OpAmp.Client;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// What the integration's settings are called, for what it says: OTEL_OPAMP_* for the plugin, the
/// <c>OpAmp</c> section for the hosting package. <paramref name="Credentials"/> is the header
/// setting with an example of the enrollment key in it.
/// </summary>
internal sealed record SettingNames(string Endpoint, string Credentials)
{
    public static readonly SettingNames Env = new("OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS (Authorization=Bearer <token>)");
}

/// <summary>
/// Says when the client connects, when it can't (once a run of failures, with what to check), and
/// when it's back: setup that works or doesn't is never silent. Notifications come one at a time,
/// on the client's loop.
/// </summary>
internal sealed class Reachability(Uri endpoint, SettingNames names, string service, string uid, AgentLog log)
{
    private readonly string endpoint = Where(endpoint);
    private bool ever;
    private bool failing;

    /// <summary>The endpoint as it is safe to log: no user info, query or fragment.</summary>
    public static string Where(Uri endpoint) => Where(endpoint.OriginalString);

    /// <summary>
    /// The same, from the text as written: scheme://host:port/path, never parsed further (a bad port
    /// or an unclosed IPv6 bracket can't make it throw). Without a scheme, the text less its query,
    /// fragment and anything up to the last '@'.
    /// </summary>
    public static string Where(string endpoint)
    {
        var s = endpoint.Trim().Split('?', 2)[0].Split('#', 2)[0];
        var scheme = s.IndexOf("://", StringComparison.Ordinal);
        if (scheme <= 0)
        {
            return s[(s.LastIndexOf('@') + 1)..];
        }

        var rest = s[(scheme + 3)..];
        var slash = rest.IndexOf('/');
        var authority = slash < 0 ? rest : rest[..slash];
        var path = slash < 0 ? string.Empty : rest[slash..];
        return $"{s[..scheme]}://{authority[(authority.LastIndexOf('@') + 1)..]}{path}";
    }

    /// <summary>What to check when the server can't be reached, by what went wrong.</summary>
    public static string Hint(int? status, SettingNames names) => status switch
    {
        401 or 403 => $"the server refused this agent's credentials: check {names.Credentials}",
        404 => $"nothing serves OpAMP at that path: check {names.Endpoint} (it usually ends in /v1/opamp)",
        null => $"check {names.Endpoint} and that the server is up",
        _ => "the server answered with an error",
    };

    /// <summary>
    /// The endpoint the client uses now, as the client gives it (null, or null from it: the
    /// configured one): after the server moved the agent, the one it moved it to.
    /// </summary>
    public Func<string?>? InUse { get; init; }

    /// <summary>The endpoint the client uses now, as it is safe to log.</summary>
    public string At() => this.InUse?.Invoke() is { Length: > 0 } e ? Where(e) : this.endpoint;

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
                    log.Warn($"can't reach {endpoint} ({why}): {Hint(f.Status, names)}. Retrying in the background; " +
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
