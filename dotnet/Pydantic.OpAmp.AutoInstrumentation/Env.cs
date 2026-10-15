// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;

namespace Pydantic.OpAmp.AutoInstrumentation;

/// <summary>The variables automatic instrumentation reads that the plugin sets.</summary>
internal static class Env
{
    /// <summary>
    /// The instrumentations automatic instrumentation 1.17 has, by signal: each has a variable
    /// <c>OTEL_DOTNET_AUTO_{SIGNAL}_{NAME}_INSTRUMENTATION_ENABLED</c>.
    /// </summary>
    internal static readonly IReadOnlyDictionary<string, string[]> Instrumentations = new Dictionary<string, string[]>
    {
        ["TRACES"] =
        [
            "ADONET", "ASPNET", "ASPNETCORE", "AZURE", "ELASTICSEARCH", "ELASTICTRANSPORT", "ENTITYFRAMEWORKCORE", "GRAPHQL",
            "GRPCNETCLIENT", "HTTPCLIENT", "KAFKA", "MASSTRANSIT", "MONGODB", "MYSQLCONNECTOR", "MYSQLDATA", "NPGSQL",
            "NSERVICEBUS", "ORACLEMDA", "QUARTZ", "RABBITMQ", "SQLCLIENT", "SQLITE", "STACKEXCHANGEREDIS", "WCFCLIENT", "WCFCORE",
            "WCFSERVICE",
        ],
        ["METRICS"] = ["ASPNET", "ASPNETCORE", "HTTPCLIENT", "NETRUNTIME", "NPGSQL", "NSERVICEBUS", "PROCESS", "QUARTZ", "SQLCLIENT"],
        ["LOGS"] = ["ILOGGER", "LOG4NET", "NLOG"],
    };

    /// <summary>
    /// The exporters automatic instrumentation builds for each signal, by their names in
    /// <c>OTEL_{SIGNAL}_EXPORTER</c> (<c>none</c> builds none).
    /// </summary>
    internal static readonly IReadOnlyDictionary<Signal, string[]> ExporterNames = new Dictionary<Signal, string[]>
    {
        [Signal.Traces] = ["otlp", "zipkin", "console"],
        [Signal.Metrics] = ["otlp", "prometheus", "console"],
        [Signal.Logs] = ["otlp", "console"],
    };

    /// <summary>
    /// The instrumentations it can run, as <c>deactivate_instrumentations</c> names them: each
    /// one's name in its variable, lowercase, once for every signal it has. ASP.NET and WCF
    /// services are .NET Framework's alone, which the plugin doesn't run on.
    /// </summary>
    internal static IEnumerable<string> Runnable =>
        Instrumentations.Values.SelectMany(names => names).Where(n => n is not ("ASPNET" or "WCFSERVICE"))
            .Select(n => n.ToLowerInvariant()).Distinct().Order(StringComparer.Ordinal);

    /// <summary>Whether automatic instrumentation sets up the signal at all (<c>OTEL_DOTNET_AUTO_{SIGNAL}_ENABLED</c>, true unless false).</summary>
    public static bool Enabled(Func<string, string?> getenv, Signal signal) =>
        !string.Equals(getenv($"OTEL_DOTNET_AUTO_{signal.ToString().ToUpperInvariant()}_ENABLED")?.Trim(), "false", StringComparison.OrdinalIgnoreCase);

    /// <summary>The exporters <c>OTEL_{SIGNAL}_EXPORTER</c> (default <c>otlp</c>) builds for the signal, as it names them; one it doesn't know builds none.</summary>
    public static IEnumerable<string> Exporters(Func<string, string?> getenv, Signal signal) =>
        (getenv($"OTEL_{signal.ToString().ToUpperInvariant()}_EXPORTER") ?? "otlp")
            .Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries)
            .Where(ExporterNames[signal].Contains);

    /// <summary>
    /// The variables that switch off the instrumentations the settings name, for automatic
    /// instrumentation to read as it starts. A name is an instrumentation's name in its variable,
    /// lowercase (<c>httpclient</c>), and switches it off for every signal. Upstream's names have no
    /// punctuation, so the <c>.</c>, <c>-</c> and <c>_</c> the settings allow are dropped:
    /// <c>http_client</c> and <c>asp-net-core</c> are <c>httpclient</c> and <c>aspnetcore</c>. Switching
    /// every one off sets each one's variable: one set to true would win over the general switch.
    /// </summary>
    public static IReadOnlyDictionary<string, string> Switches(Settings s)
    {
        var vars = new SortedDictionary<string, string>(StringComparer.Ordinal);
        if (s.HasDeactivateAllInstrumentations && s.DeactivateAllInstrumentations)
        {
            vars["OTEL_DOTNET_AUTO_INSTRUMENTATION_ENABLED"] = "false";
            foreach (var (signal, names) in Instrumentations)
            {
                vars[$"OTEL_DOTNET_AUTO_{signal}_INSTRUMENTATION_ENABLED"] = "false";
                foreach (var name in names)
                {
                    vars[$"OTEL_DOTNET_AUTO_{signal}_{name}_INSTRUMENTATION_ENABLED"] = "false";
                }
            }
        }

        foreach (var name in s.DeactivateInstrumentations?.Names_ ?? [])
        {
            var upstream = string.Concat(name.Where(c => c is not ('.' or '-' or '_'))).ToUpperInvariant();
            if (upstream.Length == 0)
            {
                continue;
            }

            foreach (var signal in Instrumentations.Keys)
            {
                vars[$"OTEL_DOTNET_AUTO_{signal}_{upstream}_INSTRUMENTATION_ENABLED"] = "false";
            }
        }

        return vars;
    }
}
