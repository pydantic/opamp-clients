// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>What the tests ask of a host: the settings it leaves out. The layer refuses a policy that overrides one.</summary>
internal static class HostTestExtensions
{
    /// <summary>
    /// The settings whose declaring depends on what the SDK built, in this order: the instrumentation
    /// switches, each signal's sending, the log level and the sampling rate.
    /// </summary>
    private static readonly string[] Built =
    [
        "deactivate_instrumentations", "deactivate_all_instrumentations", "send_traces", "send_metrics", "send_logs",
        "send_logs_level", "sampling_rate",
    ];

    /// <summary>Those of <see cref="Built"/> that <paramref name="host"/> doesn't declare.</summary>
    public static string[] Undeclared(this Host host) =>
        Built.Where(k => !host.Live.Contains(k) && !host.NextStart.Contains(k)).ToArray();
}
