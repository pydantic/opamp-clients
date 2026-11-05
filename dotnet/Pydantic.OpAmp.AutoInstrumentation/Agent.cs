// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Exporters = Pydantic.OpAmp.Otel.Exporters;

namespace Pydantic.OpAmp.AutoInstrumentation;

/// <summary>
/// The plugin's state: the SDK under the server. One per process. What every SDK set up in .NET
/// does under the server (the state directory, the settings layer, the client) is
/// <see cref="SdkAgent"/>'s, shared with the hosting package; what is automatic instrumentation's
/// (its variables, file-based configuration, the builder it hands plugins) is here.
/// </summary>
internal sealed class Agent
{
    private readonly Func<string, string?> getenv;
    private readonly bool fileConfig;
    private readonly HashSet<Signal> gated = [];
    private readonly OtlpTls? tls;
    private readonly SdkAgent agent;
    // Held weakly: a logger provider holds its watch, and one the application let go of goes with it.
    private readonly List<WeakReference<LogsWatch>> logs = [];
    private bool samplerWrapped;

    // Whether the sampler the live one wraps is the one OTEL_TRACES_SAMPLER names (not the configuration's own).
    private bool envSampler;
    private TracerProvider? tracing;
    private MeterProvider? metrics;

    private Agent(Server server, Func<string, string?> getenv, FileStream? stateLock, Clock clock, AppSettingsStore app)
    {
        this.getenv = getenv;
        // File-based configuration is read before any plugin runs, and its exporters' TLS settings
        // live in the SDK's internals: switches and gates would be silently lost or break TLS.
        this.fileConfig = string.Equals(getenv("OTEL_EXPERIMENTAL_FILE_BASED_CONFIGURATION_ENABLED")?.Trim(), "true",
            StringComparison.OrdinalIgnoreCase);
        // An OTLP exporter given a client certificate or a CA builds them into its own HTTP client,
        // which a gate (ours) replaces: the gate's carries them. Files it can't use leave every
        // exporter as it is.
        var tlsUsable = OtlpTls.TryFromEnv(getenv, Log.Warn, out this.tls);
        foreach (var signal in Enum.GetValues<Signal>())
        {
            if (tlsUsable && this.Gateable(signal))
            {
                this.gated.Add(signal);
            }
        }

        this.agent = new SdkAgent(server, getenv, stateLock, this.HostNow, Log.Sink, new Controls(clock.Nanos, app), clock);
    }

    public Controls Controls => this.agent.Controls;

    /// <summary>The instance UID, as <c>service.instance.id</c> reads it.</summary>
    public string Uid => this.agent.Uid;

    /// <summary>
    /// Before automatic instrumentation reads its settings: load the saved settings, put them in
    /// force, switch off the instrumentations they name (<paramref name="setenv"/>), name the
    /// instance. Null when OTEL_OPAMP_ENDPOINT is unset, or another process has the state directory.
    /// <paramref name="clock"/>: the system's, but in tests. <paramref name="app"/>: where the
    /// application reads its own settings, <see cref="OpAmpAppSettings"/>'s but in tests. If it
    /// throws, nothing it prepared outlives it: the agent is stopped (no timer puts the saved
    /// settings back in force later) and the application reads the settings it read before.
    /// </summary>
    public static Agent? Prepare(Func<string, string?> getenv, Action<string, string> setenv, Clock? clock = null, AppSettingsStore? app = null)
    {
        var server = Server.FromEnv(getenv, Log.Warn);
        if (server is null)
        {
            return null;
        }

        if (!StateDirs.Claim(ref server, Log.Sink, out var stateLock))
        {
            return null;
        }

        app ??= OpAmpAppSettings.Store;
        var before = app.Snapshot();
        Agent? agent = null;
        try
        {
            agent = new Agent(server, getenv, stateLock, clock ?? Clock.System, app);
            if (!agent.fileConfig)
            {
                foreach (var (name, value) in Env.Switches(agent.agent.AtStart))
                {
                    setenv(name, value);
                }
            }

            return agent;
        }
        catch
        {
            if (agent is not null)
            {
                agent.Stop(TimeSpan.Zero); // its timer too; and the state directory's lock
            }
            else
            {
                stateLock?.Dispose();
            }

            // A plugin that didn't start runs unmanaged: the saved values the agent put in force go.
            app.Put(before);
            throw;
        }
    }

    /// <summary>
    /// When the tracer provider is built: the live sampler, wrapping the one the configuration set
    /// (read from the builder) or the one OTEL_TRACES_SAMPLER names.
    /// </summary>
    public void WrapSampler(TracerProviderBuilder builder)
    {
        if (builder is not IDeferredTracerProviderBuilder deferred)
        {
            builder.SetSampler(this.Controls.Sampler.Wrap(Variables.SamplerFromEnv(this.getenv)));
            this.samplerWrapped = true;
            this.envSampler = !this.fileConfig;
            return;
        }

        deferred.Configure((_, inner) =>
        {
            if (!Samplers.TryConfigured(inner, out var configured) && this.fileConfig)
            {
                Log.Warn("the configured sampler can't be read: sampling_rate is not carried out");
                return;
            }

            inner.SetSampler(this.Controls.Sampler.Wrap(configured ?? Variables.SamplerFromEnv(this.getenv)));
            this.samplerWrapped = true;
            this.envSampler = configured is null && !this.fileConfig;
        });
    }

    /// <summary>Gate a signal's OTLP exporter, if it can be gated: held to the signal's policy (levels, caps, redaction) and counted.</summary>
    public void Gate(OtlpExporterOptions options, Signal signal)
    {
        if (!this.gated.Contains(signal))
        {
            return;
        }

        Controls.Gate(options, this.Controls, signal, this.tls);
    }

    /// <summary>The tracer or meter provider automatic instrumentation built: what it exports through is read from it (<see cref="Exporters"/>).</summary>
    public void Built(BaseProvider provider)
    {
        lock (this.logs)
        {
            if (provider is TracerProvider t)
            {
                this.tracing = t;
            }
            else if (provider is MeterProvider m)
            {
                this.metrics = m;
            }
        }
    }

    /// <summary>
    /// A logger provider's options, as automatic instrumentation configures them for each logger
    /// factory the application creates (it builds a logger provider for each): a processor of the
    /// plugin's is added first, which exports nothing. Through it the plugin reads what the
    /// provider exports through, once it is built, and as it handles its first log record, before
    /// any exporter does, asks the agent to check again (<see cref="SdkAgent.Recheck"/>): another
    /// plugin's processor exports past the gate.
    /// </summary>
    public void Watch(OpenTelemetryLoggerOptions options)
    {
        var watch = new LogsWatch(this.agent);
        lock (this.logs)
        {
            this.logs.RemoveAll(w => !w.TryGetTarget(out _));
            this.logs.Add(new WeakReference<LogsWatch>(watch));
        }

        options.AddProcessor(watch);
    }

    /// <summary>The name a provider of the application's own, which the plugin can't see into, is listed under.</summary>
    internal const string AppProvider = "app_provider";

    /// <summary>What the built providers export through, each provider apart (several logger providers, say).</summary>
    private List<List<Exporters.Built>> BuiltExporters()
    {
        List<(Signal, object?)> providers;
        lock (this.logs)
        {
            providers = [(Signal.Traces, this.tracing), (Signal.Metrics, this.metrics)];
            foreach (var w in this.logs)
            {
                if (w.TryGetTarget(out var watch))
                {
                    providers.Add((Signal.Logs, watch.ParentProvider));
                }
            }
        }

        return providers.Select(p => Exporters.Read([p])).ToList();
    }

    /// <summary>ASP.NET Core's instrumentation options: the sampler's Filter, chained after any the configuration set.</summary>
    public void Filter(object options)
    {
        if (!AspNetCoreFilter.Install(options, this.Controls.Sampler))
        {
            Log.Warn("the ASP.NET Core instrumentation's Filter can't be set: sampling rules don't see its requests' paths");
        }
    }

    /// <summary>Once the SDK is built: join the fleet, reporting what it runs.</summary>
    public void Start(IEnumerable<KeyValuePair<string, object>> resource)
    {
        // What the gates aren't in front of is warned about as the inventory is first listed.
        this.agent.Start(resource, () => this.Inventory());
    }

    /// <summary>
    /// What it runs (docs/available-components.md): every instrumentation automatic instrumentation
    /// has on .NET, switched off or not, under the name <c>deactivate_instrumentations</c> takes,
    /// at automatic instrumentation's version (they ship together; none when it isn't loaded); and
    /// each signal's exporters as <c>OTEL_*_EXPORTER</c> names them, the OTLP one gated where the
    /// plugin gates it, each sending where its variables say. Then what the built providers export through
    /// beside those (another plugin's processor, or the file's exporters under file-based
    /// configuration, whose variables the SDK doesn't read), as the providers say.
    /// The layer names each and says where it sends (<see cref="SdkAgent.Exporters"/>).
    /// <paramref name="autoVersion"/>: automatic instrumentation's version, but in tests.
    /// </summary>
    internal Inventory Inventory(string? autoVersion = null)
    {
        autoVersion ??= AutoInstrumentationVersion();
        var instrumentations = Env.Runnable.Select(name => Inventories.Scoped(name, autoVersion));
        var configured = new List<Exporters.Built>();
        foreach (var signal in Enum.GetValues<Signal>())
        {
            if (this.fileConfig || !Env.SetsUp(this.getenv, signal))
            {
                continue;
            }

            foreach (var name in Env.Exporters(this.getenv, signal))
            {
                configured.Add(new(signal, name, name == "otlp" && this.gated.Contains(signal), FromEnv: true));
            }
        }

        // Of each provider, what isn't one of those: each configured exporter accounts for one built
        // of the name it is listed under and as gated, whose endpoint says where it sends. (By name
        // alone, another plugin's OTLP exporter ahead of ours would be taken for it.)
        var beside = new List<Exporters.Built>();
        var names = this.agent.ExporterNames(configured);
        foreach (var provider in this.BuiltExporters())
        {
            var left = Enumerable.Range(0, configured.Count).ToList();
            var named = this.agent.ExporterNames(provider);
            foreach (var (e, name) in provider.Zip(named))
            {
                var i = left.FindIndex(k => configured[k].Signal == e.Signal && configured[k].Gated == e.Gated && names[k] == name);
                if (i < 0)
                {
                    beside.Add(e);
                    continue;
                }

                var at = left[i];
                left.RemoveAt(i);
                if (e.Endpoint.Length > 0)
                {
                    configured[at] = configured[at] with { Endpoint = e.Endpoint };
                }
            }
        }

        // The application's own tracer and meter providers (OTEL_DOTNET_AUTO_SETUP_SDK=false), which
        // the plugin never sees: one ungoverned exporter each, as every SDK reports such a provider.
        foreach (var signal in new[] { Signal.Traces, Signal.Metrics }.Where(s => !this.fileConfig && !Env.SetsUpSdk(this.getenv, s)))
        {
            beside.Add(new(signal, AppProvider, false));
        }

        // The SDK's OTLP protocol when no variable sets one: http/protobuf.
        var exporters = this.agent.Exporters(configured.Distinct().Concat(beside.Distinct()), "http/protobuf");
        return Inventories.Of(typeof(Agent).Assembly, instrumentations, exporters);
    }

    /// <summary>Automatic instrumentation's version, from its assembly if the process has it loaded; else unknown (empty).</summary>
    private static string AutoInstrumentationVersion() =>
        AppDomain.CurrentDomain.GetAssemblies().FirstOrDefault(a => a.GetName().Name == "OpenTelemetry.AutoInstrumentation") is { } auto
            ? Inventories.Version(auto)
            : string.Empty;

    /// <summary>
    /// The server hears the agent go (agent_disconnect); then the SDK may flush, and the state
    /// directory is free for the next process. It runs as the process exits: it never throws.
    /// </summary>
    public void Stop(TimeSpan timeout) => this.agent.Stop(timeout);

    /// <summary>Wait for the client to stop, at most <paramref name="timeout"/>; a client that failed has stopped too.</summary>
    internal static void Await(Task stopping, TimeSpan timeout) => SdkAgent.Await(stopping, timeout);

    /// <summary>Carry out the layer's decision on a remote config; the answer for the server.</summary>
    public RemoteConfigStatus RemoteConfig(AgentRemoteConfig config) => this.agent.RemoteConfig(config);

    /// <summary>The effective config the server is told.</summary>
    internal EffectiveConfig Effective => this.agent.Effective;

    /// <summary>The settings, the client and the timer under it.</summary>
    internal SdkAgent Sdk => this.agent;

    /// <summary>
    /// What this SDK carries out, declared to the layer, which sets aside any setting not named
    /// (docs/sdk-settings.md, "The host"). Every gate's setting: it governs each exporter the gates
    /// are in front of, and one they aren't (not OTLP, another plugin's) is reported ungoverned in
    /// the inventory and export health, narrowing nothing. Once the providers are built, whether
    /// the sampler could be wrapped is known (no tracer provider: it wasn't).
    /// </summary>
    internal Host HostNow(bool providersBuilt)
    {
        var h = new Host();
        if (!this.fileConfig)
        {
            // Read as the application starts, from the instrumentation variables.
            h.NextStart.Add("deactivate_instrumentations");
            h.NextStart.Add("deactivate_all_instrumentations");
        }

        h.Live.Add("send_traces");
        h.Live.Add("send_metrics");
        h.Live.Add("send_logs");
        // At the logs exporter's gate.
        h.Live.Add("send_logs_level");
        h.Live.Add("send_logs_level_by_scope");
        h.Live.Add("send_logs_per_second");
        // At the traces and logs gates.
        h.Live.Add("drop_attributes");
        h.Live.Add("mask_attributes");
        h.Live.Add("scrub_defaults");
        h.Live.Add("scrub_patterns");

        if (!providersBuilt || this.samplerWrapped)
        {
            // In the live sampler.
            h.Live.Add("sampling_rate");
            h.Live.Add("sampling_rules");
            h.Live.Add("sampling_traces_per_second");
        }

        // Debug mode: the layer folds it into the settings it hands over, and SdkAgent's timer
        // ends it. Where sampling_rate or send_logs_level is not declared, the layer refuses a
        // policy that overrides the debug rate or level, as it refuses one that overrides those.
        h.Live.Add("debug_until");
        h.Live.Add("debug_sampling_rate");
        h.Live.Add("debug_send_logs_level");

        // The application's own settings (docs/app-settings.md), which it reads with OpAmpAppSettings.
        h.Live.Add("custom");

        if (providersBuilt)
        {
            h.Local = this.LocalNow();
        }

        return h;
    }

    /// <summary>
    /// What the SDK does without the server (contracts' logfire-sdk-local/v1), once the providers
    /// are built, as far as it can be told: the rate of the sampler OTEL_TRACES_SAMPLER names, where
    /// the live sampler wraps that one; and, for each signal automatic instrumentation builds the
    /// provider of, whether it is exported at all (set up, with an exporter its variables name or one
    /// the built providers export through). A signal the application or a configuration file sets
    /// up is left out, and so is the log level: the SDK has none of its own.
    /// </summary>
    internal Local LocalNow()
    {
        var local = new Local();
        if (this.samplerWrapped && this.envSampler)
        {
            local.SamplingRate = Variables.Decimal(Variables.SamplingRateFromEnv(this.getenv));
        }

        if (this.fileConfig)
        {
            return local;
        }

        var built = this.BuiltExporters().SelectMany(p => p).ToList();
        bool Exports(Signal s) => Env.Enabled(this.getenv, s) && (Env.Exporters(this.getenv, s).Any() || built.Any(e => e.Signal == s));
        if (Env.SetsUpSdk(this.getenv, Signal.Traces))
        {
            local.SendTraces = Exports(Signal.Traces);
        }

        if (Env.SetsUpSdk(this.getenv, Signal.Metrics))
        {
            local.SendMetrics = Exports(Signal.Metrics);
        }

        local.SendLogs = Exports(Signal.Logs);
        return local;
    }

    /// <summary>A notification, on the client's loop: answered through <paramref name="client"/>.</summary>
    internal void Notified(OpAmpClient client, OpAmpNotification n) => this.agent.Notified(client, n);


    /// <summary>
    /// Whether a signal's OTLP exporter can be gated: it has one (the gate is on the OTLP client;
    /// another beside it is reported ungoverned). With file-based
    /// configuration, never; nor for traces and metrics when automatic instrumentation leaves the
    /// SDK to the application (<c>OTEL_DOTNET_AUTO_SETUP_SDK=false</c>): its exporters aren't the
    /// plugin's to gate.
    /// </summary>
    private bool Gateable(Signal signal)
    {
        if (this.fileConfig || !Env.SetsUpSdk(this.getenv, signal))
        {
            return false;
        }

        var name = signal.ToString().ToUpperInvariant();
        var exporters = (this.getenv($"OTEL_{name}_EXPORTER") ?? "otlp").Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries);
        return exporters.Contains("otlp");
    }
}

/// <summary>
/// The plugin's processor in each logger provider automatic instrumentation builds: it exports
/// nothing. The provider it is in is what the plugin reads the provider's exporters from
/// (<see cref="BaseProcessor{T}.ParentProvider"/>). Its first log record, which it sees before the
/// provider's exporters do, has the agent check again, off the application's thread.
/// </summary>
internal sealed class LogsWatch(SdkAgent agent) : BaseProcessor<OpenTelemetry.Logs.LogRecord>
{
    private int seen;

    public override void OnEnd(OpenTelemetry.Logs.LogRecord data)
    {
        if (Volatile.Read(ref this.seen) == 0 && Interlocked.Exchange(ref this.seen, 1) == 0)
        {
            ThreadPool.UnsafeQueueUserWorkItem(a => a.Recheck(), agent, preferLocal: false);
        }
    }
}

/// <summary>
/// What the plugin says: warnings and errors always, information at OTEL_LOG_LEVEL info or debug, the rest at debug.
/// The first connection too without OTEL_LOG_LEVEL, so that working and not loaded don't look alike.
/// </summary>
internal static class Log
{
    private static readonly string? Level = Environment.GetEnvironmentVariable("OTEL_LOG_LEVEL")?.Trim().ToLowerInvariant();

    private static readonly bool Verbose = Level is "info" or "debug";

    /// <summary>The same, for the agent the plugin shares with the hosting package.</summary>
    public static readonly AgentLog Sink = new(Info, Warn, Error, Debug, Notice);

    public static void Debug(string message)
    {
        if (Level is "debug")
        {
            Console.Error.WriteLine($"OpAMP: {message}");
        }
    }

    public static void Info(string message)
    {
        if (Verbose)
        {
            Console.Error.WriteLine($"OpAMP: {message}");
        }
    }

    public static void Notice(string message)
    {
        if (Level is null || Verbose)
        {
            Console.Error.WriteLine($"OpAMP: {message}");
        }
    }

    public static void Warn(string message) => Console.Error.WriteLine($"OpAMP: {message}");

    public static void Error(string message) => Console.Error.WriteLine($"OpAMP: {message}");
}
