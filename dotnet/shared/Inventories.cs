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

    /// <summary>An exporter of <paramref name="signal"/>, as the layer takes it.</summary>
    public static Exporter Exporter(Signal signal, string name, bool gated) =>
        new() { Signal = signal.ToString().ToLowerInvariant(), Name = name, Gated = gated };

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
