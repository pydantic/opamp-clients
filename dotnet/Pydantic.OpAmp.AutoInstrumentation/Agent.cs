// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;

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
    private bool samplerWrapped;

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

        if (!StateDirs.Claim(ref server, getenv, Log.Sink, out var stateLock))
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

    /// <summary>ASP.NET Core's instrumentation options: the sampler's Filter, chained after any the configuration set.</summary>
    public void Filter(object options)
    {
        if (!AspNetCoreFilter.Install(options, this.Controls.Sampler))
        {
            Log.Warn("the ASP.NET Core instrumentation's Filter can't be set: sampling rules don't see its requests' paths");
        }
    }

    /// <summary>Once the SDK is built: join the fleet, reporting what it runs.</summary>
    public void Start(IEnumerable<KeyValuePair<string, object>> resource) => this.agent.Start(resource, this.Inventory());

    /// <summary>
    /// What it runs (docs/available-components.md): every instrumentation automatic instrumentation
    /// has on .NET, switched off or not, under the name <c>deactivate_instrumentations</c> takes,
    /// at automatic instrumentation's version (they ship together; none when it isn't loaded); and
    /// each signal's exporters as <c>OTEL_*_EXPORTER</c> names them, gated where the signal's
    /// <c>send_*</c> is carried out. With file-based configuration the exporters are the file's,
    /// which the plugin can't read: none is listed.
    /// <paramref name="autoVersion"/>: automatic instrumentation's version, but in tests.
    /// </summary>
    internal Inventory Inventory(string? autoVersion = null)
    {
        autoVersion ??= AutoInstrumentationVersion();
        var instrumentations = Env.Runnable.Select(name => new Instrumentation { Name = name, Version = autoVersion });
        var exporters = new List<Exporter>();
        foreach (var signal in Enum.GetValues<Signal>())
        {
            if (this.fileConfig || !Env.Enabled(this.getenv, signal))
            {
                continue;
            }

            foreach (var name in Env.Exporters(this.getenv, signal))
            {
                exporters.Add(Inventories.Exporter(signal, name, this.gated.Contains(signal)));
            }
        }

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
    /// (docs/sdk-settings.md, "The host"). Once the providers are built, whether the sampler could
    /// be wrapped is known too (no tracer provider: it wasn't).
    /// </summary>
    internal Host HostNow(bool providersBuilt)
    {
        var h = new Host { NextStart = { "opamp_polling_interval" } };
        if (!this.fileConfig)
        {
            // Read as the application starts, from the instrumentation variables.
            h.NextStart.Add("deactivate_instrumentations");
            h.NextStart.Add("deactivate_all_instrumentations");
        }

        foreach (var signal in Enum.GetValues<Signal>().Where(this.gated.Contains))
        {
            h.Live.Add($"send_{signal.ToString().ToLowerInvariant()}");
        }

        if (this.gated.Contains(Signal.Logs))
        {
            // The levels and the cap are held at the logs exporter's gate.
            h.Live.Add("send_logs_level");
            h.Live.Add("send_logs_level_by_scope");
            h.Live.Add("send_logs_per_second");
        }

        // Redaction is at the traces and logs gates: every span and log record exported has to go
        // through one, or nothing is exported at all.
        if (this.Covered(Signal.Traces) && this.Covered(Signal.Logs))
        {
            h.Live.Add("drop_attributes");
            h.Live.Add("mask_attributes");
        }

        if (!providersBuilt || this.samplerWrapped)
        {
            // In the live sampler.
            h.Live.Add("sampling_rate");
            h.Live.Add("sampling_rules");
            h.Live.Add("sampling_traces_per_second");
        }

        // Debug mode: the layer folds it into the settings it hands over, and SdkAgent's timer
        // ends it. Its rate and level are set aside, as sampling_rate and send_logs_level are, where
        // those aren't declared.
        h.Live.Add("debug_until");
        h.Live.Add("debug_sampling_rate");
        h.Live.Add("debug_send_logs_level");

        // The application's own settings (docs/app-settings.md), which it reads with OpAmpAppSettings.
        h.Live.Add("custom");

        return h;
    }

    /// <summary>A notification, on the client's loop: answered through <paramref name="client"/>.</summary>
    internal void Notified(OpAmpClient client, OpAmpNotification n) => this.agent.Notified(client, n);

    /// <summary>Whether everything a signal exports goes through its gate: it is gated, or exported nowhere (<c>none</c>).</summary>
    private bool Covered(Signal signal) =>
        this.gated.Contains(signal) || (!this.fileConfig &&
            (this.getenv($"OTEL_{signal.ToString().ToUpperInvariant()}_EXPORTER") ?? "otlp")
                .Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries).All(e => e == "none"));

    /// <summary>
    /// Whether a signal's exports can be stopped: its every exporter is OTLP (the gate is on the
    /// OTLP client; beside another, only half the setting would be carried out). With file-based
    /// configuration, never.
    /// </summary>
    private bool Gateable(Signal signal)
    {
        if (this.fileConfig)
        {
            return false;
        }

        var name = signal.ToString().ToUpperInvariant();
        var exporters = (this.getenv($"OTEL_{name}_EXPORTER") ?? "otlp").Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries);
        return exporters.All(e => e == "otlp");
    }
}

/// <summary>What the plugin says: warnings and errors always, information at OTEL_LOG_LEVEL info or debug, the rest at debug.</summary>
internal static class Log
{
    private static readonly string? Level = Environment.GetEnvironmentVariable("OTEL_LOG_LEVEL")?.Trim().ToLowerInvariant();

    private static readonly bool Verbose = Level is "info" or "debug";

    /// <summary>The same, for the agent the plugin shares with the hosting package.</summary>
    public static readonly AgentLog Sink = new(Info, Warn, Error, Debug);

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

    public static void Warn(string message) => Console.Error.WriteLine($"OpAMP: {message}");

    public static void Error(string message) => Console.Error.WriteLine($"OpAMP: {message}");
}
