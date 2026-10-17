// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Google.Protobuf;
using OpAmp.Proto.V1;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

public class AgentTests
{
    private static AgentRemoteConfig Config(string policy) => Policy.Config(policy);

    private static readonly string DisableHttpClient = Policy.Of(("instrumentations", """{"disabled":["httpclient"]}"""));

    private static string TempDir()
    {
        var dir = Path.Combine(Path.GetTempPath(), "opamp-plugin-" + Guid.NewGuid());
        Directory.CreateDirectory(dir);
        return dir;
    }

    private sealed class Process
    {
        public Dictionary<string, string?> Env { get; } = new()
        {
            ["OTEL_OPAMP_ENDPOINT"] = "http://127.0.0.1:1/v1/opamp",
        };

        public Dictionary<string, string> Set { get; } = new();

        public Agent? Prepare() => Agent.Prepare(n => this.Env.GetValueOrDefault(n), (n, v) => this.Set[n] = v);
    }

    [Fact]
    public void WithoutAnEndpointThePluginIsOff()
    {
        var p = new Process();
        p.Env.Clear();
        Assert.Null(p.Prepare());
        Assert.Empty(p.Set);
    }

    [Fact]
    public void ABadHeartbeatIntervalIsTheDefault()
    {
        var p = new Process { Env = { ["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = "soon" } };
        var warned = new List<string>();
        Assert.Equal(TimeSpan.Zero, Server.FromEnv(n => p.Env.GetValueOrDefault(n), warned.Add)!.Heartbeat);
        Assert.Equal(["OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable, using the default: not a duration: \"soon\""], warned);
        Assert.NotNull(p.Prepare()); // managed, at the client's default interval
    }

    [Fact]
    public void LiveSettingsAtOnceSwitchesAtTheNextStart()
    {
        var p = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = TempDir() } };
        var agent = p.Prepare()!;
        var policy = Policy.Of(
            ("head_sampling", """{"ratio":0.25}"""),
            ("signal_exports.logs", """{"enabled":false}"""),
            ("instrumentations", """{"disabled":["httpclient"]}"""));
        var status = agent.RemoteConfig(Config(policy));
        Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
        Assert.False(agent.Controls.SendLogs);
        Assert.Empty(p.Set); // the instrumentations were read already: nothing changes until the restart
        // The client keeps the config applied, in its own state: nothing of the plugin's own.
        Assert.False(File.Exists(Path.Combine(p.Env["OTEL_OPAMP_STATE_DIR"]!, "sdk-settings.json")));
        Kept.KeepApplied(p.Env["OTEL_OPAMP_STATE_DIR"]!, policy);

        agent.Stop(TimeSpan.Zero); // the process exits
        var next = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = p.Env["OTEL_OPAMP_STATE_DIR"] } };
        var restarted = next.Prepare()!;
        Assert.Equal(agent.Uid, restarted.Uid);
        Assert.False(restarted.Controls.SendLogs);
        Assert.Equal("false", next.Set["OTEL_DOTNET_AUTO_TRACES_HTTPCLIENT_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", next.Set["OTEL_DOTNET_AUTO_METRICS_HTTPCLIENT_INSTRUMENTATION_ENABLED"]);
        Assert.Equal(3, next.Set.Count);
    }

    [Fact]
    public void TheSettingsToStartWithAreTheConfigTheClientKeptAppliedElseTheFileKeptBefore()
    {
        var dir = TempDir();
        double? Rate()
        {
            var agent = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!;
            try
            {
                return agent.Controls.Sampler.Policy.Rate;
            }
            finally
            {
                agent.Stop(TimeSpan.Zero);
            }
        }

        // Kept before the client kept the config applied (an earlier release): it carries over.
        File.WriteAllText(Path.Combine(dir, "sdk-settings.json"), Policy.HeadSampling(0.25));
        Assert.Equal(0.25, Rate());
        // Once the client keeps a config applied, that is where a start begins: the old file is not read.
        Kept.KeepApplied(dir, Policy.HeadSampling(0.5));
        Assert.Equal(0.5, Rate());
        // A config applied with every control local: every setting the SDK's own, not the old file's.
        Kept.KeepApplied(dir, Policy.Local);
        Assert.Null(Rate());
        // A client state that doesn't read: the old file again.
        File.WriteAllBytes(Path.Combine(dir, "opamp-state"), [0xff, 0xff]);
        Assert.Equal(0.25, Rate());
    }

    [Fact]
    public void AnUnusableConfigChangesNothing()
    {
        var p = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = TempDir() } };
        var agent = p.Prepare()!;
        agent.RemoteConfig(Config(Policy.Stop("traces")));
        var status = agent.RemoteConfig(Config(Policy.Of(("head_sampling", """{"ratio":7}"""), ("signal_exports.traces", """{"enabled":true}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, status.Status);
        Assert.StartsWith("head_sampling:", status.ErrorMessage);
        Assert.False(agent.Controls.SendTraces);
    }

    [Fact]
    public void WithoutAStateDirectoryANextStartChangeIsRefused()
    {
        var agent = new Process().Prepare()!;
        var refused = agent.RemoteConfig(Config(Policy.Of(("instrumentations", """{"all_disabled":true}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("instrumentations: takes effect at the next start, which needs somewhere to keep state", refused.ErrorMessage);
        Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(Policy.Stop("metrics"))).Status);
        Assert.False(agent.Controls.SendMetrics);
    }

    [Fact]
    public void SwitchingEveryInstrumentationOffSetsEachOnesVariable()
    {
        var vars = Env.Switches(new Client.Sdk.Settings { DeactivateAllInstrumentations = true });
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_LOGS_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_TRACES_ASPNETCORE_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_METRICS_NETRUNTIME_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_LOGS_ILOGGER_INSTRUMENTATION_ENABLED"]);
        Assert.Equal(1 + 3 + Env.Instrumentations.Values.Sum(n => n.Length), vars.Count);
        Assert.Empty(Env.Switches(new Client.Sdk.Settings()));
    }

    [Fact]
    public void AnInstrumentationNameIsUpstreamsWhateverItsPunctuation()
    {
        var names = new Client.Sdk.Names { Names_ = { "asp-net-core", "http_client", "httpclient", "entity.framework.core", "ilogger", "-" } };
        var vars = Env.Switches(new Client.Sdk.Settings { DeactivateInstrumentations = names });
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_TRACES_ASPNETCORE_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_METRICS_HTTPCLIENT_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_TRACES_ENTITYFRAMEWORKCORE_INSTRUMENTATION_ENABLED"]);
        Assert.Equal("false", vars["OTEL_DOTNET_AUTO_LOGS_ILOGGER_INSTRUMENTATION_ENABLED"]);
        Assert.Equal(4 * 3, vars.Count);

        // Each of upstream's, lowercase as the docs give them, is its own variable.
        foreach (var (signal, upstream) in Env.Instrumentations)
        {
            foreach (var name in upstream)
            {
                var one = Env.Switches(new Client.Sdk.Settings { DeactivateInstrumentations = new Client.Sdk.Names { Names_ = { name.ToLowerInvariant() } } });
                Assert.Equal("false", one[$"OTEL_DOTNET_AUTO_{signal}_{name}_INSTRUMENTATION_ENABLED"]);
            }
        }
    }

    [Fact]
    public void ASecondProcessOnTheSameStateDirectoryRunsUnmanaged()
    {
        var dir = TempDir();
        var parent = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } };
        var agent = parent.Prepare()!;
        Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(DisableHttpClient)).Status);

        // A child the application starts inherits the variables: it doesn't take the parent's UID or state.
        var child = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } };
        Assert.Null(child.Prepare());
        Assert.Empty(child.Set);

        Kept.KeepApplied(dir, DisableHttpClient); // as the client does, before APPLIED goes out
        agent.Stop(TimeSpan.Zero);
        var next = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } };
        Assert.Equal(agent.Uid, next.Prepare()!.Uid);
        Assert.Equal(3, next.Set.Count);
    }

    [Fact]
    public void AUidTheServerAssignsIsTheNextStartsUid()
    {
        var dir = TempDir();
        var agent = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!;
        var assigned = Uuid.NewV7();
        agent.Notified(null!, new OpAmpNotification.InstanceUidChanged(assigned));
        Assert.NotEqual(Uuid.Format(assigned), agent.Uid); // this run's resource is built already
        agent.Stop(TimeSpan.Zero);
        Assert.Equal(Uuid.Format(assigned), new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!.Uid);
    }

    /// <summary>
    /// The first notification (a remote config in the answer to the first message) is answered
    /// through the client it comes with: callbacks wait for Start to return, so nothing holds the
    /// answers until the agent has stored the client. The config applied is where the next start
    /// begins: the client keeps it with its state, and the server hears APPLIED once that is saved.
    /// </summary>
    [Fact]
    public async Task TheFirstNotificationIsAnsweredThroughItsClient()
    {
        var body = Policy.Stop("logs");
        var cancel = TestContext.Current.CancellationToken;
        var port = FreePort();
        using var listener = new System.Net.HttpListener();
        listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        listener.Start();
        var applied = new TaskCompletionSource<RemoteConfigStatus>(TaskCreationOptions.RunContinuationsAsynchronously);
        async Task Serve()
        {
            var messages = 0;
            while (listener.IsListening)
            {
                System.Net.HttpListenerContext ctx;
                try
                {
                    ctx = await listener.GetContextAsync();
                }
                catch (Exception)
                {
                    return;
                }

                using var buf = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(buf, cancel);
                var m = AgentToServer.Parser.ParseFrom(buf.ToArray());
                if (m.RemoteConfigStatus is { Status: RemoteConfigStatuses.Applied } s && s.LastRemoteConfigHash.ToStringUtf8() == body)
                {
                    applied.TrySetResult(s);
                }

                var reply = new ServerToAgent { InstanceUid = m.InstanceUid };
                if (++messages == 1)
                {
                    reply.RemoteConfig = Config(body);
                }

                var bytes = reply.ToByteArray();
                ctx.Response.ContentType = "application/x-protobuf";
                ctx.Response.ContentLength64 = bytes.Length;
                await ctx.Response.OutputStream.WriteAsync(bytes, cancel);
                ctx.Response.Close();
            }
        }

        var serving = Task.Run(Serve, cancel);
        var dir = TempDir();
        var agent = new Process
        {
            Env =
            {
                ["OTEL_OPAMP_ENDPOINT"] = $"http://127.0.0.1:{port}/v1/opamp",
                ["OTEL_OPAMP_STATE_DIR"] = dir,
                ["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = "100ms",
            },
        }.Prepare()!;
        try
        {
            agent.Start([]);
            var done = await Task.WhenAny(applied.Task, Task.Delay(TimeSpan.FromSeconds(20), cancel));
            Assert.True(done == applied.Task, "the remote config offered first was never answered APPLIED");
            Assert.False(agent.Controls.SendLogs);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(10));
            listener.Close();
            await serving;
        }

        Assert.False(File.Exists(Path.Combine(dir, "sdk-settings.json")));
        var next = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!;
        try
        {
            Assert.False(next.Controls.SendLogs); // in force from the start
        }
        finally
        {
            next.Stop(TimeSpan.Zero);
        }
    }

    private static int FreePort()
    {
        var l = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        l.Start();
        var port = ((System.Net.IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    [Fact]
    public void StoppingNeverThrows()
    {
        Agent.Await(Task.FromException(new InvalidOperationException("the core failed")), TimeSpan.FromSeconds(1));
        Agent.Await(new TaskCompletionSource().Task, TimeSpan.Zero); // never ends: given up on
    }

    [Fact]
    public void AnOverrideOfASignalThatCantBeStoppedIsRefused()
    {
        var p = new Process
        {
            Env =
            {
                ["OTEL_TRACES_EXPORTER"] = "console",
            },
        };
        var agent = p.Prepare()!;
        Assert.Equal(new[] { "send_traces" }, agent.HostNow(providersBuilt: false).Undeclared());
        var status = agent.RemoteConfig(Config(Policy.Stop("traces", "logs")));
        Assert.Equal(RemoteConfigStatuses.Failed, status.Status);
        Assert.Equal("signal_exports.traces: this SDK cannot apply the requested override", status.ErrorMessage);
        Assert.True(agent.Controls.SendTraces); // nothing could stop it
        Assert.True(agent.Controls.SendLogs); // a refused policy changes nothing
        Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(Policy.Stop("logs"))).Status);
        Assert.False(agent.Controls.SendLogs);
    }

    // OTLP beside another exporter: only OTLP could be stopped, so the setting is not carried out
    // at all, rather than half (and the OTLP one is not gated either). A policy that overrides it is refused.
    [Fact]
    public void ASignalWithAnotherExporterBesideOtlpIsNotCarriedOut()
    {
        var p = new Process
        {
            Env =
            {
                ["OTEL_OPAMP_STATE_DIR"] = TempDir(),
                ["OTEL_METRICS_EXPORTER"] = "otlp,console",
                ["OTEL_LOGS_EXPORTER"] = "otlp",
            },
        };
        var agent = p.Prepare()!;
        Assert.Equal(new[] { "send_metrics" }, agent.HostNow(providersBuilt: false).Undeclared());
        Assert.DoesNotContain("send_metrics", agent.HostNow(providersBuilt: true).Live);
        var status = agent.RemoteConfig(Config(Policy.Stop("metrics", "logs")));
        Assert.Equal(RemoteConfigStatuses.Failed, status.Status);
        Assert.Equal("signal_exports.metrics: this SDK cannot apply the requested override", status.ErrorMessage);
        Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(Policy.Stop("logs"))).Status);
        Assert.True(agent.Controls.SendMetrics);
        Assert.False(agent.Controls.SendLogs);
        // The server is told what is in force.
        Assert.Equal("""{"signal_exports.logs":{"enabled":false}}""", Policy.Overrides(agent.Effective));

        var options = new OpenTelemetry.Exporter.OtlpExporterOptions();
        var own = options.HttpClientFactory;
        agent.Gate(options, Signal.Metrics);
        Assert.Same(own, options.HttpClientFactory);
    }

    // The SDK's exporters build TLS from these variables into their own HTTP clients; a gate's
    // carries the same. Ones it can't use leave every exporter as it is: no signal can be
    // stopped. A signal's own TLS variables the SDK doesn't read: they stop nothing.
    [Fact]
    public void TlsTheGateCantCarryLeavesEverySignalAsItIs()
    {
        var pki = new TlsTests.Pki();
        var with = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = TempDir() } };
        foreach (var (k, v) in pki.Env)
        {
            with.Env[k] = v!;
        }

        Assert.DoesNotContain(with.Prepare()!.HostNow(providersBuilt: false).Undeclared(), u => u.StartsWith("send_", StringComparison.Ordinal));
        var own = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = TempDir(), ["OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE"] = "/etc/tls/client.pem" } };
        Assert.DoesNotContain(own.Prepare()!.HostNow(providersBuilt: false).Undeclared(), u => u.StartsWith("send_", StringComparison.Ordinal));
        var unusable = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = TempDir(), ["OTEL_EXPORTER_OTLP_CERTIFICATE"] = "/nonexistent/ca.pem" } };
        var agent = unusable.Prepare()!;
        Assert.Equal(new[] { "send_traces", "send_metrics", "send_logs", "send_logs_level" }, agent.HostNow(providersBuilt: false).Undeclared().Where(u => u.StartsWith("send_", StringComparison.Ordinal)));
        var refused = agent.RemoteConfig(Config(Policy.Stop("traces")));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("signal_exports.traces: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.True(agent.Controls.SendTraces); // nothing could stop it
    }

    [Fact]
    public void UnderFileBasedConfigurationOnlySamplingIsCarriedOut()
    {
        var p = new Process { Env = { ["OTEL_EXPERIMENTAL_FILE_BASED_CONFIGURATION_ENABLED"] = "true", ["OTEL_OPAMP_STATE_DIR"] = TempDir() } };
        var first = p.Prepare()!;
        var refused = first.RemoteConfig(Config(DisableHttpClient));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("instrumentations: disabled: this SDK cannot apply the requested override", refused.ErrorMessage);
        first.Stop(TimeSpan.Zero);
        // A policy kept from a start that could carry it out switches nothing off here.
        Kept.KeepApplied(p.Env["OTEL_OPAMP_STATE_DIR"]!, DisableHttpClient);
        var next = new Process { Env = { ["OTEL_EXPERIMENTAL_FILE_BASED_CONFIGURATION_ENABLED"] = "true", ["OTEL_OPAMP_STATE_DIR"] = p.Env["OTEL_OPAMP_STATE_DIR"] } };
        var agent = next.Prepare()!;
        Assert.Empty(next.Set);
        Assert.Equal(
            new[] { "deactivate_instrumentations", "deactivate_all_instrumentations", "send_traces", "send_metrics", "send_logs", "send_logs_level" },
            agent.HostNow(providersBuilt: false).Undeclared());
    }

    // The logs floor is held at the OTLP logs exporter's gate: with no OTLP logs exporter, nothing holds it.
    [Fact]
    public void WithoutAnOtlpLogsExporterSendLogsLevelIsNotCarriedOut()
    {
        var p = new Process { Env = { ["OTEL_LOGS_EXPORTER"] = "console" } };
        var agent = p.Prepare()!;
        Assert.Equal(new[] { "send_logs", "send_logs_level" }, agent.HostNow(providersBuilt: false).Undeclared());
        var errors = Policy.Of(("log_levels", """{"level":"error"}"""));
        var refused = agent.RemoteConfig(Config(errors));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("log_levels: level: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.Equal(0u, agent.Controls.LogsFloor); // nothing could hold it

        var otlp = new Process().Prepare()!;
        Assert.DoesNotContain("send_logs_level", otlp.HostNow(providersBuilt: false).Undeclared());
        Assert.Equal(RemoteConfigStatuses.Applied, otlp.RemoteConfig(Config(errors)).Status);
        Assert.Equal(17u, otlp.Controls.LogsFloor);
        otlp.RemoteConfig(Config(Policy.Local));
        Assert.Equal(0u, otlp.Controls.LogsFloor);
    }

    [Fact]
    public void TheSamplerWrapsTheConfiguredOneAndIsKnownWhenTheProvidersAreBuilt()
    {
        var p = new Process { Env = { ["OTEL_TRACES_SAMPLER"] = "always_off" } };
        var agent = p.Prepare()!;
        Assert.Contains("sampling_rate", agent.HostNow(providersBuilt: true).Undeclared()); // no tracer provider yet
        using var provider = OpenTelemetry.Sdk.CreateTracerProviderBuilder().AddSource("t").Let(agent.WrapSampler).Build();
        Assert.DoesNotContain("sampling_rate", agent.HostNow(providersBuilt: true).Undeclared());
        using var source = new System.Diagnostics.ActivitySource("t");
        // Dropped, a span still carries the context on, unrecorded.
        using (var dropped = source.StartActivity("s"))
        {
            Assert.False(dropped!.Recorded); // the configured always_off
        }

        agent.Controls.Sampler.Rate(1);
        using var kept = source.StartActivity("s");
        Assert.True(kept!.Recorded);
    }

    [Fact]
    public void TheSamplerTheBuilderWasGivenIsReadFromIt()
    {
        // No OTEL_TRACES_SAMPLER: the SDK's default would record. The builder's own is read instead.
        var agent = new Process().Prepare()!;
        using var provider = OpenTelemetry.Sdk.CreateTracerProviderBuilder().AddSource("r").SetSampler(new AlwaysOffSampler())
            .Let(agent.WrapSampler).Build();
        Assert.DoesNotContain("sampling_rate", agent.HostNow(providersBuilt: true).Undeclared());
        using var source = new System.Diagnostics.ActivitySource("r");
        using (var dropped = source.StartActivity("s"))
        {
            Assert.False(dropped!.Recorded);
        }

        agent.Controls.Sampler.Rate(1);
        using var kept = source.StartActivity("s");
        Assert.True(kept!.Recorded);
    }

    [Fact]
    public void ASavedUidIsLowercase()
    {
        var dir = TempDir();
        File.WriteAllText(Path.Combine(dir, "instance-uid"), "0190A3B2-7C4D-7E8F-9A0B-1C2D3E4F5A6B\n");
        var agent = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!;
        Assert.Equal("0190a3b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b", agent.Uid);
    }

    [Fact]
    public void TheInstanceUidIsKeptOrTakenFromTheResource()
    {
        const string given = "0190a3b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b";
        var p = new Process { Env = { ["OTEL_RESOURCE_ATTRIBUTES"] = $"service.name=x, service.instance.id={given}" } };
        Assert.Equal(given, p.Prepare()!.Uid);
        p.Env["OTEL_RESOURCE_ATTRIBUTES"] = "service.instance.id=pod-7";
        var made = p.Prepare()!.Uid;
        Assert.Equal('7', made[14]);
        Assert.Equal(Uuid.Format(Uuid.Bytes(made)), made);
    }

    /// <summary>
    /// Issue #218: a state directory the UID can't be saved in keeps none, so the resource's UUID
    /// names the instance at every start, as without one; not a new UID each start. (A directory
    /// that can't be locked leaves the SDK unmanaged before this: here the directory is the
    /// process's, and only the UID's file can't be written.)
    /// </summary>
    [Fact]
    public void AStateDirectoryItCantSaveInKeepsAGivenUuid()
    {
        const string given = "0190a3b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b";
        var dir = TempDir();
        Directory.CreateDirectory(Path.Combine(dir, "instance-uid.tmp"));
        for (var i = 0; i < 2; i++)
        {
            var p = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir, ["OTEL_RESOURCE_ATTRIBUTES"] = $"service.instance.id={given}" } };
            var agent = p.Prepare()!;
            Assert.Equal(given, agent.Uid);
            agent.Stop(TimeSpan.Zero);
        }

        var bare = new Process { Env = { ["OTEL_OPAMP_STATE_DIR"] = dir } }.Prepare()!;
        Assert.Equal('7', bare.Uid[14]);
        bare.Stop(TimeSpan.Zero);
    }

    /// <summary>A process with the user's state directory under <paramref name="home"/>, and no state directory named.</summary>
    private static Process At(string home, string service = "checkout") => new()
    {
        Env = { ["OTEL_SERVICE_NAME"] = service, ["XDG_STATE_HOME"] = home, ["HOME"] = home, ["LOCALAPPDATA"] = home },
    };

    [Fact]
    public void ByDefaultAnInstanceKeepsItsUidAcrossRestarts()
    {
        var home = TempDir();
        var first = At(home).Prepare()!;
        Assert.StartsWith("checkout-", Path.GetFileName(Assert.Single(Directory.GetDirectories(Path.Combine(StateBase(home), "opamp")))));
        // Another process of it, running at once: a slot of its own, so an identity of its own.
        var other = At(home).Prepare()!;
        Assert.NotEqual(first.Uid, other.Uid);
        first.Stop(TimeSpan.Zero);
        other.Stop(TimeSpan.Zero);
        var again = At(home).Prepare()!;
        Assert.Equal(first.Uid, again.Uid);
        again.Stop(TimeSpan.Zero);
        // Another service is another instance.
        var elsewhere = At(home, "billing").Prepare()!;
        Assert.NotEqual(first.Uid, elsewhere.Uid);
        elsewhere.Stop(TimeSpan.Zero);
    }

    /// <summary>
    /// A service.instance.id that isn't a UUID can't be the instance UID, so it is replaced by one:
    /// kept in the default state directory, the same at every start, rather than a new one each time.
    /// </summary>
    [Fact]
    public void AServiceInstanceIdThatIsntAUuidKeepsTheDefaultStateDirectory()
    {
        var home = TempDir();
        var uids = new List<string>();
        for (var i = 0; i < 2; i++)
        {
            var p = At(home, "web");
            p.Env["OTEL_RESOURCE_ATTRIBUTES"] = "service.instance.id=web-1";
            var agent = p.Prepare()!;
            uids.Add(agent.Uid);
            agent.Stop(TimeSpan.Zero); // as the process exiting would
        }

        Assert.Single(Directory.GetDirectories(Path.Combine(StateBase(home), "opamp")));
        Assert.Equal(uids[0], uids[1]);
    }

    [Fact]
    public void TheDefaultStateDirectoryGivesWay()
    {
        var home = TempDir();
        string? Claimed(Process p, out FileStream? held)
        {
            var server = Server.FromEnv(n => p.Env.GetValueOrDefault(n), _ => { })!;
            Assert.True(StateDirs.Claim(ref server, n => p.Env.GetValueOrDefault(n), Log.Sink, out held));
            return server.StateDir;
        }

        // Turned off, or a service.instance.id the application chose (a UUID, so the instance UID): none.
        var off = At(home);
        off.Env["OTEL_OPAMP_STATE_DIR"] = "none";
        Assert.Null(Claimed(off, out var none));
        Assert.Null(none);
        var chosen = At(home);
        chosen.Env["OTEL_RESOURCE_ATTRIBUTES"] = $"service.instance.id={Guid.NewGuid()}";
        Assert.Null(Claimed(chosen, out _));
        // Named: that one.
        var named = At(home);
        named.Env["OTEL_OPAMP_STATE_DIR"] = Path.Combine(home, "mine");
        Assert.Equal(Path.Combine(home, "mine"), Claimed(named, out var mine));
        mine!.Dispose();
        var padded = new Server(new Uri("http://127.0.0.1:1/v1/opamp"), new Dictionary<string, string>(), " " + Path.Combine(home, "mine") + " ", TimeSpan.Zero);
        Assert.True(StateDirs.Claim(ref padded, _ => null, Log.Sink, out var trimmed));
        Assert.Equal(Path.Combine(home, "mine"), padded.StateDir); // the directory locked is the one used
        trimmed!.Dispose();
        // Nowhere to keep it: none, and no error.
        Assert.Null(Claimed(new Process(), out _));
        var file = Path.Combine(home, "file");
        File.WriteAllText(file, string.Empty);
        Assert.Null(Claimed(At(file), out _));
        // Every slot taken: none.
        var held = new List<FileStream>();
        for (var i = 0; i < StateDirs.Slots; i++)
        {
            Assert.NotNull(Claimed(At(home), out var l));
            held.Add(l!);
        }

        Assert.Null(Claimed(At(home), out _));
        held.ForEach(l => l.Dispose());
        // Without a service name, the program's.
        var unnamed = At(home);
        unnamed.Env.Remove("OTEL_SERVICE_NAME");
        Assert.StartsWith(Path.Combine(StateBase(home), "opamp", StateDirs.ProgramName() + "-"), Claimed(unnamed, out var p));
        p!.Dispose();
    }

    [Fact]
    public void TheDefaultStateDirectoryIsTheUsersOnly()
    {
        if (OperatingSystem.IsWindows())
        {
            return;
        }

        var home = TempDir();
        var claimed = StateDirs.Default(n => At(home).Env.GetValueOrDefault(n), Log.Sink, "app", "/srv/app")!.Value;
        Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute, File.GetUnixFileMode(claimed.Dir));
        Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(Path.Combine(claimed.Dir, "lock")));
        // Two working directories of one program: two identities.
        var elsewhere = StateDirs.Default(n => At(home).Env.GetValueOrDefault(n), Log.Sink, "app", "/srv/other")!.Value;
        Assert.Equal("1", Path.GetFileName(elsewhere.Dir));
        Assert.NotEqual(Path.GetDirectoryName(claimed.Dir), Path.GetDirectoryName(elsewhere.Dir));
        claimed.Lock.Dispose();
        elsewhere.Lock.Dispose();
    }

    private static string StateBase(string home) =>
        OperatingSystem.IsMacOS() ? Path.Combine(home, "Library", "Application Support") : home;
}

internal static class BuilderExtensions
{
    public static TracerProviderBuilder Let(this TracerProviderBuilder b, Action<TracerProviderBuilder> f)
    {
        f(b);
        return b;
    }
}
