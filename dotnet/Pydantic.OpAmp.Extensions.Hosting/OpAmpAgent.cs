// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Reflection;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using OpAmp.Proto.V1;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;

namespace Pydantic.OpAmp.Extensions.Hosting;

/// <summary>
/// The application's SDK under the server: one per service provider. Built the first time the
/// SDK needs it (its resource, sampler or exporters), it puts the saved settings in force before
/// anything is sent; <see cref="OpAmpHostedService"/> then joins the fleet. With no endpoint, or a
/// state directory another process has, <see cref="Agent"/> is null and nothing changes.
/// </summary>
internal sealed class OpAmpAgent : IDisposable
{
    private readonly Func<string, string?> getenv;
    private readonly Log log = new();
    private readonly bool gateable;
    private readonly OtlpTls? tls;
    private TracerProviderBuilder? tracing;
    private (TracerProvider? Tracing, MeterProvider? Metrics, LoggerProvider? Logs)? built;
    private ComponentHealth? health;

    public OpAmpAgent(IOptions<OpAmpOptions> options, IConfiguration? configuration = null)
    {
        this.getenv = Getenv(configuration);
        var o = options.Value;
        foreach (var w in o.Warnings)
        {
            this.log.Sink.Warn(w);
        }

        if (o.Endpoint is null)
        {
            if (o.UnusableEndpoint is { } why)
            {
                this.log.Sink.Error($"the client did not start; the SDK runs unmanaged: {why}");
            }

            return;
        }

        // An OTLP exporter given a client certificate or a CA builds them into its own HTTP client,
        // which a gate (ours) replaces: the gate's carries them. Files it can't use leave every
        // exporter as it is, and none of the signals can be stopped.
        this.gateable = OtlpTls.TryFromEnv(this.getenv, this.log.Sink.Warn, out this.tls);

        // What the configuration and the variables said is in the options already; the core's Setup
        // decides from them, naming the configuration's settings in what it says.
        var server = Server.FromOptions(o.Endpoint, o.Headers, o.StateDirectory, o.HeartbeatInterval, this.getenv, this.log.Sink.Warn,
            "OpAmp:Endpoint", "OpAmp:Headers", "OpAmp:HeartbeatInterval");
        if (server is null || !StateDirs.Claim(ref server, this.log.Sink, out var stateLock))
        {
            return;
        }

        // An agent that fails to start leaves what the application reads as it was: the saved
        // values it put in force go.
        var before = this.Controls.App.Snapshot();
        try
        {
            this.Agent = new SdkAgent(server, this.getenv, stateLock, this.HostNow, this.log.Sink, this.Controls);
        }
        catch
        {
            stateLock?.Dispose();
            this.Controls.App.Put(before);
            throw;
        }
    }

    /// <summary>Null: unmanaged.</summary>
    public SdkAgent? Agent { get; }

    /// <summary>What the server changes; the application's own settings are <see cref="OpAmpAppSettings"/>'s.</summary>
    public Controls Controls { get; } = new(app: OpAmpAppSettings.Store);

    /// <summary>
    /// What this SDK carries out, declared to the layer, which sets aside any setting not named
    /// (docs/sdk-settings.md, "The host"): never the instrumentation switches (the application
    /// names its instrumentations in code); the sampling rate only while the live sampler can be the
    /// tracer provider's; a signal's sending only while every exporter it has can be gated
    /// (<see cref="Exporters"/>, read from the built providers each time: a processor the application
    /// adds to one later is found). Nothing waits for the next start.
    /// </summary>
    public Host HostNow(bool providersBuilt)
    {
        // Before the providers are built, an exporter not yet seen may still be gated.
        var ungated = providersBuilt ? this.Ungated() : [];
        var h = new Host();
        if (this.gateable)
        {
            foreach (var signal in Enum.GetValues<Signal>().Where(s => !ungated.Contains(s)))
            {
                h.Live.Add($"send_{signal.ToString().ToLowerInvariant()}");
            }

            if (!ungated.Contains(Signal.Logs))
            {
                // The levels are held at the logs gate, which every exporter of the logs is behind.
                h.Live.Add("send_logs_level");
                h.Live.Add("send_logs_level_by_scope");
                h.Live.Add("send_logs_per_second");
            }
        }

        // Redaction is at the traces and logs gates: every span and log record exported has to go through one.
        if (this.gateable && !ungated.Contains(Signal.Traces) && !ungated.Contains(Signal.Logs))
        {
            h.Live.Add("drop_attributes");
            h.Live.Add("mask_attributes");
        }

        if (!providersBuilt || this.SamplerInForce())
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

        return h;
    }

    /// <summary>
    /// As the tracer provider is built: the live sampler, wrapping the one the application set so
    /// far, or the one OTEL_TRACES_SAMPLER names.
    /// </summary>
    public void WrapSampler(TracerProviderBuilder inner)
    {
        if (this.Agent is null)
        {
            return;
        }

        Samplers.TryConfigured(inner, out var configured);
        inner.SetSampler(this.Controls.Sampler.Wrap(configured ?? Variables.SamplerFromEnv(this.getenv)));
        this.tracing = inner;
    }

    /// <summary>
    /// As an OTLP exporter is configured, whatever its signal: its requests go through the gate. One
    /// the application gave an HTTP client of its own keeps it, and its signal can't be stopped.
    /// </summary>
    public void Gate(OtlpExporterOptions options)
    {
        if (this.Agent is null || !this.gateable)
        {
            return;
        }

        if (!DefaultHttpClient(options))
        {
            this.log.Sink.Warn("an OTLP exporter has an HttpClientFactory of its own: its exports are sent whatever the server says, and its signal's send_* setting is not carried out");
            return;
        }

        Controls.Gate(options, this.Controls, signal: null, this.tls);
    }

    /// <summary>
    /// ASP.NET Core's instrumentation options (an <c>AspNetCoreTraceInstrumentationOptions</c>, by
    /// reflection): the sampler's Filter, chained after the application's.
    /// </summary>
    public void Filter(object options)
    {
        if (this.Agent is not null && !AspNetCoreFilter.Install(options, this.Controls.Sampler))
        {
            this.log.Sink.Warn("the ASP.NET Core instrumentation's Filter can't be set: sampling rules don't see its requests' paths");
        }
    }

    /// <summary>The application's health, reported now if the client runs, else as it starts.</summary>
    public void SetHealth(ComponentHealth value)
    {
        this.health = value;
        this.Agent?.SetHealth(value);
    }

    /// <summary>Unmanaged, once the host starts: say what was held back until there was a logger.</summary>
    public void Said(ILogger logger) => this.log.Attach(logger);

    /// <summary>
    /// Once the SDK is built (the providers the application has; null: none): join the fleet,
    /// saying what was held back until there was a logger.
    /// </summary>
    public void Start(IEnumerable<KeyValuePair<string, object>> resource, ILogger logger, TracerProvider? tracing = null, MeterProvider? metrics = null, LoggerProvider? logs = null)
    {
        this.log.Attach(logger);
        if (this.SamplerReplaced())
        {
            this.log.Sink.Warn("the tracer provider's sampler was set after WithOpAmp(): sampling_rate is not carried out. Call WithOpAmp() last.");
        }

        // Which signals go to an exporter the gate isn't in front of is known now. The host does not
        // declare their send_*, so the layer refuses a policy that overrides it, and sets it aside in
        // one in force. The providers are read again before each decision, and on the health timer.
        this.built = (tracing, metrics, logs);
        foreach (var signal in this.Ungated())
        {
            var name = signal.ToString().ToLowerInvariant();
            this.log.Sink.Info($"{name} go to an exporter that can't be stopped (not OTLP, an endpoint with a path of its own, or an HttpClientFactory of its own): send_{name} is not carried out");
        }

        this.Agent?.Start(resource, () => this.Inventory(Exporters.Read(tracing, metrics, logs).Built, tracing, metrics, logs), this.health);
    }

    /// <summary>The signals some exporter of which the built providers send through past the gates, read now; none before they are built.</summary>
    private HashSet<Signal> Ungated() =>
        this.Agent is not null && this.gateable && this.built is { } b ? Exporters.Read(b.Tracing, b.Metrics, b.Logs).Ungated : [];

    /// <summary>
    /// What it runs (docs/available-components.md): the instrumentations the application added to
    /// the providers, named as the automatic instrumentation plugin's <c>deactivate_instrumentations</c>
    /// names them, at their packages' versions; and the providers' exporters, gated where their
    /// signal's <c>send_*</c> is carried out (<see cref="HostNow"/>).
    /// </summary>
    internal Inventory Inventory(IEnumerable<(Signal Signal, string Name, bool Gated)> built, params object?[] providers)
    {
        var instrumentations = providers.SelectMany(p => p is not null && Exporters.TryMember(p, "Instrumentations", out var i) && i is System.Collections.IEnumerable list
                ? list.Cast<object?>()
                : [])
            .OfType<object>()
            .Select(i => i.GetType().Assembly)
            .Select(a => new Instrumentation { Name = InstrumentationName(a.GetName().Name ?? string.Empty), Version = Inventories.Version(a) });
        var ungated = this.Ungated();
        var exporters = built.Select(e => Inventories.Exporter(e.Signal, e.Name, e.Gated && this.gateable && !ungated.Contains(e.Signal)));
        return Inventories.Of(typeof(OpAmpAgent).Assembly, instrumentations, exporters);
    }

    /// <summary>
    /// An instrumentation package's name as automatic instrumentation's variables name it, lowercase
    /// (<c>OpenTelemetry.Instrumentation.Http</c> is <c>httpclient</c>); another's, after
    /// <c>OpenTelemetry.Instrumentation.</c>, without its punctuation, as the plugin reads names.
    /// </summary>
    internal static string InstrumentationName(string assembly)
    {
        const string prefix = "OpenTelemetry.Instrumentation.";
        var name = assembly.StartsWith(prefix, StringComparison.Ordinal) ? assembly[prefix.Length..] : assembly;
        return name switch
        {
            "Http" => "httpclient",
            "Runtime" => "netruntime",
            "ElasticsearchClient" => "elasticsearch",
            _ => string.Concat(name.Where(c => c is not ('.' or '-' or '_'))).ToLowerInvariant(),
        };
    }

    public void Dispose() => this.Agent?.Stop(TimeSpan.Zero);

    private bool SamplerInForce() =>
        this.tracing is { } t && Samplers.TryConfigured(t, out var s) && ReferenceEquals(s, this.Controls.Sampler);

    private bool SamplerReplaced() => this.tracing is not null && !this.SamplerInForce();

    /// <summary>Whether the exporter's HTTP client is still the SDK's (its internal default factory).</summary>
    private static bool DefaultHttpClient(OtlpExporterOptions options)
    {
        const BindingFlags any = BindingFlags.Instance | BindingFlags.NonPublic | BindingFlags.Public;
        var type = typeof(OtlpExporterOptions);
        var d = type.GetProperty("DefaultHttpClientFactory", any)?.GetValue(options) ?? type.GetField("DefaultHttpClientFactory", any)?.GetValue(options);
        return d is not Delegate f || ReferenceEquals(f, options.HttpClientFactory);
    }

    /// <summary>The configuration's value, else the process's variable.</summary>
    private static Func<string, string?> Getenv(IConfiguration? configuration)
    {
        var process = Variables.Process();
        return name => configuration?[name] is { Length: > 0 } v ? v : process(name);
    }

    /// <summary>
    /// The application's logger, once there is one: the agent is built while the SDK is (the
    /// logger factory among it), so what it says before is held until the host starts.
    /// </summary>
    private sealed class Log
    {
        private readonly object gate = new();
        private readonly List<(LogLevel Level, string Message)> held = [];
        private ILogger? logger;

        public Log() => this.Sink = new AgentLog(
            m => this.Say(LogLevel.Information, m), m => this.Say(LogLevel.Warning, m), m => this.Say(LogLevel.Error, m), m => this.Say(LogLevel.Debug, m));

        public AgentLog Sink { get; }

        public void Attach(ILogger to)
        {
            lock (this.gate)
            {
                this.logger = to;
                foreach (var (level, message) in this.held)
                {
                    to.Log(level, "OpAMP: {Message}", message);
                }

                this.held.Clear();
            }
        }

        private void Say(LogLevel level, string message)
        {
            lock (this.gate)
            {
                if (this.logger is null)
                {
                    this.held.Add((level, message));
                    return;
                }
            }

            this.logger.Log(level, "OpAMP: {Message}", message);
        }
    }
}
