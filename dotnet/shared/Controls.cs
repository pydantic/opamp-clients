// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net;
using System.Reflection;
using OpenTelemetry.Exporter;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>A signal an OTLP exporter sends.</summary>
internal enum Signal
{
    Traces,
    Metrics,
    Logs,
}

/// <summary>
/// What the server changes while the SDK runs: the sampler (rate, rules, trace cap), whether each signal is sent, and what
/// the gates let through of what is sent (the log levels, the log rate cap, and redaction).
/// </summary>
internal sealed class Controls
{
    // Where OTLP sends each signal: over HTTP, then over gRPC.
    private static readonly (Signal Signal, string Http, string Grpc)[] Paths =
    [
        (Signal.Traces, "/v1/traces", "/opentelemetry.proto.collector.trace.v1.TraceService/Export"),
        (Signal.Metrics, "/v1/metrics", "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export"),
        (Signal.Logs, "/v1/logs", "/opentelemetry.proto.collector.logs.v1.LogsService/Export"),
    ];

    // What the gates hold exports to, swapped whole by Apply: a gate reads it once per export, so
    // an export is held to one settings' send_* and policy, never to the send_* of one and the
    // policy of the one before.
    private volatile Gates gates = Gates.Default;

    /// <summary>The monotonic clock the rate caps refill on, in nanoseconds.</summary>
    private readonly Func<long> nanos;

    // Apply: one document at a time, the SDK's settings and custom together.
    private readonly object applying = new();

    /// <summary>
    /// Controls on the process's monotonic clock, or <paramref name="nanos"/> (tests); the
    /// application's own settings put in force on <paramref name="app"/> (null: a store of their own;
    /// the integrations pass <see cref="OpAmpAppSettings"/>'s, which the application reads).
    /// </summary>
    public Controls(Func<long>? nanos = null, AppSettingsStore? app = null)
    {
        this.nanos = nanos ?? TokenBucket.Monotonic;
        this.Sampler = new LiveSampler(this.nanos);
        this.Sampler.Capped = () => this.Tally.Filtered(Signal.Traces, 1);
        this.App = app ?? new AppSettingsStore();
    }

    public LiveSampler Sampler { get; }

    /// <summary>The application's own settings (custom), as it reads them.</summary>
    public AppSettingsStore App { get; }

    /// <summary>Export health's counts, kept by the gates and the sampler's trace cap.</summary>
    public ExportTally Tally { get; } = new();

    public bool SendTraces => this.gates.SendTraces;

    public bool SendMetrics => this.gates.SendMetrics;

    public bool SendLogs => this.gates.SendLogs;

    /// <summary>The lowest OpenTelemetry SeverityNumber of the log records sent (send_logs_level); 0: every one.</summary>
    public uint LogsFloor => this.gates.Logs.Levels?.Floor ?? 0;

    /// <summary>The log levels in force: send_logs_level and send_logs_level_by_scope.</summary>
    public LogLevels LogLevels => this.gates.Logs.Levels ?? new LogLevels(0, []);

    /// <summary>Whether a signal is sent.</summary>
    public bool Sends(Signal signal) => this.Held(signal).Send;

    /// <summary>What a signal's exports are held to at its gate.</summary>
    public ExportPolicy Policy(Signal signal) => this.Held(signal).Policy;

    /// <summary>Whether a signal is sent, and what its exports are held to, as one settings put them: what a gate reads for an export.</summary>
    public (bool Send, ExportPolicy Policy) Held(Signal signal)
    {
        var g = this.gates;
        return signal switch
        {
            Signal.Traces => (g.SendTraces, g.Traces),
            Signal.Metrics => (g.SendMetrics, ExportPolicy.None),
            _ => (g.SendLogs, g.Logs),
        };
    }

    /// <summary>
    /// Carry out the live settings; a setting left out is the SDK's own. The application's own
    /// settings (custom) are put in force last, so a handler given to
    /// <see cref="OpAmpAppSettings.OnChange"/> sees the SDK's settings already in force; it runs on
    /// the calling thread. Both are put in force under one lock, so the SDK's settings and the
    /// application's always come from the same document.
    /// </summary>
    public void Apply(Settings s)
    {
        lock (this.applying)
        {
            this.ApplySdk(s);
            this.App.Put(s.Custom);
        }
    }

    /// <summary>The SDK's own settings; <see cref="applying"/> is held.</summary>
    private void ApplySdk(Settings s)
    {
        this.Sampler.Apply(s.HasSamplingRate ? s.SamplingRate : null, [.. s.SamplingRules], s.HasSamplingTracesPerSecond ? s.SamplingTracesPerSecond : null);
        var levels = new LogLevels(s.HasSendLogsMinSeverity ? s.SendLogsMinSeverity : 0, [.. s.SendLogsScopeLevels]);
        var redact = Redactor.Of([.. s.DropAttributes], [.. s.MaskAttributes], Scrubber.Of(s.ScrubDefaults, [.. s.ScrubPatterns]));
        this.gates = new Gates(
            !s.HasSendTraces || s.SendTraces,
            !s.HasSendMetrics || s.SendMetrics,
            !s.HasSendLogs || s.SendLogs,
            new ExportPolicy(levels.Active ? levels : null, this.Bucket(this.gates.Logs.Cap, s.HasSendLogsPerSecond ? s.SendLogsPerSecond : null), redact),
            new ExportPolicy(Redact: redact));
    }

    /// <summary>Whether each signal is sent, and the logs' and traces' policies: one settings' worth, never changed.</summary>
    private sealed record Gates(bool SendTraces, bool SendMetrics, bool SendLogs, ExportPolicy Logs, ExportPolicy Traces)
    {
        public static readonly Gates Default = new(true, true, true, ExportPolicy.None, ExportPolicy.None);
    }

    /// <summary>A rate cap's bucket for <paramref name="perSecond"/>: the one in force if the rate is the same, else a full one; null: no cap.</summary>
    private TokenBucket? Bucket(TokenBucket? current, double? perSecond) =>
        perSecond is not { } n ? null : current is { } b && b.PerSecond == n ? b : new TokenBucket(n, this.nanos);

    /// <summary>
    /// Gate a signal's OTLP exporter: its requests go through a handler (<see cref="GateHandler"/>)
    /// that answers them itself with success while the signal isn't sent, and holds what is sent
    /// to the signal's policy. It replaces the exporter's own HTTP client, as <c>HttpClientFactory</c>
    /// does, keeping the timeout, and the TLS the SDK would have built into it (<paramref name="tls"/>).
    /// <paramref name="signal"/> null: the exporter's signal isn't known when it is configured, so
    /// each request's is its path's (<see cref="SignalOf"/>).
    /// </summary>
    public static void Gate(OtlpExporterOptions options, Controls controls, Signal? signal, OtlpTls? tls = null)
    {
        var timeout = TimeSpan.FromMilliseconds(options.TimeoutMilliseconds);
        options.HttpClientFactory = () =>
        {
            var inner = new SocketsHttpHandler();
            tls?.Configure(inner);
            return new HttpClient(new GateHandler(controls, signal) { InnerHandler = inner }) { Timeout = timeout };
        };
    }

    /// <summary>
    /// The signal an export is of, by the path OTLP gives it: <c>/v1/traces</c> over HTTP, the
    /// service's method over gRPC. Null for any other path (an endpoint set to one of its own).
    /// </summary>
    public static Signal? SignalOf(Uri? uri)
    {
        var path = uri is null ? string.Empty : uri.IsAbsoluteUri ? uri.AbsolutePath : uri.OriginalString;
        foreach (var (signal, http, grpc) in Paths)
        {
            if (path.EndsWith(http, StringComparison.Ordinal) || path.EndsWith(grpc, StringComparison.Ordinal))
            {
                return signal;
            }
        }

        return null;
    }
}

/// <summary>
/// An OTLP exporter's gate: answers an export itself while its signal isn't sent; holds what it
/// sends to the signal's policy (<see cref="OtlpRewriter"/>), and answers itself an export with
/// nothing left in it. It counts each attempt for export health (<see cref="ExportTally"/>): an
/// HTTP status, gRPC's status in the trailers, or an exception.
/// </summary>
/// <param name="controls">What the server set.</param>
/// <param name="signal">The exporter's signal; null: each request's, by its path (one whose path names none goes as it is).</param>
internal sealed class GateHandler(Controls controls, Signal? signal = null) : DelegatingHandler
{
    /// <summary>The signal it was given; null: each request's, by its path.</summary>
    public Signal? Signal => signal;

    // http/protobuf sends synchronously; gRPC asynchronously.
    protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var s = signal ?? Controls.SignalOf(request.RequestUri);
        var admitted = this.Admit(request, s);
        if (!admitted.Send)
        {
            return Dropped(request);
        }

        try
        {
            var response = base.Send(request, cancellationToken);
            this.Count(s, request, response, admitted.Items);
            return response;
        }
        catch (Exception e)
        {
            this.Count(s, request, e, admitted.Items);
            throw;
        }
    }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var s = signal ?? Controls.SignalOf(request.RequestUri);
        var admitted = this.Admit(request, s);
        if (!admitted.Send)
        {
            return Dropped(request);
        }

        try
        {
            var response = await base.SendAsync(request, cancellationToken).ConfigureAwait(false);
            if (s is not null && IsGrpc(request) && response.IsSuccessStatusCode)
            {
                // gRPC's status comes in the trailers, after the body: read it, buffered, so the
                // exporter still reads it as it would.
                await response.Content.LoadIntoBufferAsync().ConfigureAwait(false);
            }

            this.Count(s, request, response, admitted.Items);
            return response;
        }
        catch (Exception e)
        {
            this.Count(s, request, e, admitted.Items);
            throw;
        }
    }

    internal static HttpResponseMessage Dropped(HttpRequestMessage request)
    {
        var response = new HttpResponseMessage(HttpStatusCode.OK) { RequestMessage = request, Content = new ByteArrayContent([]) };
        if (IsGrpc(request))
        {
            response.Headers.TryAddWithoutValidation("grpc-status", "0");
        }

        return response;
    }

    private static bool IsGrpc(HttpRequestMessage request) => request.Content?.Headers.ContentType?.MediaType == "application/grpc";

    /// <summary>The gRPC status of a response: in its trailers, or its headers for a trailers-only one; null if neither has it.</summary>
    private static (int Code, string? Message)? GrpcStatus(HttpResponseMessage response)
    {
        foreach (var headers in new System.Net.Http.Headers.HttpHeaders[] { response.TrailingHeaders, response.Headers })
        {
            if (headers.TryGetValues("grpc-status", out var values) && int.TryParse(values.FirstOrDefault(), out var code))
            {
                var message = headers.TryGetValues("grpc-message", out var m) ? Uri.UnescapeDataString(m.FirstOrDefault() ?? string.Empty) : null;
                return (code, message);
            }
        }

        return null;
    }

    private void Count(Signal? s, HttpRequestMessage request, HttpResponseMessage response, int? items)
    {
        if (s is not { } sig)
        {
            return;
        }

        string? error = null;
        if (!response.IsSuccessStatusCode)
        {
            error = $"HTTP {(int)response.StatusCode} {response.ReasonPhrase}".TrimEnd();
        }
        else if (IsGrpc(request) && GrpcStatus(response) is { Code: not 0 } status)
        {
            error = string.IsNullOrEmpty(status.Message) ? $"gRPC status {status.Code}" : $"gRPC status {status.Code}: {status.Message}";
        }

        if (error is null)
        {
            controls.Tally.Ok(sig);
        }
        else
        {
            controls.Tally.Failed(sig, items ?? ItemsIn(request, sig), error);
        }
    }

    private void Count(Signal? s, HttpRequestMessage request, Exception e, int? items)
    {
        if (s is { } sig)
        {
            var error = e is TaskCanceledException or OperationCanceledException ? "timed out" : e.GetBaseException().Message;
            controls.Tally.Failed(sig, items ?? ItemsIn(request, sig), error);
        }
    }

    /// <summary>The items in a request's body, for a failed export the gate didn't read before; 0 if it can't be read.</summary>
    private static int ItemsIn(HttpRequestMessage request, Signal signal)
    {
        try
        {
            return request.Content is { } content && Read(request, content) is var (body, grpc, encoding)
                ? OtlpRewriter.Rewrite(body, grpc, encoding, signal, ExportPolicy.None).Items
                : 0;
        }
        catch (Exception e) when (e is IOException or ObjectDisposedException or InvalidOperationException)
        {
            return 0;
        }
    }

    private static (byte[] Body, bool Grpc, string? Encoding) Read(HttpRequestMessage request, HttpContent content)
    {
        using var buffer = new MemoryStream();
        content.ReadAsStream().CopyTo(buffer);
        var grpc = content.Headers.ContentType?.MediaType == "application/grpc";
        // http/protobuf names its compression in Content-Encoding; gRPC in grpc-encoding.
        var encoding = grpc
            ? request.Headers.TryGetValues("grpc-encoding", out var codecs) ? codecs.FirstOrDefault() : null
            : content.Headers.ContentEncoding.Count == 0 ? null : string.Join(",", content.Headers.ContentEncoding);
        return (buffer.ToArray(), grpc, encoding);
    }

    /// <summary>
    /// Whether to send a request, held to its signal's policy: not when its signal isn't sent, or
    /// nothing is left in it. A body that can't be read as OTLP is sent as it is, unless it was to
    /// be redacted: then it isn't sent at all (fail closed), and counts as a failed export. The
    /// items it carried, when the gate read it; what the levels and the cap dropped counts as
    /// filtered.
    /// </summary>
    private (bool Send, int? Items) Admit(HttpRequestMessage request, Signal? s)
    {
        if (s is not { } sig)
        {
            return (true, null);
        }

        // Read once: whether it is sent and how, as one settings put them.
        var (send, policy) = controls.Held(sig);
        if (!send)
        {
            return (false, null); // off on purpose: health reports the signal off
        }

        if (!policy.Changes(sig) || request.Content is not { } content)
        {
            return (true, null);
        }

        // The exporter's content is its serialized request, in memory: read at once.
        var (body, grpc, encoding) = Read(request, content);
        var rewritten = OtlpRewriter.Rewrite(body, grpc, encoding, sig, policy);
        if (!rewritten.Readable && policy.Redact is not null)
        {
            // Fail closed: what can't be read can't be redacted, so it isn't sent.
            controls.Tally.Failed(sig, 0, "an export the gate can't read (not OTLP protobuf, or an encoding other than gzip) isn't sent while attributes are redacted");
            return (false, null);
        }

        if (rewritten.Readable)
        {
            controls.Tally.Filtered(sig, rewritten.Items - rewritten.Kept);
            if (rewritten.Kept == 0 && rewritten.Items > 0)
            {
                return (false, null);
            }
        }

        // Read once: the request goes on with the bytes read, whatever the content was.
        var replaced = new ByteArrayContent(rewritten.Body ?? body);
        foreach (var (name, values) in content.Headers)
        {
            if (!name.Equals("Content-Length", StringComparison.OrdinalIgnoreCase))
            {
                replaced.Headers.TryAddWithoutValidation(name, values);
            }
        }

        request.Content = replaced;
        content.Dispose();
        return (true, rewritten.Readable ? rewritten.Kept : null);
    }
}

/// <summary>The sampler a tracer provider builder has been given so far.</summary>
internal static class Samplers
{
    /// <summary>
    /// The SDK's builder keeps it in a property of its internal type: false when there is none to
    /// read. <paramref name="sampler"/> is null when none was set (the SDK's default then).
    /// </summary>
    public static bool TryConfigured(TracerProviderBuilder inner, out Sampler? sampler)
    {
        var property = inner.GetType().GetProperty("Sampler", BindingFlags.Instance | BindingFlags.Public | BindingFlags.NonPublic);
        sampler = property?.GetValue(inner) as Sampler;
        return property is not null;
    }
}
