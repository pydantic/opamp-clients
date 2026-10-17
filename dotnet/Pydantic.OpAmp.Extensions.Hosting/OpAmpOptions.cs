// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

namespace Pydantic.OpAmp.Extensions.Hosting;

/// <summary>
/// Where the OpAMP server is. Each is read from the configuration's <c>OpAmp</c> section
/// (<c>OpAmp:Endpoint</c>, <c>OpAmp:Headers:&lt;name&gt;</c>, <c>OpAmp:StateDirectory</c>,
/// <c>OpAmp:HeartbeatInterval</c>), else from the variables the automatic instrumentation plugin
/// reads (<c>OTEL_OPAMP_ENDPOINT</c>, <c>OTEL_OPAMP_HEADERS</c>, <c>OTEL_OPAMP_STATE_DIR</c>,
/// <c>OTEL_OPAMP_HEARTBEAT_INTERVAL</c>); what <c>WithOpAmp</c>'s delegate sets wins over both.
/// </summary>
public sealed class OpAmpOptions
{
    /// <summary>
    /// The OpAMP server: <c>http(s)://.../v1/opamp</c> polls, <c>ws(s)://...</c> keeps a connection
    /// open. Without one, nothing is managed and the application runs as it would without the package.
    /// </summary>
    public Uri? Endpoint { get; set; }

    /// <summary>Headers for every request to the server: the enrollment key goes here.</summary>
    public IDictionary<string, string> Headers { get; } = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);

    /// <summary>
    /// Where to keep state across restarts: the instance UID, the settings for the next start, the
    /// client's own state. Without it, one of the user's own, one per process of the application
    /// running at once (docs/default-state-dir.md); <c>none</c>: none, and each start is a new
    /// instance. One process per directory named here.
    /// </summary>
    public string? StateDirectory { get; set; }

    /// <summary>How often to check in until the server offers an interval (null: the client's default, 30 s).</summary>
    public TimeSpan? HeartbeatInterval { get; set; }

    /// <summary>What the configuration had that couldn't be read, and was the default instead: said once there is a logger.</summary>
    internal List<string> Warnings { get; } = [];

    /// <summary>Why the configured endpoint couldn't be used, if it couldn't: then nothing is managed.</summary>
    internal string? UnusableEndpoint { get; set; }
}
