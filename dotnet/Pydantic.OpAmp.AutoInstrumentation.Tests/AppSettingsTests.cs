// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Text.Json;
using System.Text.Json.Nodes;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// The application's own settings (docs/app-settings.md): what <see cref="OpAmpAppSettings"/> gives
/// the application to read, on its own and through the plugin's agent and the layer, with the
/// shared vectors (testdata/sdk-settings/custom.json). Each test has a store of its own;
/// those of <see cref="OpAmpAppSettings"/>'s own are in <see cref="GlobalAppSettingsTests"/>.
/// </summary>
public class AppSettingsTests
{
    /// <summary>What a handler is told: each call's map and keys, in order.</summary>
    internal sealed class Told
    {
        private readonly List<(IReadOnlyDictionary<string, string> Settings, string[] Changed)> calls = [];

        public void Handler(IReadOnlyDictionary<string, string> settings, IReadOnlySet<string> changed)
        {
            lock (this.calls)
            {
                this.calls.Add((settings, changed.ToArray()));
            }
        }

        public List<(IReadOnlyDictionary<string, string> Settings, string[] Changed)> All
        {
            get
            {
                lock (this.calls)
                {
                    return [.. this.calls];
                }
            }
        }

        /// <summary>Told exactly these changes, in order: each the map then in force and the keys, sorted.</summary>
        public void Want(params (Dictionary<string, string> Settings, string[] Changed)[] want)
        {
            var got = this.All;
            Assert.True(got.Count == want.Length, $"told {got.Count} times: {string.Join("; ", got.Select(Show))}, want {want.Length}");
            for (var i = 0; i < want.Length; i++)
            {
                Assert.Equal(Canon(want[i].Settings), Canon(got[i].Settings));
                Assert.Equal(want[i].Changed, got[i].Changed);
            }
        }

        private static string Show((IReadOnlyDictionary<string, string> Settings, string[] Changed) c) =>
            $"{Canon(c.Settings)} [{string.Join(", ", c.Changed)}]";
    }

    /// <summary>A map as text, its entries in key order: two maps are equal when their texts are.</summary>
    internal static string Canon(IEnumerable<KeyValuePair<string, string>> map) =>
        JsonSerializer.Serialize(map.OrderBy(kv => kv.Key, StringComparer.Ordinal).Select(kv => new[] { kv.Key, kv.Value }));

    internal static Dictionary<string, string> M(params (string Key, string Value)[] entries) =>
        entries.ToDictionary(e => e.Key, e => e.Value);

    private static string TempDir()
    {
        var dir = Path.Combine(Path.GetTempPath(), "opamp-app-settings-" + Guid.NewGuid());
        Directory.CreateDirectory(dir);
        return dir;
    }

    /// <summary>The plugin's agent on <paramref name="app"/>, its state in <paramref name="dir"/> (null: none).</summary>
    private static Agent Prepare(AppSettingsStore app, string? dir, Action<string, string>? setenv = null) =>
        Agent.Prepare(
            n => n switch
            {
                "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp",
                "OTEL_OPAMP_STATE_DIR" => dir ?? "none",
                _ => null,
            },
            setenv ?? ((_, _) => { }),
            app: app)!;

    /// <summary>The overrides of the policy the agent reports in force (<see cref="Policy.Overrides(string)"/>).</summary>
    private static string Effective(Agent agent) => Policy.Overrides(agent.Effective);

    /// <summary>A policy whose only override is <c>app_settings</c>, with these values (a JSON object).</summary>
    private static string Values(string values) => Policy.Of(("app_settings", $"{{\"values\":{values}}}"));

    private static JsonElement CustomJson()
    {
        var dir = AppContext.BaseDirectory;
        while (!Directory.Exists(Path.Combine(dir, "testdata", "sdk-settings")))
        {
            dir = Path.GetDirectoryName(dir) ?? throw new DirectoryNotFoundException("testdata/sdk-settings");
        }

        using var doc = JsonDocument.Parse(File.ReadAllText(Path.Combine(dir, "testdata", "sdk-settings", "custom.json")));
        return doc.RootElement.Clone();
    }

    // --- reading ---------------------------------------------------------------------------------

    [Fact]
    public void BeforeAnyConfigEveryReadIsItsDefault()
    {
        var app = new AppSettingsStore();
        Assert.Equal("off", app.Get("checkout.new_flow", "off"));
        Assert.Null(app.Get("checkout.new_flow", null));
        Assert.True(app.GetBool("flag", true));
        Assert.False(app.GetBool("flag", false));
        Assert.Equal(7, app.GetInt64("limit", 7));
        Assert.Equal(0.5, app.GetDouble("ratio", 0.5));
        Assert.Empty(app.Snapshot());
    }

    [Fact]
    public void AValueInForceIsReadAsSentAndAKeyLeftOutIsItsDefault()
    {
        var app = new AppSettingsStore();
        app.Put(M(("greeting", "  Hello "), ("empty", "")));
        Assert.Equal("  Hello ", app.Get("greeting", "x"));
        Assert.Equal(string.Empty, app.Get("empty", "x")); // an empty value is a value
        Assert.Equal("x", app.Get("Greeting", "x")); // keys are case-sensitive
        Assert.Equal("x", app.Get("missing", "x"));
        Assert.Equal("x", app.Get(string.Empty, "x"));
        Assert.Equal("x", app.Get(null, "x")); // never an error, whatever the key
        Assert.False(app.GetBool(null, false));
    }

    // The typed reads, as the shared vectors say: each value, read as a bool, an Int64 and a
    // double, gives what the vector says, or the caller's default (null in the vector).
    [Fact]
    public void TheTypedReadsTakeWhatTheSharedVectorsSay()
    {
        var typed = CustomJson().GetProperty("typed").EnumerateArray().ToList();
        Assert.True(typed.Count >= 30, $"{typed.Count} typed vectors: the file changed shape?");
        var failed = new List<string>();
        foreach (var v in typed)
        {
            var value = v.GetProperty("value").GetString()!;
            var app = new AppSettingsStore();
            app.Put(M(("k", value)));
            // Two defaults each, so a read that happens to equal one default still shows.
            foreach (var def in new[] { true, false })
            {
                var want = v.GetProperty("bool") is { ValueKind: JsonValueKind.True or JsonValueKind.False } b ? b.GetBoolean() : def;
                if (app.GetBool("k", def) != want)
                {
                    failed.Add($"GetBool({JsonSerializer.Serialize(value)}, {def}): {app.GetBool("k", def)}, want {want}");
                }
            }

            foreach (var def in new[] { -424242L, long.MaxValue - 1 })
            {
                var want = v.GetProperty("int") is { ValueKind: JsonValueKind.Number } i ? i.GetInt64() : def;
                if (app.GetInt64("k", def) != want)
                {
                    failed.Add($"GetInt64({JsonSerializer.Serialize(value)}, {def}): {app.GetInt64("k", def)}, want {want}");
                }
            }

            foreach (var def in new[] { -4.25, Math.PI })
            {
                var want = v.GetProperty("float") is { ValueKind: JsonValueKind.Number } f ? f.GetDouble() : def;
                if (!app.GetDouble("k", def).Equals(want))
                {
                    failed.Add($"GetDouble({JsonSerializer.Serialize(value)}, {def}): {app.GetDouble("k", def)}, want {want}");
                }
            }
        }

        Assert.Empty(failed);
    }

    // Beyond the vectors: Unicode that folds to ASCII is no boolean, only the four spaces are
    // trimmed, and a double too small to keep is zero, as JSON reads it.
    [Fact]
    public void TheTypedReadsAtTheirEdges()
    {
        var app = new AppSettingsStore();
        app.Put(M(("long-s", "fal\u017Fe"), ("vt", "\vtrue"), ("nbsp", "\u00A0true"), ("tiny", "1e-400"),
            ("neg-zero", "-0"), ("max", "1.7976931348623157e308"), ("min", "-9223372036854775809"), ("crlf", "\r\n12\r\n")));
        Assert.True(app.GetBool("long-s", true));
        Assert.False(app.GetBool("vt", false));
        Assert.False(app.GetBool("nbsp", false));
        Assert.Equal(0.0, app.GetDouble("tiny", 1));
        Assert.True(double.IsNegative(app.GetDouble("neg-zero", 1)) && app.GetDouble("neg-zero", 1) == 0);
        Assert.Equal(double.MaxValue, app.GetDouble("max", 1));
        Assert.Equal(3, app.GetInt64("min", 3)); // below the range
        Assert.Equal(12, app.GetInt64("crlf", 3));
        Assert.Equal(3, app.GetInt64("missing", 3));
        Assert.Equal(3.0, app.GetDouble("missing", 3));
        Assert.True(app.GetBool("missing", true));
    }

    [Fact]
    public void TheSnapshotIsAReadOnlyMapNoLaterChangeAlters()
    {
        var app = new AppSettingsStore();
        app.Put(M(("b", "2"), ("a", "1")));
        var before = app.Snapshot();
        app.Put(M(("a", "9")));
        Assert.Equal(["a", "b"], before.Keys); // in key order
        Assert.Equal("1", before["a"]);
        Assert.Equal("9", app.Snapshot()["a"]);
        Assert.False(app.Snapshot().ContainsKey("b"));
        Assert.Throws<NotSupportedException>(() => ((IDictionary<string, string>)before)["a"] = "x");
    }

    [Fact]
    public void PutKeepsACopyOfWhatItIsGiven()
    {
        var app = new AppSettingsStore();
        var given = M(("a", "1"));
        app.Put(given);
        given["a"] = "2";
        given["b"] = "3";
        Assert.Equal("1", app.Get("a", null));
        Assert.Single(app.Snapshot());
    }

    // --- being told ------------------------------------------------------------------------------

    [Fact]
    public void OnChangeIsToldOncePerChangeWithTheMapAndTheKeysThatChanged()
    {
        var app = new AppSettingsStore();
        var told = new Told();
        using var sub = app.OnChange(told.Handler);
        app.Put(M(("a", "1"), ("b", "2")));
        app.Put(M(("b", "2"), ("a", "1"))); // the same again: nothing changed
        app.Put(M(("a", "1"), ("b", "3"), ("c", ""))); // b changed, c added
        app.Put(M(("a", "1"))); // b and c left out: back to their defaults
        app.Put(null); // app_settings local
        app.Put(M()); // and again: nothing
        told.Want(
            (M(("a", "1"), ("b", "2")), new[] { "a", "b" }),
            (M(("a", "1"), ("b", "3"), ("c", "")), new[] { "b", "c" }),
            (M(("a", "1")), new[] { "b", "c" }),
            (M(), new[] { "a" }));
    }

    [Fact]
    public void TheChangedKeysAreASortedReadOnlySet()
    {
        var app = new AppSettingsStore();
        IReadOnlySet<string>? keys = null;
        using var sub = app.OnChange((_, changed) => keys = changed);
        app.Put(M(("b", "1"), ("B", "1"), ("a.z", "1"), ("a", "1")));
        Assert.NotNull(keys);
        Assert.Equal(["B", "a", "a.z", "b"], keys); // ordinal: upper case first
        Assert.True(keys.Contains("a.z") && !keys.Contains("A"));
        Assert.True(keys.SetEquals(["a", "b", "B", "a.z"]));
        Assert.Equal("B, a, a.z, b", keys.ToString());
    }

    [Fact]
    public void NotToldAtSubscriptionOfTheValuesAlreadyInForce()
    {
        var app = new AppSettingsStore();
        app.Put(M(("a", "1")));
        var told = new Told();
        using var sub = app.OnChange(told.Handler);
        app.Put(M(("a", "1")));
        told.Want();
    }

    [Fact]
    public void AHandlerSeesTheNewValuesThroughGetToo()
    {
        var app = new AppSettingsStore();
        string? seen = null;
        using var sub = app.OnChange((_, _) => seen = app.Get("a", "default"));
        app.Put(M(("a", "1")));
        Assert.Equal("1", seen);
    }

    [Fact]
    public void HandlersAreToldInTheOrderTheyWereAddedAndOneAddedTwiceTwice()
    {
        var app = new AppSettingsStore();
        var order = new List<string>();
        void First(IReadOnlyDictionary<string, string> settings, IReadOnlySet<string> changed) => order.Add("first");
        using var a = app.OnChange(First);
        using var b = app.OnChange((_, _) => order.Add("second"));
        using var c = app.OnChange(First);
        app.Put(M(("a", "1")));
        Assert.Equal(["first", "second", "first"], order);
    }

    [Fact]
    public void AHandlerRemovedIsToldNothingMoreAndRemovingItAgainDoesNothing()
    {
        var app = new AppSettingsStore();
        var told = new Told();
        var subscription = app.OnChange(told.Handler);
        app.Put(M(("a", "1")));
        subscription.Dispose();
        subscription.Dispose();
        app.Put(M(("a", "2")));
        told.Want((M(("a", "1")), new[] { "a" }));
    }

    [Fact]
    public void RemovingOneOfAHandlerAddedTwiceLeavesTheOther()
    {
        var app = new AppSettingsStore();
        var told = new Told();
        var first = app.OnChange(told.Handler);
        using var second = app.OnChange(told.Handler);
        first.Dispose();
        app.Put(M(("a", "1")));
        told.Want((M(("a", "1")), new[] { "a" }));
    }

    [Fact]
    public void ANullHandlerIsRefused()
    {
        var app = new AppSettingsStore();
        Assert.Throws<ArgumentNullException>(() => app.OnChange(null!));
        Assert.Throws<ArgumentNullException>(() => OpAmpAppSettings.OnChange(null!));
    }

    [Fact]
    public void AHandlerMayRemoveItselfOrAddAnotherWhileItIsTold()
    {
        var app = new AppSettingsStore();
        var told = new Told();
        var later = new Told();
        var subscriptions = new List<IDisposable>();
        IDisposable? once = null;
        once = app.OnChange((_, _) =>
        {
            once!.Dispose();
            subscriptions.Add(app.OnChange(told.Handler));
        });
        using var l = app.OnChange(later.Handler); // added before, removed by nobody: told of both
        app.Put(M(("a", "1"))); // once runs and goes; told is added, and hears from the next change on
        app.Put(M(("a", "2")));
        told.Want((M(("a", "2")), new[] { "a" }));
        later.Want((M(("a", "1")), new[] { "a" }), (M(("a", "2")), new[] { "a" }));
        Assert.Single(subscriptions);
    }

    // Who is told of a change is fixed as it begins: a handler added once its values can be read
    // (here, from the line that says so, between the values going in force and the handlers being
    // called) hears from the next change on, never of one already in force when it subscribed.
    [Fact]
    public void AHandlerAddedOnceAChangeIsInForceIsToldFromTheNextChange()
    {
        var app = new AppSettingsStore();
        var late = new Told();
        var subscriptions = new List<IDisposable>();
        app.Info = _ =>
        {
            if (subscriptions.Count == 0)
            {
                Assert.Equal("1", app.Get("a", null)); // in force already
                subscriptions.Add(app.OnChange(late.Handler));
            }
        };
        app.Put(M(("a", "1")));
        late.Want();
        app.Put(M(("a", "2")));
        late.Want((M(("a", "2")), new[] { "a" }));
        subscriptions.ForEach(s => s.Dispose());
    }

    [Fact]
    public void AHandlerThatThrowsIsLoggedAndTheRestAreStillTold()
    {
        var app = new AppSettingsStore();
        var errors = new List<string>();
        var infos = new List<string>();
        app.Error = errors.Add;
        app.Info = infos.Add;
        var told = new Told();
        using var a = app.OnChange((_, _) => throw new InvalidOperationException("nope"));
        using var b = app.OnChange(told.Handler);
        app.Put(M(("a", "1")));
        Assert.Equal("1", app.Get("a", null));
        told.Want((M(("a", "1")), new[] { "a" }));
        var error = Assert.Single(errors);
        Assert.Contains("an application settings handler threw (changed: a)", error);
        Assert.Contains("nope", error);
        Assert.Equal(["application settings changed: a"], infos);
    }

    [Fact]
    public void ControlsKeepTheirOwnApplicationSettingsUnlessGivenSome()
    {
        var own = new Controls();
        own.Apply(new Settings { Custom = { ["a"] = "1" } });
        Assert.Equal("1", own.App.Get("a", null));
        Assert.NotSame(OpAmpAppSettings.Store, own.App);
        var store = new AppSettingsStore();
        Assert.Same(store, new Controls(app: store).App);
    }

    // --- through the agent and the layer ---------------------------------------------------------

    // Each shared case through the layer (the .NET wrapper's Decide), as the plugin declares the
    // settings or as the case's own host does, then through Controls: the map the application reads
    // is the one the case says, told once, and a refused case is FAILED with its reason. The cases
    // with no host of their own go through the plugin's agent too.
    [Fact]
    public void TheSharedCustomVectorsReachTheApplicationAsTheLayerDecides()
    {
        var cases = CustomJson().GetProperty("cases").EnumerateArray().ToList();
        Assert.True(cases.Count >= 25, $"{cases.Count} cases: the file changed shape?");
        var failed = new List<string>();
        using var layer = new SdkSettings();
        foreach (var c in cases)
        {
            var name = c.GetProperty("name").GetString()!;
            var document = c.GetProperty("document").GetString()!;
            var config = Vectors.Config(document);
            Host host;
            if (c.TryGetProperty("host", out var h))
            {
                host = new Host();
                host.Live.AddRange(h.TryGetProperty("live", out var live) ? live.EnumerateArray().Select(e => e.GetString()!) : Enumerable.Empty<string>());
                host.NextStart.AddRange(h.TryGetProperty("next_start", out var un) ? un.EnumerateArray().Select(e => e.GetString()!) : Enumerable.Empty<string>());
            }
            else
            {
                host = Vectors.Declaring();
                host.Live.Add("custom");
            }

            var d = layer.Decide(config, ByteString.Empty, ByteString.Empty, host, Vectors.Now);
            if (c.TryGetProperty("refused", out var refused))
            {
                if (d.Status.Status != RemoteConfigStatuses.Failed || d.Status.ErrorMessage != refused.GetString())
                {
                    failed.Add($"{name}: {d.Status.Status} {d.Status.ErrorMessage}, want FAILED {refused.GetString()}");
                }

                if (d.Apply is not null)
                {
                    failed.Add($"{name}: a refused config hands settings over");
                }
            }
            else
            {
                var want = c.GetProperty("custom").EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString()!);
                if (d.Status.Status != RemoteConfigStatuses.Applied)
                {
                    failed.Add($"{name}: {d.Status.Status} {d.Status.ErrorMessage}");
                    continue;
                }

                var controls = new Controls();
                var told = new Told();
                using (controls.App.OnChange(told.Handler))
                {
                    controls.Apply(d.Apply);
                }

                if (Canon(want) != Canon(controls.App.Snapshot()))
                {
                    failed.Add($"{name}: in force {JsonSerializer.Serialize(controls.App.Snapshot())}");
                }

                if (told.All.Count != (want.Count == 0 ? 0 : 1))
                {
                    failed.Add($"{name}: told {told.All.Count} times");
                }

                var body = Policy.Text(d.Effective);
                if (body != c.GetProperty("effective").GetString())
                {
                    failed.Add($"{name}: effective {body}");
                }
            }

            // The plugin's agent too, where its host carries out the policy: a case refused, or one
            // that overrides nothing but app_settings.
            if (h.ValueKind == JsonValueKind.Undefined && (c.TryGetProperty("refused", out _) || OnlyAppSettings(document)))
            {
                // The plugin's agent, as the client hands the config over.
                var app = new AppSettingsStore();
                var agent = Prepare(app, null);
                try
                {
                    var status = agent.RemoteConfig(config);
                    var wantStatus = c.TryGetProperty("refused", out _) ? RemoteConfigStatuses.Failed : RemoteConfigStatuses.Applied;
                    if (status.Status != wantStatus)
                    {
                        failed.Add($"{name}: the agent answered {status.Status} {status.ErrorMessage}");
                    }

                    var want = c.TryGetProperty("custom", out var custom)
                        ? custom.EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString()!)
                        : new Dictionary<string, string>();
                    if (Canon(want) != Canon(app.Snapshot()))
                    {
                        failed.Add($"{name}: the agent put {JsonSerializer.Serialize(app.Snapshot())} in force");
                    }

                    if (status.Status == RemoteConfigStatuses.Failed && status.ErrorMessage != c.GetProperty("refused").GetString())
                    {
                        failed.Add($"{name}: the agent refused it: {status.ErrorMessage}");
                    }

                    if (c.TryGetProperty("effective", out var effective) && Policy.Text(agent.Effective) != effective.GetString())
                    {
                        failed.Add($"{name}: the agent reports {Policy.Text(agent.Effective)}");
                    }
                }
                finally
                {
                    agent.Stop(TimeSpan.Zero);
                }
            }
        }

        Assert.Empty(failed);
    }

    private static bool OnlyAppSettings(string document)
    {
        try
        {
            return JsonNode.Parse(Policy.Overrides(document))!.AsObject().All(kv => kv.Key == "app_settings");
        }
        catch (Exception e) when (e is JsonException or ArgumentException)
        {
            return false; // a document that repeats a key
        }
    }

    [Fact]
    public void ThePluginDeclaresCustom()
    {
        using var probe = new ScratchAgent();
        Assert.Contains("custom", probe.Agent.HostNow(providersBuilt: false).Live);
        Assert.Contains("custom", probe.Agent.HostNow(providersBuilt: true).Live);
    }

    [Fact]
    public void AConfigPutsTheValuesInForceAfterTheSdksAndTheEffectiveConfigReportsThem()
    {
        var app = new AppSettingsStore();
        var agent = Prepare(app, TempDir());
        try
        {
            var told = new Told();
            double? rate = null;
            using var sub = app.OnChange((s, c) =>
            {
                rate = agent.Controls.Sampler.Policy.Rate; // last: the SDK's settings beside them already in force
                told.Handler(s, c);
            });
            var status = agent.RemoteConfig(Vectors.Config(Policy.Of(
                ("head_sampling", """{"ratio":0.5}"""),
                ("app_settings", """{"values":{"checkout.new_flow":"on","limit":25}}"""))));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            Assert.Equal(0.5, rate);
            Assert.Equal("on", app.Get("checkout.new_flow", "off"));
            Assert.Equal(25, app.GetInt64("limit", 0));
            Assert.Equal("""{"app_settings":{"values":{"checkout.new_flow":"on","limit":"25"}},"head_sampling":{"ratio":0.5}}""", Effective(agent));
            told.Want((M(("checkout.new_flow", "on"), ("limit", "25")), new[] { "checkout.new_flow", "limit" }));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void TheSameValuesSentAgainOrAnotherWayTellNothing()
    {
        var app = new AppSettingsStore();
        var agent = Prepare(app, TempDir());
        try
        {
            var told = new Told();
            using var sub = app.OnChange(told.Handler);
            agent.RemoteConfig(Vectors.Config(Values("""{"a":"1","b":"2"}""")));
            // New configs (other hashes) whose map is the same: keys in another order, a number for
            // its text, another control beside it.
            foreach (var body in new[]
            {
                Values("""{"b":"2","a":1}"""),
                Policy.Of(("head_sampling", """{"ratio":1}"""), ("app_settings", """{"values":{"a":"1","b":"2"}}""")),
            })
            {
                Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Vectors.Config(body)).Status);
            }

            told.Want((M(("a", "1"), ("b", "2")), new[] { "a", "b" }));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void AWrongCustomEntryFailsTheConfigAndNothingChanges()
    {
        var dir = TempDir();
        var app = new AppSettingsStore();
        var agent = Prepare(app, dir);
        try
        {
            var told = new Told();
            using var sub = app.OnChange(told.Handler);
            agent.RemoteConfig(Vectors.Config(Policy.Of(("head_sampling", """{"ratio":0.5}"""), ("app_settings", """{"values":{"a":"1"}}"""))));
            foreach (var (body, why) in new[]
            {
                (Policy.Of(("head_sampling", """{"ratio":1}"""), ("app_settings", """{"values":{"a":"2","b":["x"]}}""")), "app_settings: values must be an object"),
                (Values("""{"a b":"2"}"""), "app_settings: values: \"a b\" is not a key"),
                (Values("""{"a":"2","a":"3"}"""), "app_settings: values.a: sent more than once"),
                (Values("{\"a\":\"" + new string('x', 4097) + "\"}"), "app_settings: values.a: 4097 bytes, at most 4096"),
                (Values("7"), "app_settings: values must be an object"),
            })
            {
                var status = agent.RemoteConfig(Vectors.Config(body));
                Assert.Equal(RemoteConfigStatuses.Failed, status.Status);
                Assert.StartsWith(why, status.ErrorMessage, StringComparison.Ordinal);
            }

            Assert.Equal("1", app.Get("a", null));
            Assert.Equal(0.5, agent.Controls.Sampler.Policy.Rate);
            Assert.Equal("""{"app_settings":{"values":{"a":"1"}},"head_sampling":{"ratio":0.5}}""", Effective(agent));
            told.Want((M(("a", "1")), new[] { "a" }));
            Assert.False(File.Exists(Path.Combine(dir, "sdk-settings.json"))); // the client keeps the config applied
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void APolicyWithAppSettingsLocalPutsEveryKeyBackToItsDefault()
    {
        var app = new AppSettingsStore();
        var agent = Prepare(app, TempDir());
        try
        {
            var told = new Told();
            using var sub = app.OnChange(told.Handler);
            agent.RemoteConfig(Vectors.Config(Values("""{"a":"1"}""")));
            // A short policy leaves app_settings out: it is local.
            agent.RemoteConfig(Vectors.Config(Policy.HeadSampling(1)));
            Assert.Equal("default", app.Get("a", "default"));
            agent.RemoteConfig(Vectors.Config(Values("""{"a":"1"}""")));
            // A remote config with no policy at all is refused, and changes nothing.
            var none = agent.RemoteConfig(new AgentRemoteConfig { ConfigHash = ByteString.CopyFromUtf8("none"), Config = new AgentConfigMap() });
            Assert.Equal(RemoteConfigStatuses.Failed, none.Status);
            Assert.StartsWith("config_map:", none.ErrorMessage, StringComparison.Ordinal);
            Assert.Equal("1", app.Get("a", "default"));
            // A policy sending every control, each local.
            agent.RemoteConfig(Vectors.Config(Policy.OfEvery()));
            Assert.Equal("default", app.Get("a", "default"));
            told.Want((M(("a", "1")), new[] { "a" }), (M(), new[] { "a" }), (M(("a", "1")), new[] { "a" }), (M(), new[] { "a" }));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void AHandlerThatThrowsDoesNotBreakTheAnswer()
    {
        var app = new AppSettingsStore();
        var agent = Prepare(app, TempDir());
        try
        {
            var errors = new List<string>();
            app.Error = errors.Add; // after Prepare, which points it at the plugin's standard error
            using var sub = app.OnChange((_, _) => throw new InvalidOperationException("nope"));
            var status = agent.RemoteConfig(Vectors.Config(Values("""{"a":"1"}""")));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            Assert.Equal("1", app.Get("a", null));
            Assert.Equal("""{"app_settings":{"values":{"a":"1"}}}""", Effective(agent));
            Assert.Single(errors);
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // The layer's Refresh (debug mode ends, on the timer) hands the settings over again: the same map
    // is put in force again, which is no change, and the application still reads it.
    [Fact]
    public void ARefreshWithTheSameMapTellsNothing()
    {
        var app = new AppSettingsStore();
        var wall = Vectors.Now;
        var agent = Agent.Prepare(
            n => n switch
            {
                "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp",
                "OTEL_OPAMP_STATE_DIR" => "none",
                _ => null,
            },
            (_, _) => { },
            new Clock(() => wall, () => 0, (_, _) => new Dummy()),
            app)!;
        try
        {
            var told = new Told();
            using var sub = app.OnChange(told.Handler);
            var status = agent.RemoteConfig(Vectors.Config(Policy.Of(
                ("head_sampling", """{"ratio":0.1}"""),
                ("debug", """{"until":"2026-10-14T12:10:00Z"}"""),
                ("app_settings", """{"values":{"a":"1"}}"""))));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            agent.Sdk.Refresh(Vectors.Now + 300_000); // while debug mode lasts
            agent.Sdk.Refresh(Vectors.Now + 600_000); // and at its end
            Assert.Equal(0.1, agent.Controls.Sampler.Policy.Rate);
            Assert.Equal(0UL, agent.Sdk.RefreshAt);
            told.Want((M(("a", "1")), new[] { "a" }));
            Assert.Equal("1", app.Get("a", null));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void ValuesSurviveARestartWithAStateDirectory()
    {
        var dir = TempDir();
        var first = Prepare(new AppSettingsStore(), dir);
        var policy = Values("""{"checkout.new_flow":"on"}""");
        first.RemoteConfig(Vectors.Config(policy));
        Kept.KeepApplied(dir, policy); // as the client does, before APPLIED goes out
        first.Stop(TimeSpan.Zero); // the process exits

        // The next start, before any config arrives: in force as the plugin prepares, before the
        // application's code runs; a handler added before that is told of them, once.
        var app = new AppSettingsStore();
        var told = new Told();
        using var sub = app.OnChange(told.Handler);
        var again = Prepare(app, dir);
        try
        {
            Assert.Equal("on", app.Get("checkout.new_flow", "off"));
            Assert.Equal("""{"app_settings":{"values":{"checkout.new_flow":"on"}}}""", Effective(again));
            told.Want((M(("checkout.new_flow", "on")), new[] { "checkout.new_flow" }));
        }
        finally
        {
            again.Stop(TimeSpan.Zero);
        }
    }

    [Fact]
    public void WithoutAStateDirectoryARestartStartsFromTheDefaults()
    {
        var app = new AppSettingsStore();
        var agent = Prepare(app, null);
        Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Vectors.Config(Values("""{"a":"1"}"""))).Status); // live: no state needed
        Assert.Equal("1", app.Get("a", null));
        agent.Stop(TimeSpan.Zero);

        var next = new AppSettingsStore();
        var again = Prepare(next, null);
        Assert.Equal("default", next.Get("a", "default"));
        again.Stop(TimeSpan.Zero);
    }

    // A plugin that fails as it prepares, after it has put the saved values in force, takes them
    // back: the application reads what it read before, a handler told of the saved values is told
    // they are gone, and the state directory is free again.
    [Fact]
    public void AFailedPrepareTakesTheSavedValuesBack()
    {
        var dir = TempDir();
        var first = Prepare(new AppSettingsStore(), dir);
        var policy = Policy.Of(("app_settings", """{"values":{"example.feature":"on"}}"""), ("instrumentations", """{"disabled":["httpclient"]}"""));
        first.RemoteConfig(Vectors.Config(policy));
        Kept.KeepApplied(dir, policy);
        first.Stop(TimeSpan.Zero);

        var app = new AppSettingsStore();
        app.Put(M(("before", "1")));
        var told = new Told();
        using var sub = app.OnChange(told.Handler);
        // The saved values are loaded, then switching the instrumentation off fails.
        var e = Assert.Throws<InvalidOperationException>(() => Prepare(app, dir, (_, _) => throw new InvalidOperationException("no")));
        Assert.Equal("no", e.Message);
        Assert.Equal(Canon(M(("before", "1"))), Canon(app.Snapshot()));
        told.Want(
            (M(("example.feature", "on")), new[] { "before", "example.feature" }),
            (M(("before", "1")), new[] { "before", "example.feature" }));

        var again = Prepare(new AppSettingsStore(), dir); // the lock was let go
        Assert.NotNull(again);
        again.Stop(TimeSpan.Zero);
    }

    /// <summary>A plugin agent on a store of its own and no state, to ask what it declares.</summary>
    private sealed class ScratchAgent : IDisposable
    {
        public Agent Agent { get; } = Prepare(new AppSettingsStore(), null);

        public void Dispose() => this.Agent.Stop(TimeSpan.Zero);
    }

    private sealed class Dummy : IDisposable
    {
        public void Dispose()
        {
        }
    }
}

/// <summary>
/// The application settings across threads: each test keeps every core busy a while, so they run
/// alone, not beside the tests that time a real Kestrel application's spans.
/// </summary>
[Collection(nameof(GlobalAppSettings))]
public sealed class AppSettingsThreadTests
{
    [Fact]
    public void ChangesAreToldOneAtATimeInOrderWhicheverThreadPutsThem()
    {
        var app = new AppSettingsStore();
        var inside = 0;
        var overlapped = 0;
        var seen = new List<(IReadOnlyDictionary<string, string> Settings, string[] Changed)>();
        using var sub = app.OnChange((settings, changed) =>
        {
            if (Interlocked.Increment(ref inside) != 1)
            {
                Interlocked.Increment(ref overlapped);
            }

            seen.Add((settings, changed.ToArray())); // one at a time: no lock needed
            Thread.SpinWait(50);
            Interlocked.Decrement(ref inside);
        });
        Parallel.For(0, 8, g =>
        {
            for (var i = 0; i < 200; i++)
            {
                app.Put(AppSettingsTests.M(("n", ((g * 200) + i).ToString(System.Globalization.CultureInfo.InvariantCulture)), ("g", (i % 2).ToString(System.Globalization.CultureInfo.InvariantCulture))));
            }
        });
        Assert.Equal(0, overlapped);
        // Each change told is relative to the one told before it: the order they happened in.
        IReadOnlyDictionary<string, string> previous = new Dictionary<string, string>();
        foreach (var (settings, changed) in seen)
        {
            var want = settings.Keys.Union(previous.Keys)
                .Where(k => !previous.TryGetValue(k, out var v) || !settings.TryGetValue(k, out var w) || v != w)
                .Order(StringComparer.Ordinal).ToArray();
            Assert.Equal(want, changed);
            previous = settings;
        }

        Assert.Equal(AppSettingsTests.Canon(previous), AppSettingsTests.Canon(app.Snapshot()));
    }

    [Fact]
    public async Task ReadersOnOtherThreadsAlwaysSeeOneWholeMap()
    {
        var app = new AppSettingsStore();
        using var stop = new CancellationTokenSource();
        var torn = 0;
        var reads = 0;
        var readers = Enumerable.Range(0, 4).Select(_ => Task.Run(() =>
        {
            while (!stop.IsCancellationRequested)
            {
                var s = app.Snapshot();
                if (s.Count != 0 && (s.Count != 2 || s["a"] != s["b"]))
                {
                    Interlocked.Increment(ref torn);
                }

                Interlocked.Increment(ref reads);
            }
        })).ToArray();
        // Writes go on until the readers have run alongside them a good while (a reader's task may
        // start only once a few thousand writes are done), at most 20 s.
        var deadline = DateTime.UtcNow + TimeSpan.FromSeconds(20);
        var writes = 0;
        for (var started = -1; writes < 5000 || Volatile.Read(ref reads) - started < 20_000; writes++)
        {
            if (started < 0 && Volatile.Read(ref reads) > 0)
            {
                started = Volatile.Read(ref reads);
            }

            var v = (writes % 3).ToString(System.Globalization.CultureInfo.InvariantCulture);
            app.Put(AppSettingsTests.M(("a", v), ("b", v)));
            if (DateTime.UtcNow > deadline)
            {
                break;
            }
        }

        stop.Cancel();
        await Task.WhenAll(readers);
        Assert.Equal(0, torn);
        Assert.True(reads >= 20_000, $"{reads} reads alongside {writes} writes");
    }

    [Fact]
    public async Task HandlersAddedAndRemovedFromOtherThreadsWhileChangesAreTold()
    {
        var app = new AppSettingsStore();
        var errors = new ConcurrentQueue<string>();
        app.Error = errors.Enqueue;
        var steady = new AppSettingsTests.Told();
        using var s = app.OnChange(steady.Handler);
        using var stop = new CancellationTokenSource();
        var churn = Enumerable.Range(0, 4).Select(_ => Task.Run(() =>
        {
            while (!stop.IsCancellationRequested)
            {
                using (app.OnChange((_, _) => { }))
                {
                }
            }
        })).ToArray();
        for (var i = 0; i < 2000; i++)
        {
            app.Put(AppSettingsTests.M(("a", (i % 2).ToString(System.Globalization.CultureInfo.InvariantCulture))));
        }

        stop.Cancel();
        await Task.WhenAll(churn);
        Assert.Empty(errors);
        Assert.Equal(2000, steady.All.Count); // the one that stayed heard every change
    }

    // Apply puts a document's SDK settings and its custom values in force together: a handler told
    // of the custom values sees that document's SDK settings, whatever another Apply is doing.
    [Fact]
    public void ApplyPutsEachDocumentInForceWhole()
    {
        var controls = new Controls();
        var off = new Settings { SendTraces = false, Custom = { ["traces"] = "off" } };
        var on = new Settings { SendTraces = true, Custom = { ["traces"] = "on" } };
        var mixed = 0;
        using var sub = controls.App.OnChange((now, _) =>
        {
            if ((now["traces"] == "on") != controls.SendTraces)
            {
                Interlocked.Increment(ref mixed);
            }
        });
        Parallel.For(0, 4, g =>
        {
            for (var i = 0; i < 500; i++)
            {
                controls.Apply((g + i) % 2 == 0 ? off : on);
            }
        });
        Assert.Equal(0, mixed);
    }
}

/// <summary>Tests of <see cref="OpAmpAppSettings"/>'s own store, which the plugin fills: alone, no other test running.</summary>
[CollectionDefinition(nameof(GlobalAppSettings), DisableParallelization = true)]
public sealed class GlobalAppSettings
{
}

[Collection(nameof(GlobalAppSettings))]
public sealed class GlobalAppSettingsTests : IDisposable
{
    public void Dispose() => OpAmpAppSettings.Store.Put(null);

    [Fact]
    public void WithoutAnEndpointEveryReadIsItsDefaultAndNothingIsTold()
    {
        OpAmpAppSettings.Store.Put(null);
        var told = new AppSettingsTests.Told();
        using var sub = OpAmpAppSettings.OnChange(told.Handler);
        Assert.Null(Agent.Prepare(_ => null, (_, _) => { }));
        Assert.Equal("off", OpAmpAppSettings.Get("example.feature", "off"));
        Assert.Null(OpAmpAppSettings.Get("example.feature"));
        Assert.True(OpAmpAppSettings.GetBool("flag", true));
        Assert.Equal(7, OpAmpAppSettings.GetInt64("limit", 7));
        Assert.Equal(0.5, OpAmpAppSettings.GetDouble("ratio", 0.5));
        Assert.Empty(OpAmpAppSettings.Snapshot());
        told.Want();
    }

    // The plugin, as automatic instrumentation runs it: what OpAmpAppSettings reads is what the
    // server sent, and a handler the application added is told.
    [Fact]
    public void ThePluginFillsWhatTheApplicationReads()
    {
        OpAmpAppSettings.Store.Put(null);
        var agent = Agent.Prepare(
            n => n switch
            {
                "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp",
                "OTEL_OPAMP_STATE_DIR" => "none",
                _ => null,
            },
            (_, _) => { })!;
        try
        {
            Assert.Same(OpAmpAppSettings.Store, agent.Controls.App);
            var told = new AppSettingsTests.Told();
            using (OpAmpAppSettings.OnChange(told.Handler))
            {
                agent.RemoteConfig(Vectors.Config(Policy.Of(("app_settings", """{"values":{"example.feature":"on","limit":"9","ratio":"0.25","beta":"TRUE"}}"""))));
                Assert.Equal("on", OpAmpAppSettings.Get("example.feature", "off"));
                Assert.Equal(9, OpAmpAppSettings.GetInt64("limit", 0));
                Assert.Equal(0.25, OpAmpAppSettings.GetDouble("ratio", 0));
                Assert.True(OpAmpAppSettings.GetBool("beta", false));
                Assert.Equal(["beta", "example.feature", "limit", "ratio"], OpAmpAppSettings.Snapshot().Keys);
                agent.RemoteConfig(Vectors.Config(Policy.Of(("app_settings", """{"values":{"example.feature":"off"}}"""))));
            }

            agent.RemoteConfig(Vectors.Config(Policy.Local));
            Assert.Equal(2, told.All.Count); // unsubscribed before the third
            Assert.Equal(["beta", "example.feature", "limit", "ratio"], told.All[1].Changed);
            Assert.Equal("default", OpAmpAppSettings.Get("example.feature", "default"));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }
}
