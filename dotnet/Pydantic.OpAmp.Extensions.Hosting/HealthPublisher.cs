// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Microsoft.Extensions.Diagnostics.HealthChecks;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Extensions.Hosting;

/// <summary>
/// The application's health checks, as the agent's health: what they say each time they run
/// (<c>HealthCheckPublisherOptions</c>), reported when it changes. Without health checks
/// registered, nothing runs it, and the agent is healthy while it runs.
/// </summary>
internal sealed class HealthPublisher(OpAmpAgent agent) : IHealthCheckPublisher
{
    private readonly ulong start = UnixNanos();
    private ComponentHealth? last;

    public Task PublishAsync(HealthReport report, CancellationToken cancellationToken)
    {
        var health = Health(report, this.start);
        if (this.last is null || !Same(this.last, health))
        {
            agent.SetHealth(health);
            this.last = health;
        }

        return Task.CompletedTask;
    }

    /// <summary>The report as OpAMP says it: each check a component; degraded still serves.</summary>
    internal static ComponentHealth Health(HealthReport report, ulong start)
    {
        var now = UnixNanos();
        var health = new ComponentHealth
        {
            Healthy = report.Status != HealthStatus.Unhealthy,
            StartTimeUnixNano = start,
            Status = report.Status.ToString(),
            StatusTimeUnixNano = now,
        };
        foreach (var (name, entry) in report.Entries)
        {
            health.ComponentHealthMap[name] = new ComponentHealth
            {
                Healthy = entry.Status != HealthStatus.Unhealthy,
                Status = entry.Status.ToString(),
                LastError = entry.Status == HealthStatus.Healthy ? string.Empty : entry.Description ?? entry.Exception?.Message ?? string.Empty,
                StatusTimeUnixNano = now,
            };
        }

        health.LastError = string.Join("; ", health.ComponentHealthMap
            .Where(c => c.Value.LastError.Length > 0)
            .Select(c => $"{c.Key}: {c.Value.LastError}"));
        return health;
    }

    /// <summary>The same but for the time of the status.</summary>
    internal static bool Same(ComponentHealth a, ComponentHealth b) => Timeless(a).Equals(Timeless(b));

    private static ComponentHealth Timeless(ComponentHealth h)
    {
        var c = h.Clone();
        c.StatusTimeUnixNano = 0;
        foreach (var key in c.ComponentHealthMap.Keys.ToList())
        {
            c.ComponentHealthMap[key] = Timeless(c.ComponentHealthMap[key]);
        }

        return c;
    }

    private static ulong UnixNanos() => (ulong)DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() * 1_000_000;
}
