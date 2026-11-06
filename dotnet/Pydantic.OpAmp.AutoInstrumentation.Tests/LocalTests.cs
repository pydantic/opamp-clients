// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Globalization;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// The local report (contracts' logfire-sdk-local/v1): what the SDK automatic instrumentation builds
/// does without the server, as the host's <c>local</c> carries it to the layer; and each
/// instrumentation's scope in the inventory.
/// </summary>
public class LocalTests
{
    private static Agent Prepared(Dictionary<string, string?> env)
    {
        env.TryAdd("OTEL_OPAMP_ENDPOINT", "http://127.0.0.1:1/v1/opamp");
        env.TryAdd("OTEL_OPAMP_STATE_DIR", "none");
        return Agent.Prepare(n => env.GetValueOrDefault(n), (_, _) => { })!;
    }

    [Theory]
    [InlineData(null, null, "1")] // parentbased_always_on, the SDK's default
    [InlineData("always_on", null, "1")]
    [InlineData("always_off", null, "0")]
    [InlineData("parentbased_always_off", null, "0")]
    [InlineData("traceidratio", "0.25", "0.25")]
    [InlineData("parentbased_traceidratio", "0.1", "0.1")]
    [InlineData("traceidratio", "2", "1")] // out of range: 1, as the sampler built
    [InlineData("jaeger_remote", null, "1")] // not one it builds: the default it builds
    public void TheSamplingRateIsTheRatioOfTheSamplerTheVariablesName(string? sampler, string? arg, string rate)
    {
        var env = new Dictionary<string, string?> { ["OTEL_TRACES_SAMPLER"] = sampler, ["OTEL_TRACES_SAMPLER_ARG"] = arg };
        Assert.Equal(rate, Variables.Decimal(Variables.SamplingRateFromEnv(n => env.GetValueOrDefault(n))));
        var agent = Prepared(env);
        Assert.False(agent.HostNow(providersBuilt: true).Local.HasSamplingRate); // no tracer provider yet
        using var provider = OpenTelemetry.Sdk.CreateTracerProviderBuilder().AddSource("local").Let(agent.WrapSampler).Build();
        Assert.Equal(rate, agent.HostNow(providersBuilt: true).Local.SamplingRate);
    }

    [Fact]
    public void ASamplerTheBuilderWasGivenIsNotTold()
    {
        var agent = Prepared([]);
        using var provider = OpenTelemetry.Sdk.CreateTracerProviderBuilder().AddSource("own").SetSampler(new AlwaysOffSampler())
            .Let(agent.WrapSampler).Build();
        Assert.False(agent.HostNow(providersBuilt: true).Local.HasSamplingRate);
    }

    [Theory]
    [InlineData(1, "1")]
    [InlineData(0, "0")]
    [InlineData(0.25, "0.25")]
    [InlineData(0.00001, "0.00001")]
    [InlineData(1e-7, "0.0000001")]
    [InlineData(1.5e-7, "0.00000015")]
    public void ARatioIsDecimalTextWithoutAnExponent(double x, string text)
    {
        Assert.Equal(text, Variables.Decimal(x));
        Assert.Equal(x, double.Parse(text, CultureInfo.InvariantCulture));
    }

    [Fact]
    public void EachSignalIsExportedWhereItsVariablesNameAnExporter()
    {
        var defaults = Prepared([]).HostNow(providersBuilt: true).Local;
        Assert.True(defaults.SendTraces && defaults.SendMetrics && defaults.SendLogs);
        var some = Prepared(new() { ["OTEL_TRACES_EXPORTER"] = "none", ["OTEL_METRICS_EXPORTER"] = "console", ["OTEL_DOTNET_AUTO_LOGS_ENABLED"] = "false" })
            .HostNow(providersBuilt: true).Local;
        Assert.True(some.HasSendTraces && some.HasSendMetrics && some.HasSendLogs);
        Assert.Equal([false, true, false], new[] { some.SendTraces, some.SendMetrics, some.SendLogs });
        // The SDK has no level of its own.
        Assert.False(some.HasSendLogsLevel);
        // Before the providers are built nothing is told: the host loaded at first carries none.
        Assert.Null(Prepared([]).HostNow(providersBuilt: false).Local);
    }

    [Fact]
    public void WhatTheApplicationSetsUpIsLeftOut()
    {
        // OTEL_DOTNET_AUTO_SETUP_SDK=false: traces and metrics are the application's providers'.
        var local = Prepared(new() { ["OTEL_DOTNET_AUTO_SETUP_SDK"] = "false" }).HostNow(providersBuilt: true).Local;
        Assert.False(local.HasSendTraces);
        Assert.False(local.HasSendMetrics);
        Assert.True(local.HasSendLogs && local.SendLogs);
    }

    [Fact]
    public void EachInstrumentationIsListedWithItsScopeWhereItIsKnown()
    {
        var agent = Prepared([]);
        agent.loadedAssemblies = InventoryTests.EveryLibrary;
        var scopes = agent.Inventory().Instrumentations.ToDictionary(i => i.Name, i => i.Scope);
        Assert.Equal("System.Net.Http", scopes["httpclient"]);
        Assert.Equal("Microsoft.AspNetCore", scopes["aspnetcore"]);
        Assert.Equal("OpenTelemetry.Instrumentation.Runtime", scopes["netruntime"]); // metrics only: its meter
        Assert.Equal(string.Empty, scopes["ilogger"]); // logs under the application's categories
        Assert.All(Inventories.Scopes.Keys, name => Assert.Contains(name, scopes.Keys));
    }
}
