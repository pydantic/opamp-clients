// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

// The layer through the package: what the Rust tests hold it to, reached from .NET.
public class SdkSettingsTests
{
    private static AgentRemoteConfig Config(string body) => new()
    {
        ConfigHash = ByteString.CopyFromUtf8("h"),
        Config = new AgentConfigMap
        {
            ConfigMap = { ["elastic"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(body), ContentType = "application/json" } },
        },
    };

    private static string Effective(EffectiveConfig e) => e.ConfigMap.ConfigMap["elastic"].Body.ToStringUtf8();

    // What the effective config names as set aside (its elastic.set_aside entry); null without one.
    private static string? SetAside(EffectiveConfig e)
    {
        Assert.All(e.ConfigMap.ConfigMap.Keys, k => Assert.Contains(k, new[] { "elastic", "elastic.set_aside" }));
        if (!e.ConfigMap.ConfigMap.TryGetValue("elastic.set_aside", out var f))
        {
            return null;
        }

        Assert.Equal("application/json", f.ContentType);
        return f.Body.ToStringUtf8();
    }

    [Fact]
    public void LoadDecideDescribe()
    {
        using var layer = new SdkSettings();
        var loaded = layer.Load(System.Text.Encoding.UTF8.GetBytes("{\"send_logs\":\"false\"}"), Declaring());
        Assert.False(loaded.Settings.SendLogs);
        Assert.Equal("{\"send_logs\":\"false\"}", Effective(loaded.Effective));

        var kept = new List<string>();
        var d = layer.Decide(Config("{\"sampling_rate\":\"0.25\"}"), loaded.Current, loaded.Current, Declaring(), save: doc => kept.Add(System.Text.Encoding.UTF8.GetString(doc)));
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.Equal(0.25, d.Apply.SamplingRate);
        Assert.False(d.Apply.HasSendLogs); // left out: the SDK's own
        Assert.Equal(new[] { "{\"sampling_rate\":\"0.25\"}" }, kept);
        Assert.Equal("{\"sampling_rate\":\"0.25\"}", Effective(d.Effective));
        Assert.Null(SetAside(d.Effective)); // nothing set aside, no entry

        var bad = layer.Decide(Config("{\"sampling_rate\":\"x\"}"), d.Current, loaded.Current, Declaring(), save: doc => kept.Add("again"));
        Assert.Equal(RemoteConfigStatuses.Failed, bad.Status.Status);
        Assert.StartsWith("sampling_rate:", bad.Status.ErrorMessage);
        Assert.Null(bad.Apply);
        Assert.Single(kept); // refused: nothing to keep

        var description = layer.Describe(new[]
        {
            new KeyValue { Key = "service.name", Value = new AnyValue { StringValue = "checkout" } },
            new KeyValue { Key = "host.name", Value = new AnyValue { StringValue = "h" } },
        });
        Assert.Single(description.IdentifyingAttributes);
        Assert.Single(description.NonIdentifyingAttributes);
    }

    // A save that keeps nothing, for a host that has somewhere to keep state.
    private static void Keep(byte[] doc)
    {
    }

    // Decide, then commit (#215): the integration saves what the layer asks it to keep, and the layer
    // answers from how that went, the same in every language.
    [Fact]
    public void TheSaveTheLayerAsksForDecidesTheAnswer()
    {
        using var layer = new SdkSettings();
        var current = ByteString.CopyFromUtf8("{\"sampling_rate\":\"0.5\"}");
        var failed = layer.Decide(Config("{\"sampling_rate\":\"0.25\"}"), current, current, Declaring(), save: _ => throw new IOException("No space left on device"));
        Assert.Equal(RemoteConfigStatuses.Failed, failed.Status.Status);
        Assert.Equal("can't save the settings: No space left on device", failed.Status.ErrorMessage);
        Assert.Null(failed.Apply);
        Assert.Equal(current, failed.Current);
        Assert.Equal("{\"sampling_rate\":\"0.5\"}", Effective(failed.Effective));

        // Whatever stops the save refuses the config.
        var odd = layer.Decide(Config("{\"sampling_rate\":\"0.25\"}"), current, current, Declaring(), save: _ => throw new InvalidOperationException("odd"));
        Assert.Equal("can't save the settings: odd", odd.Status.ErrorMessage);

        // Nowhere to keep state: a live change is applied, a next-start one refused.
        var live = layer.Decide(Config("{\"sampling_rate\":\"0.25\"}"), current, current, Declaring());
        Assert.Equal(RemoteConfigStatuses.Applied, live.Status.Status);
        var later = layer.Decide(Config("{\"opamp_polling_interval\":\"1m\"}"), current, current, Declaring());
        Assert.Equal("opamp_polling_interval take effect at the next start, which needs somewhere to keep state", later.Status.ErrorMessage);
        var saved = layer.Decide(Config("{\"opamp_polling_interval\":\"1m\"}"), current, current, Declaring(), save: Keep);
        Assert.Equal(RemoteConfigStatuses.Applied, saved.Status.Status);
        Assert.Equal(new[] { "opamp_polling_interval" }, saved.PendingRestart);
    }

    [Fact]
    public void AHostThatDoesntDeclareTheInstrumentationSwitchSetsItAside()
    {
        using var layer = new SdkSettings();
        var host = new Host { Live = { "sampling_rate" } };
        var d = layer.Decide(Config("{\"deactivate_instrumentations\":\"httpclient\",\"x\":\"1\"}"), ByteString.Empty, ByteString.Empty, host);
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.Equal("{}", Effective(d.Effective));
        // Not declared, so not carried out; a setting nothing knows is told apart from it.
        Assert.Equal(new[] { "deactivate_instrumentations" }, d.Apply.Unsupported);
        Assert.Equal(new[] { "x" }, d.Apply.Ignored);
        // Named for the server beside what is in force (#215), by Decide, Load and Refresh alike,
        // and still after a FAILED, which leaves the document in force.
        const string names = "{\"ignored\":[\"x\"],\"unsupported\":[\"deactivate_instrumentations\"]}";
        Assert.Equal(names, SetAside(d.Effective));
        Assert.Equal("", d.Status.ErrorMessage);
        Assert.Equal(names, SetAside(layer.Load(d.Current.ToByteArray(), host).Effective));
        Assert.Equal(names, SetAside(layer.Refresh(d.Current, ByteString.Empty, 1, host).Effective));
        var failed = layer.Decide(Config("{\"sampling_rate\":\"7\"}"), d.Current, ByteString.Empty, host);
        Assert.Equal(RemoteConfigStatuses.Failed, failed.Status.Status);
        Assert.Equal(names, SetAside(failed.Effective));
        // A host naming what is no setting has a bug: an error, saying which.
        var e = Assert.ThrowsAny<Exception>(() => layer.Load(null, new Host { Live = { "send_metric" } }));
        Assert.Contains("send_metric", e.Message);
    }

    [Fact]
    public void ALogLevelArrivesAsTheLowestSeverityNumberToExport()
    {
        using var layer = new SdkSettings();
        var d = layer.Decide(Config("{\"send_logs_level\":\"Warning\"}"), ByteString.Empty, ByteString.Empty, Declaring());
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.True(d.Apply.HasSendLogsMinSeverity);
        Assert.Equal(13u, d.Apply.SendLogsMinSeverity);
        Assert.Equal("{\"send_logs_level\":\"warn\"}", Effective(d.Effective));
        // Unset: every severity.
        Assert.False(layer.Load(System.Text.Encoding.UTF8.GetBytes("{}"), Declaring()).Settings.HasSendLogsMinSeverity);
    }

    // A host that declares every setting: each live one at once, each next-start one at the next start.
    private static Host Declaring() => new()
    {
        Live =
        {
            "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
            "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
            "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces",
        },
        NextStart = { "deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval" },
    };

    private const ulong Now = 1_791_979_200_000; // 2026-10-14T12:00:00Z

    [Fact]
    public void TheSettingsThatMatchTelemetryArriveTypedWhereTheHostDeclaresThem()
    {
        using var layer = new SdkSettings();
        const string doc = "{\"sampling_rules\":[{\"rate\":0,\"kind\":\"server\",\"path\":\"/health*\"}],"
            + "\"send_logs_level_by_scope\":\"com.acme.db*=debug\",\"drop_attributes\":\"*token*\","
            + "\"mask_attributes\":\"User.Email\",\"sampling_traces_per_second\":\"50\",\"send_logs_per_second\":\"0.5\"}";
        var d = layer.Decide(Config(doc), ByteString.Empty, ByteString.Empty, Declaring(), Now);
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        var s = d.Apply;
        var rule = Assert.Single(s.SamplingRules);
        Assert.Equal(0.0, rule.Rate);
        Assert.Equal(SpanKind.Server, rule.Kind);
        Assert.Equal("/health*", rule.Path);
        var level = Assert.Single(s.SendLogsScopeLevels);
        Assert.Equal(("com.acme.db*", 5u), (level.Pattern, level.MinSeverity));
        Assert.Equal(new[] { "*token*" }, s.DropAttributes);
        Assert.Equal(new[] { "user.email" }, s.MaskAttributes);
        Assert.Equal((50.0, 0.5), (s.SamplingTracesPerSecond, s.SendLogsPerSecond));
        // Undeclared: ignored, never in force.
        var plain = layer.Decide(Config(doc), ByteString.Empty, ByteString.Empty, new Host { Live = { "sampling_rate" } });
        Assert.Empty(plain.Apply.SamplingRules);
        Assert.Equal(6, plain.Apply.Unsupported.Count);
        Assert.Equal("{}", Effective(plain.Effective));
    }

    [Fact]
    public void DebugModeArrivesFoldedInAndARefreshAtItsEndEndsIt()
    {
        using var layer = new SdkSettings();
        const string doc = "{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:30:00Z\",\"debug_send_logs_level\":\"info\"}";
        var d = layer.Decide(Config(doc), ByteString.Empty, ByteString.Empty, Declaring(), Now, save: Keep);
        Assert.Equal(1.0, d.Apply.SamplingRate);
        Assert.Equal(9u, d.Apply.SendLogsMinSeverity);
        const ulong end = Now + (30 * 60_000);
        Assert.Equal(end, d.RefreshAtUnixMs);
        var r = layer.Refresh(d.Current, ByteString.Empty, end, Declaring());
        Assert.Equal(0.1, r.Settings.SamplingRate);
        Assert.Equal(0ul, r.RefreshAtUnixMs);
        Assert.Equal("{\"sampling_rate\":\"0.1\"}", Effective(r.Effective));
        Assert.Equal(end, layer.Load(d.Current.ToByteArray(), Declaring(), Now).RefreshAtUnixMs);
    }

    [Fact]
    public void ExportHealthIsSentAtTheFirstCallAndOnAFailureAndHeldWhileNothingChanges()
    {
        using var layer = new SdkSettings();
        var first = layer.Health(new ExportCounts(), null, Now);
        Assert.True(first.Send);
        Assert.Equal(3, first.Exporters.ComponentHealthMap.Count);
        Assert.Equal("ok", first.Exporters.Status);
        Assert.False(layer.Health(new ExportCounts(), first, Now + 30_000).Send);
        var failed = layer.Health(
            new ExportCounts
            {
                Traces = new SignalCounts { ExportsFailed = 1, ItemsFailed = 512, LastFailed = true, LastError = "connection refused" },
            },
            first,
            Now + 30_000,
            new Settings { SendLogs = false });
        Assert.True(failed.Send);
        Assert.False(failed.Exporters.Healthy);
        Assert.Equal("connection refused", failed.Exporters.ComponentHealthMap["traces"].LastError);
        Assert.Equal("off", failed.Exporters.ComponentHealthMap["logs"].Status);
    }

    // Every case of testdata/components.json through the wrapper: each component the case gives
    // compared whole, and the four always there.
    [Fact]
    public void TheInventoryVectors()
    {
        using var layer = new SdkSettings();
        foreach (var c in Cases("components.json"))
        {
            var name = c.GetProperty("name").GetString();
            var got = layer.Components(InventoryOf(c), HostOf(c), c.TryGetProperty("now_unix_ms", out var n) ? n.GetUInt64() : 0);
            Assert.True(got.Hash.IsEmpty, name);
            Assert.Equal(new[] { "exporters", "instrumentations", "sdk", "settings" }, got.Components.Keys.Order(StringComparer.Ordinal));
            var want = c.GetProperty("want");
            if (want.TryGetProperty("sdk", out var sdk))
            {
                Assert.Empty(got.Components["sdk"].SubComponentMap);
                Assert.Equal(Texts(sdk), Metadata(got.Components["sdk"]));
            }

            if (want.TryGetProperty("instrumentations", out var instrumentations))
            {
                var expected = instrumentations.EnumerateObject().ToDictionary(
                    p => p.Name, p => p.Value.ValueKind == JsonValueKind.Null ? new Dictionary<string, string>() : new() { ["version"] = p.Value.GetString()! });
                Assert.Equal(expected, got.Components["instrumentations"].SubComponentMap.ToDictionary(kv => kv.Key, kv => Metadata(kv.Value)));
            }

            if (want.TryGetProperty("exporters", out var exporters))
            {
                var expected = exporters.EnumerateObject().ToDictionary(
                    s => s.Name, s => s.Value.EnumerateObject().ToDictionary(e => e.Name, e => e.Value.GetBoolean()));
                var actual = got.Components["exporters"].SubComponentMap.ToDictionary(
                    s => s.Key, s => s.Value.SubComponentMap.ToDictionary(e => e.Key, e => Assert.Single(e.Value.Metadata, m => m.Key == "gated").Value.BoolValue));
                Assert.Equal(expected, actual);
            }

            if (want.TryGetProperty("settings", out var settings))
            {
                Assert.Equal(
                    Texts(settings).ToDictionary(kv => kv.Key, kv => new Dictionary<string, string> { ["applies"] = kv.Value }),
                    got.Components["settings"].SubComponentMap.ToDictionary(kv => kv.Key, kv => Metadata(kv.Value)));
            }
        }
    }

    // The same inventory twice is the same components, to the byte: the core's hash is of them.
    [Fact]
    public void TheSameInventoryIsTheSameBytes()
    {
        using var layer = new SdkSettings();
        var inventory = new Inventory
        {
            Integration = "Pydantic.OpAmp.AutoInstrumentation",
            SdkLanguage = "dotnet",
            Instrumentations = { new Instrumentation { Name = "httpclient", Version = "1.17.0" }, new Instrumentation { Name = "aspnetcore" } },
            Exporters = { new Exporter { Signal = "traces", Name = "otlp", Gated = true } },
        };
        var reversed = inventory.Clone();
        reversed.Instrumentations.Clear();
        reversed.Instrumentations.Add(inventory.Instrumentations.Reverse());
        Assert.Equal(
            layer.Components(inventory, Declaring(), Now).ToByteArray(),
            layer.Components(reversed, Declaring(), Now).ToByteArray());
    }

    private static List<JsonElement> Cases(string file)
    {
        var dir = AppContext.BaseDirectory;
        while (!Directory.Exists(Path.Combine(dir, "testdata", "sdk-settings")))
        {
            dir = Path.GetDirectoryName(dir) ?? throw new DirectoryNotFoundException("testdata/sdk-settings");
        }

        using var doc = JsonDocument.Parse(File.ReadAllText(Path.Combine(dir, "testdata", "sdk-settings", file)));
        return [.. doc.RootElement.GetProperty("cases").EnumerateArray().Select(c => c.Clone())];
    }

    private static Inventory InventoryOf(JsonElement c)
    {
        var i = c.GetProperty("inventory");
        string Text(JsonElement o, string key) => o.TryGetProperty(key, out var v) ? v.GetString()! : string.Empty;
        var inventory = new Inventory
        {
            Integration = Text(i, "integration"),
            IntegrationVersion = Text(i, "integration_version"),
            SdkLanguage = Text(i, "sdk_language"),
            SdkVersion = Text(i, "sdk_version"),
        };
        if (i.TryGetProperty("instrumentations", out var instrumentations))
        {
            inventory.Instrumentations.Add(instrumentations.EnumerateArray().Select(e => new Instrumentation { Name = Text(e, "name"), Version = Text(e, "version") }));
        }

        if (i.TryGetProperty("exporters", out var exporters))
        {
            inventory.Exporters.Add(exporters.EnumerateArray().Select(e => new Exporter
            {
                Signal = Text(e, "signal"),
                Name = Text(e, "name"),
                Gated = e.TryGetProperty("gated", out var g) && g.GetBoolean(),
            }));
        }

        return inventory;
    }

    // Left out: a host that declares every setting, the application's own too: each live one in
    // live, each next-start one in next_start. Given, it declares only what it lists.
    private static Host HostOf(JsonElement c)
    {
        if (!c.TryGetProperty("host", out var h))
        {
            return DeclaringAll();
        }

        IEnumerable<string> Keys(string key) =>
            h.TryGetProperty(key, out var v) ? v.EnumerateArray().Select(e => e.GetString()!) : [];
        return new Host { Live = { Keys("live") }, NextStart = { Keys("next_start") } };
    }

    // Declaring, with the application's own settings (custom) too.
    private static Host DeclaringAll()
    {
        var host = Declaring();
        host.Live.Add("custom");
        return host;
    }

    private static Dictionary<string, string> Texts(JsonElement o) => o.EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString()!);

    private static Dictionary<string, string> Metadata(ComponentDetails d) => d.Metadata.ToDictionary(m => m.Key, m => m.Value.StringValue);
}
