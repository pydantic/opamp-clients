// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Reflection;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection.Extensions;
using Microsoft.Extensions.Diagnostics.HealthChecks;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Extensions.Hosting;
using Pydantic.OpAmp.Otel;

// Where services.AddOpenTelemetry() is: wherever an application sets its SDK up, WithOpAmp() is there too.
namespace Microsoft.Extensions.DependencyInjection;

/// <summary>The OpenTelemetry .NET SDK an application sets up in code, under an OpAMP server.</summary>
public static class OpAmpOpenTelemetryBuilderExtensions
{
    /// <summary>
    /// Put the SDK under an OpAMP server (<see cref="OpAmpOptions"/>: the <c>OpAmp</c> configuration
    /// section, else <c>OTEL_OPAMP_ENDPOINT</c> and the rest). The SDK joins the server's fleet as
    /// one agent, named by <c>service.instance.id</c>, and takes the server's SDK settings
    /// (docs/sdk-settings.md): the sampling rate, through a sampler wrapping the application's,
    /// and whether each signal is sent, through every OTLP exporter. The application's health
    /// checks, if it has any, are the agent's health. Without an endpoint it changes nothing.
    /// Call it after the tracing setup that sets a sampler.
    /// </summary>
    public static T WithOpAmp<T>(this T builder, Action<OpAmpOptions>? configure = null)
        where T : IOpenTelemetryBuilder
    {
        var services = builder.Services;
        // Once per application, however many times it is called: each call's delegate still applies, in turn.
        var first = !services.Any(d => d.ImplementationType == typeof(OpAmpHostedService));
        if (first)
        {
            services.AddOptions<OpAmpOptions>().Configure<IConfiguration>(Read);
        }

        if (configure is not null)
        {
            services.Configure(configure);
        }

        if (!first)
        {
            return builder;
        }

        services.TryAddSingleton(sp => new OpAmpAgent(sp.GetRequiredService<IOptions<OpAmpOptions>>(), sp.GetService<IConfiguration>()));
        services.AddHostedService<OpAmpHostedService>();
        services.AddSingleton<IHealthCheckPublisher, HealthPublisher>();
        // Every OTLP exporter's options, whatever their name: the signal is told by each request's path.
        services.AddSingleton<IPostConfigureOptions<OtlpExporterOptions>>(sp =>
            new PostConfigureOptions<OtlpExporterOptions, OpAmpAgent>(null, sp.GetRequiredService<OpAmpAgent>(), (o, agent) => agent.Gate(o)));
        // The ASP.NET Core instrumentation's options, if the application has it: the sampler's Filter
        // (AspNetCoreFilter), after the application's own. By reflection, so that the package
        // doesn't make every application an ASP.NET Core one.
        if (AspNetCoreFilter.OptionsType() is { } aspNetCore)
        {
            typeof(OpAmpOpenTelemetryBuilderExtensions).GetMethod(nameof(PostConfigureFilter), BindingFlags.NonPublic | BindingFlags.Static)!
                .MakeGenericMethod(aspNetCore).Invoke(null, [services]);
        }

        builder.ConfigureResource(r => r.AddDetector(sp => new InstanceDetector(sp.GetRequiredService<OpAmpAgent>())));
        services.ConfigureOpenTelemetryTracerProvider(t =>
        {
            if (t is IDeferredTracerProviderBuilder deferred)
            {
                deferred.Configure((sp, inner) => sp.GetRequiredService<OpAmpAgent>().WrapSampler(inner));
            }
        });
        return builder;
    }

    private static void PostConfigureFilter<TOptions>(IServiceCollection services)
        where TOptions : class =>
        services.AddSingleton<IPostConfigureOptions<TOptions>>(sp =>
            new PostConfigureOptions<TOptions, OpAmpAgent>(null, sp.GetRequiredService<OpAmpAgent>(), (o, agent) => agent.Filter(o)));

    /// <summary>The <c>OpAmp</c> section's settings, each else its OTEL_OPAMP_* variable.</summary>
    private static void Read(OpAmpOptions o, IConfiguration configuration)
    {
        // A value it can't read never fails the host: it is the default, or (the endpoint) nothing
        // is managed, said once there is a logger (OpAmpAgent).
        var section = configuration.GetSection("OpAmp");
        Server? env = null;
        string? unusable = null;
        try
        {
            env = Server.FromEnv(name => configuration[name] is { Length: > 0 } v ? v : Environment.GetEnvironmentVariable(name), o.Warnings.Add);
        }
        catch (UriFormatException e)
        {
            unusable = $"OTEL_OPAMP_ENDPOINT is not a URL ({e.Message})";
        }

        var endpoint = section["Endpoint"];
        if (string.IsNullOrWhiteSpace(endpoint))
        {
            o.Endpoint = env?.Endpoint;
        }
        else if (Uri.TryCreate(endpoint.Trim(), UriKind.Absolute, out var uri))
        {
            o.Endpoint = uri;
            unusable = null;
        }
        else
        {
            unusable = "OpAmp:Endpoint is not a URL";
        }

        o.UnusableEndpoint = unusable;
        var headers = section.GetSection("Headers").GetChildren().Where(c => c.Value is not null).ToList();
        foreach (var (name, value) in headers.Count > 0 ? headers.Select(c => (c.Key, c.Value!)) : (env?.Headers ?? new Dictionary<string, string>()).Select(h => (h.Key, h.Value)))
        {
            o.Headers[name] = value;
        }

        var stateDir = section["StateDirectory"];
        o.StateDirectory = !string.IsNullOrWhiteSpace(stateDir) ? stateDir.Trim() : env?.StateDir;
        var heartbeat = section["HeartbeatInterval"];
        // As the variable (1500 ms, 500ms, 30s, 2m), or a TimeSpan (00:00:30).
        o.HeartbeatInterval = !string.IsNullOrWhiteSpace(heartbeat) ? Variables.Interval("OpAmp:HeartbeatInterval", heartbeat.Trim(), o.Warnings.Add, timeSpan: true)
            : env is { Heartbeat: var h } && h > TimeSpan.Zero ? h : null;
    }

    /// <summary><c>service.instance.id</c>: the agent's instance UID, so the server and the telemetry name the instance alike.</summary>
    private sealed class InstanceDetector(OpAmpAgent agent) : IResourceDetector
    {
        public Resource Detect() =>
            agent.Agent is { } a ? new Resource([new("service.instance.id", a.Uid)]) : Resource.Empty;
    }
}

/// <summary>The same, from the application's builder (<c>Microsoft.Extensions.Hosting</c>'s <c>IHostApplicationBuilder</c>).</summary>
public static class OpAmpHostApplicationBuilderExtensions
{
    /// <summary>
    /// <c>builder.Services.AddOpenTelemetry().WithOpAmp(configure)</c>: the SDK the application
    /// sets up, under an OpAMP server.
    /// </summary>
    public static TBuilder AddPydanticOpAmp<TBuilder>(this TBuilder builder, Action<OpAmpOptions>? configure = null)
        where TBuilder : IHostApplicationBuilder
    {
        builder.Services.AddOpenTelemetry().WithOpAmp(configure);
        return builder;
    }
}
