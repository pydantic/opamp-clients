// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using OpenTelemetry;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;

namespace Pydantic.OpAmp.Extensions.Hosting;

/// <summary>
/// The OpAMP client's lifetime: started with the host, once the SDK is built (what it carries out
/// is known then, and its resource describes the agent); stopped with it, the server told the
/// agent is going (agent_disconnect).
/// </summary>
internal sealed class OpAmpHostedService(IServiceProvider services, OpAmpAgent agent, ILogger<OpAmpHostedService> logger) : IHostedService
{
    public Task StartAsync(CancellationToken cancellationToken)
    {
        if (agent.Agent is not { } a)
        {
            agent.Said(logger);
            return Task.CompletedTask;
        }

        // Build what the application set up, if nothing has yet.
        var tracing = services.GetService<TracerProvider>();
        var metrics = services.GetService<MeterProvider>();
        var logs = services.GetService<LoggerProvider>();
        var resource = tracing?.GetResource() ?? metrics?.GetResource() ??
                       ResourceBuilder.CreateDefault().AddAttributes([new("service.instance.id", a.Uid)]).Build();
        try
        {
            agent.Start(resource.Attributes, logger, tracing, metrics, logs);
        }
        catch (Exception e)
        {
            // Telemetry never waits on the server, nor fails the application.
            logger.LogError(e, "OpAMP: the client did not start; the SDK runs unmanaged");
        }

        return Task.CompletedTask;
    }

    public async Task StopAsync(CancellationToken cancellationToken)
    {
        if (agent.Agent is not { } a)
        {
            return;
        }

        // As long as the core says (its stop timeout, 5 s), or the host allows.
        try
        {
            await Task.Run(() => a.Stop(Timeout.InfiniteTimeSpan), CancellationToken.None).WaitAsync(cancellationToken);
        }
        catch (OperationCanceledException)
        {
        }
    }
}
