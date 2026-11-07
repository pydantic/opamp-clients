// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text.Json;
using System.Text.Json.Nodes;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Logfire.Fleet.Client.Sdk;
using Logfire.Fleet.Tests;
using Xunit;

namespace Logfire.Fleet.Client.Tests;

// The layer through the package: what the Rust tests hold it to, reached from .NET.
public class SdkSettingsTests
{
    private static AgentRemoteConfig Config(string policy) => Policy.Config(policy, "h");

    /// <summary>The layer's agent flow, stepped by a test, on clocks that move together from <see cref="Now"/>.</summary>
    private sealed class Flow
    {
        public readonly SdkSettings Layer = new();
        public AgentOutput Out;

        public Flow(Host? host, bool stateDir = true, byte[]? coreState = null)
        {
            var start = new AgentStart
            {
                Host = host, StateDir = stateDir, Endpoint = "http://127.0.0.1:1", InstanceUid = ByteString.CopyFrom(new byte[16]),
                NowUnixMs = Now, NowMonoMs = Now,
            };
            if (coreState is not null)
            {
                start.CoreState = ByteString.CopyFrom(coreState);
            }

            this.Out = this.Layer.Start(start);
        }

        public Flow Step(ulong ms, AgentStep input)
        {
            input.Agent = this.Out.Agent;
            input.NowUnixMs = Now + ms;
            input.NowMonoMs = Now + ms;
            this.Out = this.Layer.Step(input);
            return this;
        }

        public Flow Decide(AgentRemoteConfig c, ulong ms = 1) => this.Step(ms, new AgentStep { RemoteConfig = c });

        public RemoteConfigStatus Status => this.Out.Effects.LastOrDefault(e => e.EffectCase == AgentEffect.EffectOneofCase.Status)?.Status ?? new();

        public Settings? Apply => this.Out.Effects.LastOrDefault(e => e.EffectCase == AgentEffect.EffectOneofCase.Apply)?.Apply;

        public ComponentHealth? Health => this.Out.Effects.LastOrDefault(e => e.EffectCase == AgentEffect.EffectOneofCase.Health)?.Health;

        public EffectiveConfig Effective => this.Out.Effective;

        public bool Warned => this.Out.Effects.Any(e => e.EffectCase == AgentEffect.EffectOneofCase.Log && e.Log.Level >= AgentLog.Types.Level.Warn);

        public AgentState State => AgentState.Parser.ParseFrom(this.Out.Agent);
    }

    [Fact]
    public void StartDecideDescribe()
    {
        var f = new Flow(Declaring());
        Assert.Equal(Policy.Full(Policy.Local), Policy.Text(f.Effective));
        f.Decide(Config(Policy.HeadSampling(0.25)));
        Assert.Equal(RemoteConfigStatuses.Applied, f.Status.Status);
        Assert.Equal(0.25, f.Apply!.SamplingRate);
        Assert.False(f.Apply.HasSendLogs); // local: the SDK's own
        Assert.Equal("""{"head_sampling":{"ratio":0.25}}""", Policy.Overrides(f.Effective));

        f.Decide(Config(Policy.Of(("head_sampling", """{"ratio":"0.25"}"""))), 2);
        Assert.Equal(RemoteConfigStatuses.Failed, f.Status.Status);
        Assert.StartsWith("head_sampling:", f.Status.ErrorMessage);
        Assert.Null(f.Apply);
        Assert.Equal("""{"head_sampling":{"ratio":0.25}}""", Policy.Overrides(f.Effective)); // what is still in force

        // A step without an input, or on a state that isn't one, is an error.
        Assert.ThrowsAny<Exception>(() => f.Layer.Step(new AgentStep { Agent = f.Out.Agent }));
        Assert.ThrowsAny<Exception>(() => f.Layer.Step(new AgentStep { Agent = ByteString.CopyFrom(0xff), Timer = new ExportCounts() }));

        var description = f.Layer.Describe(
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
                "instrumentations", "scrubbing", "self_monitoring",
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
        var other = new AgentRemoteConfig
        {
            ConfigHash = ByteString.CopyFromUtf8("h"),
            Config = new AgentConfigMap
            {
                ConfigMap = { ["settings.json"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(Policy.Local), ContentType = "application/json" } },
            },
        };
        var f = new Flow(Declaring()).Decide(other);
        Assert.Equal(RemoteConfigStatuses.Failed, f.Status.Status);
        Assert.StartsWith("config_map:", f.Status.ErrorMessage);

        var two = Config(Policy.HeadSampling(0.25));
        two.Config.ConfigMap["settings.json"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8("{}"), ContentType = "application/json" };
        f.Decide(two, 2);
        Assert.Equal(RemoteConfigStatuses.Failed, f.Status.Status);
        Assert.StartsWith("config_map:", f.Status.ErrorMessage);
        Assert.Null(f.Apply);
    }

    // Nowhere to keep state: a live change is applied, a next-start one refused.
    [Fact]
    public void ANextStartChangeNeedsSomewhereToKeepState()
    {
        Assert.Equal(RemoteConfigStatuses.Applied, new Flow(Declaring(), stateDir: false).Decide(Config(Policy.HeadSampling(0.25))).Status.Status);
        var redis = Policy.Of(("instrumentations", """{"disabled":["redis"]}"""));
        var later = new Flow(Declaring(), stateDir: false).Decide(Config(redis));
        Assert.Equal(RemoteConfigStatuses.Failed, later.Status.Status);
        Assert.Equal("instrumentations: takes effect at the next start, which needs somewhere to keep state", later.Status.ErrorMessage);
        Assert.Equal(RemoteConfigStatuses.Applied, new Flow(Declaring()).Decide(Config(redis)).Status.Status);
    }

    // An override that the host does not carry out refuses the whole policy, and nothing changes.
    [Fact]
    public void AnOverrideTheHostDoesNotCarryOutIsRefused()
    {
        var host = new Host { Live = { "sampling_rate" } };
        var f = new Flow(host).Decide(Config(Policy.HeadSampling(0.5)));
        Assert.Equal(RemoteConfigStatuses.Applied, f.Status.Status);

        f.Decide(Config(Policy.Of(("head_sampling", """{"ratio":0.25}"""), ("signal_exports.logs", """{"enabled":false}"""))), 2);
        Assert.Equal(RemoteConfigStatuses.Failed, f.Status.Status);
        Assert.Equal("signal_exports.logs: this SDK cannot apply the requested override", f.Status.ErrorMessage);
        Assert.Null(f.Apply);
        Assert.Equal("""{"head_sampling":{"ratio":0.5}}""", Policy.Overrides(f.Effective));

        f.Decide(Config(Policy.Of(("instrumentations", """{"disabled":["httpclient"]}"""))), 3);
        Assert.Equal(RemoteConfigStatuses.Failed, f.Status.Status);
        Assert.Equal("instrumentations: disabled: this SDK cannot apply the requested override", f.Status.ErrorMessage);
        Assert.Null(f.Apply);

        // The same controls local need nothing of the host.
        var every = Policy.OfEvery(("head_sampling", """{"ratio":0.25}"""));
        f.Decide(Config(every), 4);
        Assert.Equal(RemoteConfigStatuses.Applied, f.Status.Status);
        Assert.Equal(Policy.Full(every), Policy.Text(f.Effective));

        // A host naming what is no setting has a bug: an error, saying which.
        var e = Assert.ThrowsAny<Exception>(() => new Flow(new Host { Live = { "send_metric" } }));
        Assert.Contains("send_metric", e.Message);
    }

    [Fact]
    public void ALogLevelArrivesAsTheLowestSeverityNumberToExport()
    {
        var f = new Flow(Declaring());
        Assert.False(f.Apply!.HasSendLogsMinSeverity); // local: every severity
        f.Decide(Config(Policy.Of(("log_levels", """{"level":"warn"}"""))));
        Assert.Equal(RemoteConfigStatuses.Applied, f.Status.Status);
        Assert.True(f.Apply!.HasSendLogsMinSeverity);
        Assert.Equal(13u, f.Apply.SendLogsMinSeverity);
        Assert.Equal("""{"log_levels":{"level":"warn"}}""", Policy.Overrides(f.Effective));
    }

    // A host that declares every setting: each live one at once, each next-start one at the next start.
    private static Host Declaring() => new()
    {
        Live =
        {
            "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
            "sampling_rate", "sampling_rules", "sampling_traces_per_second", "scrub_defaults", "scrub_patterns",
            "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics",
            "send_self_metrics", "send_traces",
        },
        NextStart = { "deactivate_all_instrumentations", "deactivate_instrumentations" },
    };

    private const ulong Now = 1_791_979_200_000; // 2026-10-14T12:00:00Z

    [Fact]
    public void TheSettingsThatMatchTelemetryArriveTypedWhereTheHostDeclaresThem()
    {
        var doc = Policy.Of(
            ("sampling_rules", """{"rules":[{"rate":0,"kind":"server","path":"/health*"}]}"""),
            ("log_levels", """{"scopes":{"com.acme.db*":"debug"}}"""),
            ("attribute_redaction", """{"drop":["*token*"],"mask":["User.Email"]}"""),
            ("trace_rate_limit", """{"traces_per_second":50}"""),
            ("log_rate_limit", """{"records_per_second":0.5}"""));
        var d = new Flow(Declaring()).Decide(Config(doc));
        Assert.Equal(RemoteConfigStatuses.Applied, d.Status.Status);
        var s = d.Apply!;
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
        var plain = new Flow(new Host { Live = { "sampling_rate" } }).Decide(Config(doc));
        Assert.Equal(RemoteConfigStatuses.Failed, plain.Status.Status);
        Assert.Equal("sampling_rules: rules: this SDK cannot apply the requested override", plain.Status.ErrorMessage);
        Assert.Null(plain.Apply);
        Assert.Equal("{}", Policy.Overrides(plain.Effective));
    }

    [Fact]
    public void DebugModeArrivesFoldedInAndTheTimerAtItsEndEndsIt()
    {
        var doc = Policy.Of(("head_sampling", """{"ratio":0.1}"""), ("debug", """{"until":"2026-10-14T12:30:00Z","log_level":"info"}"""));
        var f = new Flow(Declaring()).Decide(Config(doc), 0);
        Assert.Equal(RemoteConfigStatuses.Applied, f.Status.Status);
        Assert.Equal(1.0, f.Apply!.SamplingRate);
        Assert.Equal(9u, f.Apply.SendLogsMinSeverity);
        const ulong end = 30 * 60_000;
        Assert.Equal(Now + end, f.State.RefreshAtUnixMs);
        Assert.InRange(f.Out.WakeAtMonoMs, 1ul, Now + end);
        f.Step(end, new AgentStep { Timer = new ExportCounts() });
        Assert.Equal(0.1, f.Apply!.SamplingRate);
        Assert.Equal(0ul, f.State.RefreshAtUnixMs);
        Assert.Equal("""{"head_sampling":{"ratio":0.1}}""", Policy.Overrides(f.Effective));
    }

    [Fact]
    public void ExportHealthIsSentWhenBuiltAndOnAFailureAndHeldWhileNothingChanges()
    {
        var f = new Flow(Declaring());
        f.Step(0, new AgentStep { Built = new AgentBuilt { Service = "checkout" } });
        f.Step(0, new AgentStep { Exporters = new AgentExporters { Counts = new ExportCounts() } });
        var first = f.Health!.ComponentHealthMap["exporters"];
        Assert.Equal(3, first.ComponentHealthMap.Count);
        Assert.Equal("ok", first.Status);
        Assert.Null(f.Step(30_000, new AgentStep { Exporters = new AgentExporters { Counts = new ExportCounts() } }).Health);
        f.Step(60_000, new AgentStep
        {
            Exporters = new AgentExporters
            {
                Counts = new ExportCounts
                {
                    Traces = new SignalCounts { ExportsFailed = 1, ItemsFailed = 512, LastFailed = true, LastError = "connection refused" },
                },
                Exporters = { new Exporter { Signal = "traces", Name = "zipkin" } },
            },
        });
        var failed = f.Health!.ComponentHealthMap["exporters"];
        Assert.False(failed.Healthy);
        Assert.Equal("connection refused", failed.ComponentHealthMap["traces"].LastError);
        var zipkin = failed.ComponentHealthMap["traces"].ComponentHealthMap["zipkin"];
        Assert.Equal("ungoverned", zipkin.Status);
        Assert.True(zipkin.Healthy);
    }

    // Every case of testdata/exporters.json through the wrapper: what is decided, compared whole.
    [Fact]
    public void TheExportersVectors()
    {
        using var layer = new SdkSettings();
        foreach (var c in Cases("exporters.json"))
        {
            string Text(JsonElement e, string k) => e.TryGetProperty(k, out var v) ? v.GetString()! : string.Empty;
            bool Flag(JsonElement e, string k) => e.TryGetProperty(k, out var v) && v.GetBoolean();
            var found = c.GetProperty("found").EnumerateArray().Select(f => new FoundExporter
            {
                Signal = Text(f, "signal"), Kind = Text(f, "kind"), Gated = Flag(f, "gated"), Endpoint = Text(f, "endpoint"),
                FromEnv = Flag(f, "from_env"),
            }).ToList();
            var env = c.TryGetProperty("env", out var e) ? e.EnumerateObject().Select(p => KeyValuePair.Create(p.Name, p.Value.GetString()!)).ToList() : [];
            var got = layer.Exporters(found, env, Text(c, "default_protocol"));
            var actual = JsonSerializer.Serialize(new
            {
                exporters = got.Exporters.Select(x => new { signal = x.Signal, name = x.Name, gated = x.Gated, destination = x.Destination }),
                governed = got.Governed, ungoverned = got.Ungoverned, warnings = got.Warnings,
            });
            var want = JsonSerializer.Serialize(c.GetProperty("want"));
            Assert.True(JsonNode.DeepEquals(JsonNode.Parse(want), JsonNode.Parse(actual)), $"{c.GetProperty("name")}: {actual}");
        }
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

            if (want.TryGetProperty("destinations", out var destinations))
            {
                // Where each exporter sends, for those that say: signal to identity to destination.
                var expected = destinations.EnumerateObject().ToDictionary(
                    s => s.Name, s => s.Value.EnumerateObject().ToDictionary(e => e.Name, e => e.Value.GetString()!));
                var actual = got.Components["exporters"].SubComponentMap
                    .Select(s => (s.Key, Sent: s.Value.SubComponentMap
                        .Where(e => e.Value.Metadata.Any(m => m.Key == "destination"))
                        .ToDictionary(e => e.Key, e => e.Value.Metadata.Single(m => m.Key == "destination").Value.StringValue)))
                    .Where(s => s.Sent.Count > 0)
                    .ToDictionary(s => s.Key, s => s.Sent);
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
            Integration = "Logfire.Fleet.AutoInstrumentation",
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
    // policy of the last config applied in the core's saved state, else the SDK's own.
    [Fact]
    public void TheStartVectors()
    {
        foreach (var c in Cases("start.json"))
        {
            var name = c.GetProperty("name").GetString();
            var coreState = c.TryGetProperty("core_state", out var h) ? Convert.FromHexString(h.GetString()!) : null;
            var f = new Flow(DeclaringAll(), coreState: coreState);
            var want = c.GetProperty("want").GetString();
            Assert.True(want == f.State.Current.ToStringUtf8(), $"{name}: started from {f.State.Current.ToStringUtf8()}");
            Assert.True(c.GetProperty("warns").GetBoolean() == f.Warned, $"{name}: warned {f.Warned}");
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
                Destination = e.TryGetProperty("destination", out var d) ? d.GetString() : string.Empty,
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
