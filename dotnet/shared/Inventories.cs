// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Reflection;
using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// The inventory (docs/available-components.md): what the integration lists of itself and the SDK,
/// for the layer to make the agent's available components of. The instrumentations and exporters
/// are the integration's; the rest is the same for both.
/// </summary>
internal static class Inventories
{
    /// <summary>
    /// <paramref name="integration"/>'s package (its assembly's name, which is the package's) and
    /// version, the OpenTelemetry SDK's, and what the integration lists.
    /// </summary>
    public static Inventory Of(Assembly integration, IEnumerable<Instrumentation> instrumentations, IEnumerable<Exporter> exporters) => new()
    {
        Integration = integration.GetName().Name ?? string.Empty,
        IntegrationVersion = Version(integration),
        SdkLanguage = "dotnet",
        SdkVersion = Version(typeof(OpenTelemetry.Sdk).Assembly),
        Instrumentations = { instrumentations },
        Exporters = { exporters },
    };

    /// <summary>
    /// The instrumentation scope an instrumentation's telemetry carries (the name of the
    /// ActivitySource its spans come from on .NET 8 and later, or of the Meter for one that makes
    /// metrics alone), by the name <c>deactivate_instrumentations</c> takes: for those whose scope is
    /// known. A logging bridge's logs are under the application's own categories: none of its own.
    /// </summary>
    public static readonly IReadOnlyDictionary<string, string> Scopes = new Dictionary<string, string>
    {
        ["aspnetcore"] = "Microsoft.AspNetCore",
        ["entityframeworkcore"] = "OpenTelemetry.Instrumentation.EntityFrameworkCore",
        ["grpcnetclient"] = "OpenTelemetry.Instrumentation.GrpcNetClient",
        ["httpclient"] = "System.Net.Http",
        ["masstransit"] = "MassTransit",
        ["mysqlconnector"] = "MySqlConnector",
        ["netruntime"] = "OpenTelemetry.Instrumentation.Runtime",
        ["npgsql"] = "Npgsql",
        ["nservicebus"] = "NServiceBus.Core",
        ["process"] = "OpenTelemetry.Instrumentation.Process",
        ["quartz"] = "OpenTelemetry.Instrumentation.Quartz",
        ["sqlclient"] = "OpenTelemetry.Instrumentation.SqlClient",
        ["stackexchangeredis"] = "OpenTelemetry.Instrumentation.StackExchangeRedis",
        ["wcfclient"] = "OpenTelemetry.Instrumentation.Wcf",
    };

    /// <summary>
    /// An instrumentation as the inventory lists it, with its scope: <see cref="Scopes"/>'s, else
    /// <paramref name="otherwise"/> (empty: not known).
    /// </summary>
    public static Instrumentation Scoped(string name, string version, string otherwise = "") =>
        new() { Name = name, Version = version, Scope = Scopes.GetValueOrDefault(name, otherwise) };

    /// <summary>
    /// An assembly's version as its package's: the informational version without its build metadata
    /// (<c>1.19.1+abc</c> is <c>1.19.1</c>), else the assembly version's first three parts.
    /// </summary>
    public static string Version(Assembly assembly)
    {
        var informational = assembly.GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion;
        if (!string.IsNullOrEmpty(informational))
        {
            var plus = informational.IndexOf('+');
            return plus < 0 ? informational : informational[..plus];
        }

        return assembly.GetName().Version?.ToString(3) ?? string.Empty;
    }
}
