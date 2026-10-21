// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

// The layer through the package: what the Rust tests hold it to, reached from .NET.
public class SdkSettingsTests
{
    private static AgentRemoteConfig Config(string policy) => Policy.Config(policy, "h");

    [Fact]
    public void LoadDecideDescribe()
    {
        using var layer = new SdkSettings();
        var loaded = layer.Load(System.Text.Encoding.UTF8.GetBytes(Policy.Stop("logs")), Declaring());
        Assert.False(loaded.Settings.SendLogs);
        Assert.Equal(Policy.Full(Policy.Stop("logs")), Policy.Text(loaded.Effective));

        var kept = new List<string>();
        var d = layer.Decide(Config(Policy.HeadSampling(0.25)), loaded.Current, loaded.Current, Declaring(), save: doc => kept.Add(System.Text.Encoding.UTF8.GetString(doc)));
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.Equal(0.25, d.Apply.SamplingRate);
        Assert.False(d.Apply.HasSendLogs); // local: the SDK's own
        Assert.Equal(new[] { Policy.HeadSampling(0.25) }, kept);
        Assert.Equal("""{"head_sampling":{"ratio":0.25}}""", Policy.Overrides(d.Effective));

        var bad = layer.Decide(Config(Policy.Of(("head_sampling", """{"ratio":"0.25"}"""))), d.Current, loaded.Current, Declaring(), save: doc => kept.Add("again"));
        Assert.Equal(RemoteConfigStatuses.Failed, bad.Status.Status);
        Assert.StartsWith("head_sampling:", bad.Status.ErrorMessage);
        Assert.Null(bad.Apply);
        Assert.Single(kept); // refused: nothing to keep
        Assert.Equal("""{"head_sampling":{"ratio":0.25}}""", Policy.Overrides(bad.Effective)); // what is still in force

        var description = layer.Describe(
            new[]
            {
                new KeyValue { Key = "service.name", Value = new AnyValue { StringValue = "checkout" } },
                new KeyValue { Key = "host.name", Value = new AnyValue { StringValue = "h" } },
            },
            Declaring(),
            Now);
        Assert.Equal("service.name", Assert.Single(description.IdentifyingAttributes).Key);
        Assert.Contains(description.NonIdentifyingAttributes, a => a.Key == "host.name");
    }

    // The description says which policy format the SDK reads and which controls its host carries out,
    // so that a server sends an override only to an SDK that lists its control.
    [Fact]
    public void TheDescriptionNamesThePolicyFormatsAndTheControlsTheHostCarriesOut()
    {
        using var layer = new SdkSettings();
        var resource = new[] { new KeyValue { Key = "service.name", Value = new AnyValue { StringValue = "checkout" } } };

        var all = Attributes(layer.Describe(resource, DeclaringAll(), Now));
        Assert.Equal(Policy.V1, all["logfire.opamp.config_format"].StringValue);
        Assert.Equal(
            new[]
            {
                "head_sampling", "signal_exports.traces", "signal_exports.metrics", "signal_exports.logs", "sampling_rules",
                "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug", "app_settings",
                "instrumentations", "scrubbing",
            },
            Strings(all["logfire.opamp.supported_controls"]));

        // Only the controls the host carries out: debug needs a clock, app_settings needs custom.
        Assert.DoesNotContain("debug", Strings(Attributes(layer.Describe(resource, DeclaringAll()))["logfire.opamp.supported_controls"]));
        Assert.DoesNotContain("app_settings", Strings(Attributes(layer.Describe(resource, Declaring(), Now))["logfire.opamp.supported_controls"]));
        Assert.Equal(new[] { "head_sampling" }, Strings(Attributes(layer.Describe(resource, new Host { Live = { "sampling_rate" } }, Now))["logfire.opamp.supported_controls"]));
        var none = Attributes(layer.Describe(resource));
        Assert.Empty(Strings(none["logfire.opamp.supported_controls"]));
        Assert.Equal(Policy.V1, none["logfire.opamp.config_format"].StringValue);

        static Dictionary<string, AnyValue> Attributes(AgentDescription d) => d.NonIdentifyingAttributes.ToDictionary(a => a.Key, a => a.Value);
        static string[] Strings(AnyValue v) => [.. v.ArrayValue.Values.Select(x => x.StringValue)];
    }

    // A config whose only file is not the policy, or that has another file beside it, is refused.
    [Fact]
    public void AConfigWithoutThePolicyAsItsOnlyFileIsRefused()
    {
        using var layer = new SdkSettings();
        var other = new AgentRemoteConfig
        {
            ConfigHash = ByteString.CopyFromUtf8("h"),
            Config = new AgentConfigMap
            {
                ConfigMap = { ["settings.json"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(Policy.Local), ContentType = "application/json" } },
            },
        };
        var none = layer.Decide(other, ByteString.Empty, ByteString.Empty, Declaring());
        Assert.Equal(RemoteConfigStatuses.Failed, none.Status.Status);
        Assert.StartsWith("config_map:", none.Status.ErrorMessage);

        var two = Config(Policy.HeadSampling(0.25));
        two.Config.ConfigMap["settings.json"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8("{}"), ContentType = "application/json" };
        var both = layer.Decide(two, ByteString.Empty, ByteString.Empty, Declaring());
        Assert.Equal(RemoteConfigStatuses.Failed, both.Status.Status);
        Assert.StartsWith("config_map:", both.Status.ErrorMessage);
        Assert.Null(both.Apply);
    }

    // A save that keeps nothing, for a host that has somewhere to keep state.
    private static void Keep(byte[] doc)
    {
    }

    // Decide, then commit: the integration saves what the layer asks it to keep, and the layer
    // answers from how that went, the same in every language.
    [Fact]
    public void TheSaveTheLayerAsksForDecidesTheAnswer()
    {
        using var layer = new SdkSettings();
        var current = ByteString.CopyFromUtf8(Policy.HeadSampling(0.5));
        var failed = layer.Decide(Config(Policy.HeadSampling(0.25)), current, current, Declaring(), save: _ => throw new IOException("No space left on device"));
        Assert.Equal(RemoteConfigStatuses.Failed, failed.Status.Status);
        Assert.Equal("can't save the settings: No space left on device", failed.Status.ErrorMessage);
        Assert.Null(failed.Apply);
        Assert.Equal(current, failed.Current);
        Assert.Equal("""{"head_sampling":{"ratio":0.5}}""", Policy.Overrides(failed.Effective));

        // Whatever stops the save refuses the config.
        var odd = layer.Decide(Config(Policy.HeadSampling(0.25)), current, current, Declaring(), save: _ => throw new InvalidOperationException("odd"));
        Assert.Equal("can't save the settings: odd", odd.Status.ErrorMessage);

        // Nowhere to keep state: a live change is applied, a next-start one refused.
        var live = layer.Decide(Config(Policy.HeadSampling(0.25)), current, current, Declaring());
        Assert.Equal(RemoteConfigStatuses.Applied, live.Status.Status);
        var redis = Policy.Of(("instrumentations", """{"disabled":["redis"]}"""));
        var later = layer.Decide(Config(redis), current, current, Declaring());
        Assert.Equal(RemoteConfigStatuses.Failed, later.Status.Status);
        Assert.Equal("instrumentations: takes effect at the next start, which needs somewhere to keep state", later.Status.ErrorMessage);
        var saved = layer.Decide(Config(redis), current, current, Declaring(), save: Keep);
        Assert.Equal(RemoteConfigStatuses.Applied, saved.Status.Status);
        Assert.Equal(new[] { "deactivate_instrumentations" }, saved.PendingRestart);
    }

    // An override that the host does not carry out refuses the whole policy, and nothing changes.
    [Fact]
    public void AnOverrideTheHostDoesNotCarryOutIsRefused()
    {
        using var layer = new SdkSettings();
        var host = new Host { Live = { "sampling_rate" } };
        var current = ByteString.CopyFromUtf8(Policy.HeadSampling(0.5));

        var logs = layer.Decide(Config(Policy.Of(("head_sampling", """{"ratio":0.25}"""), ("signal_exports.logs", """{"enabled":false}"""))), current, current, host, save: Keep);
        Assert.Equal(RemoteConfigStatuses.Failed, logs.Status.Status);
        Assert.Equal("signal_exports.logs: this SDK cannot apply the requested override", logs.Status.ErrorMessage);
        Assert.Null(logs.Apply);
        Assert.Equal(current, logs.Current);
        Assert.Equal("""{"head_sampling":{"ratio":0.5}}""", Policy.Overrides(logs.Effective));

        var instrumentations = layer.Decide(Config(Policy.Of(("instrumentations", """{"disabled":["httpclient"]}"""))), current, current, host, save: Keep);
        Assert.Equal(RemoteConfigStatuses.Failed, instrumentations.Status.Status);
        Assert.Equal("instrumentations: disabled: this SDK cannot apply the requested override", instrumentations.Status.ErrorMessage);
        Assert.Null(instrumentations.Apply);

        // The same controls local need nothing of the host.
        var every = Policy.OfEvery(("head_sampling", """{"ratio":0.25}"""));
        var local = layer.Decide(Config(every), current, current, host, save: Keep);
        Assert.Equal(RemoteConfigStatuses.Applied, local.Status.Status);
        Assert.Equal(Policy.Full(every), Policy.Text(local.Effective));

        // A host naming what is no setting has a bug: an error, saying which.
        var e = Assert.ThrowsAny<Exception>(() => layer.Load(null, new Host { Live = { "send_metric" } }));
        Assert.Contains("send_metric", e.Message);
    }

    [Fact]
    public void ALogLevelArrivesAsTheLowestSeverityNumberToExport()
    {
        using var layer = new SdkSettings();
        var d = layer.Decide(Config(Policy.Of(("log_levels", """{"level":"warn"}"""))), ByteString.Empty, ByteString.Empty, Declaring());
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.True(d.Apply.HasSendLogsMinSeverity);
        Assert.Equal(13u, d.Apply.SendLogsMinSeverity);
        Assert.Equal("""{"log_levels":{"level":"warn"}}""", Policy.Overrides(d.Effective));
        // Local: every severity.
        Assert.False(layer.Load(System.Text.Encoding.UTF8.GetBytes(Policy.Local), Declaring()).Settings.HasSendLogsMinSeverity);
    }

    // A host that declares every setting: each live one at once, each next-start one at the next start.
    private static Host Declaring() => new()
    {
        Live =
        {
            "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
            "sampling_rate", "sampling_rules", "sampling_traces_per_second", "scrub_defaults", "scrub_patterns",
            "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics",
            "send_traces",
        },
        NextStart = { "deactivate_all_instrumentations", "deactivate_instrumentations" },
    };

    private const ulong Now = 1_791_979_200_000; // 2026-10-14T12:00:00Z

    [Fact]
    public void TheSettingsThatMatchTelemetryArriveTypedWhereTheHostDeclaresThem()
    {
        using var layer = new SdkSettings();
        var doc = Policy.Of(
            ("sampling_rules", """{"rules":[{"rate":0,"kind":"server","path":"/health*"}]}"""),
            ("log_levels", """{"scopes":{"com.acme.db*":"debug"}}"""),
            ("attribute_redaction", """{"drop":["*token*"],"mask":["User.Email"]}"""),
            ("trace_rate_limit", """{"traces_per_second":50}"""),
            ("log_rate_limit", """{"records_per_second":0.5}"""));
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
        // Not declared: the policy is refused, and nothing is in force.
        var plain = layer.Decide(Config(doc), ByteString.Empty, ByteString.Empty, new Host { Live = { "sampling_rate" } });
        Assert.Equal(RemoteConfigStatuses.Failed, plain.Status.Status);
        Assert.Equal("sampling_rules: rules: this SDK cannot apply the requested override", plain.Status.ErrorMessage);
        Assert.Null(plain.Apply);
        Assert.Equal("{}", Policy.Overrides(plain.Effective));
    }

    [Fact]
    public void DebugModeArrivesFoldedInAndARefreshAtItsEndEndsIt()
    {
        using var layer = new SdkSettings();
        var doc = Policy.Of(("head_sampling", """{"ratio":0.1}"""), ("debug", """{"until":"2026-10-14T12:30:00Z","log_level":"info"}"""));
        var d = layer.Decide(Config(doc), ByteString.Empty, ByteString.Empty, Declaring(), Now, save: Keep);
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        Assert.Equal(1.0, d.Apply.SamplingRate);
        Assert.Equal(9u, d.Apply.SendLogsMinSeverity);
        const ulong end = Now + (30 * 60_000);
        Assert.Equal(end, d.RefreshAtUnixMs);
        var r = layer.Refresh(d.Current, ByteString.Empty, end, Declaring());
        Assert.Equal(0.1, r.Settings.SamplingRate);
        Assert.Equal(0ul, r.RefreshAtUnixMs);
        Assert.Equal("""{"head_sampling":{"ratio":0.1}}""", Policy.Overrides(r.Effective));
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

    // Every case of testdata/start.json through the wrapper: the settings to start with come from the
    // policy of the last config applied in the core's saved state, else from the policy kept before.
    [Fact]
    public void TheStartVectors()
    {
        using var layer = new SdkSettings();
        foreach (var c in Cases("start.json"))
        {
            var name = c.GetProperty("name").GetString();
            var coreState = c.TryGetProperty("core_state", out var h) ? Convert.FromHexString(h.GetString()!) : null;
            var saved = c.TryGetProperty("saved", out var s) ? System.Text.Encoding.UTF8.GetBytes(s.GetString()!) : null;
            var loaded = layer.Load(saved, DeclaringAll(), Now, coreState);
            var want = c.GetProperty("want").GetString();
            Assert.True(want == loaded.Current.ToStringUtf8(), $"{name}: started from {loaded.Current.ToStringUtf8()}");
            Assert.True(c.GetProperty("warns").GetBoolean() == (loaded.Warning.Length > 0), $"{name}: warning '{loaded.Warning}'");
        }
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
