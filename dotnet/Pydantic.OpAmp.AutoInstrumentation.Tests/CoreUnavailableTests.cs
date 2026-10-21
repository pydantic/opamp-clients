// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// A core that can't be loaded: the plugin says why once, on standard error, and every hook it
/// gives automatic instrumentation goes through without an agent, so the SDK runs unmanaged.
/// </summary>
[Collection(nameof(StandardError))]
public class CoreUnavailableTests
{
    private const string Why = "DllNotFoundException: Unable to load shared library 'opamp_native'";

    [Fact]
    public void ACoreThatCantBeLoadedIsSaidOnceAndTheSdkRunsUnmanaged()
    {
        // A server to be managed by: with a core, the plugin would start an agent for it.
        var vars = new Dictionary<string, string?>
        {
            ["OTEL_OPAMP_ENDPOINT"] = "http://127.0.0.1:1/v1/opamp",
            ["OTEL_OPAMP_STATE_DIR"] = "none",
        };
        var before = vars.ToDictionary(v => v.Key, v => Environment.GetEnvironmentVariable(v.Key));
        var was = Console.Error;
        var err = new StringWriter();
        var plugin = new Plugin { CoreUnavailable = () => Why };
        Console.SetError(err);
        try
        {
            foreach (var (k, v) in vars)
            {
                Environment.SetEnvironmentVariable(k, v);
            }

            plugin.Initializing();
            var resource = plugin.ConfigureResource(ResourceBuilder.CreateEmpty()).Build();
            Assert.DoesNotContain(resource.Attributes, a => a.Key == "service.instance.id");
            plugin.ConfigureTracesOptions(new OtlpExporterOptions());
            using var source = new ActivitySource("app");
            using var tracerProvider = plugin.AfterConfigureTracerProvider(
                Sdk.CreateTracerProviderBuilder().AddSource("app").SetResourceBuilder(ResourceBuilder.CreateEmpty())).Build();
            plugin.TracerProviderInitialized(tracerProvider);
            plugin.Initialized();
            using (var work = source.StartActivity("work"))
            {
                Assert.NotNull(work);
                Assert.True(work.Recorded, "the SDK records the application's span on its own sampler");
            }
        }
        finally
        {
            Console.SetError(was);
            foreach (var (k, v) in before)
            {
                Environment.SetEnvironmentVariable(k, v);
            }
        }

        var said = err.ToString().Split('\n').Select(l => l.TrimEnd('\r')).Where(l => l.Contains("OpAMP", StringComparison.Ordinal)).ToList();
        var rid = System.Runtime.InteropServices.RuntimeInformation.RuntimeIdentifier;
        Assert.Equal([$"OpAMP: the core can't be loaded on {rid}; the SDK runs unmanaged: {Why}"], said);
    }
}
