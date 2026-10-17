// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;
using static Pydantic.OpAmp.AutoInstrumentation.Tests.Otlp;
using SdkSpanKind = Pydantic.OpAmp.Client.Sdk.SpanKind;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// The controls under fire. One applier flips through a seeded run of Logfire SDK policies (rules
/// that overlap and change order, caps coming and going, levels per scope, redaction lists growing
/// and shrinking, <c>send_*</c> on and off, debug windows on and off, every control local) while
/// workers emit spans (roots and children, with attributes, events and links) and log records
/// through the real SDK pipeline: ActivitySource and ILogger, the batch processors, the OTLP
/// exporters (http/protobuf or gRPC, gzip or not), the gates, and a collector that checks each
/// export as it arrives. Once the flipping stops, a last document is put in force and what is
/// emitted after it has to come out exactly as that document says.
/// <para>
/// A run's seed decides its documents, its pipeline and what each worker emits; the threads'
/// interleaving is the scheduler's, so a replayed seed runs the same documents and telemetry, not
/// the same interleaving. Runs: OPAMP_PROP_RUNS (default 3: one of each kind of run), each flipping
/// for OPAMP_CONCURRENCY_MS (default 400); OPAMP_CONCURRENCY_SEED=S replays the run of seed S.
/// </para>
/// </summary>
public class ConcurrencyTests(ITestOutputHelper output)
{
    /// <summary>The threads emitting telemetry while the applier flips.</summary>
    private const int Workers = 4;

    /// <summary>How long a run may take before it counts as a deadlock.</summary>
    private static readonly TimeSpan Bound = TimeSpan.FromSeconds(60);

    private static int Runs => int.TryParse(Environment.GetEnvironmentVariable("OPAMP_PROP_RUNS"), NumberStyles.None, CultureInfo.InvariantCulture, out var n) && n > 0 ? n : 3;

    private static int FlipMs => int.TryParse(Environment.GetEnvironmentVariable("OPAMP_CONCURRENCY_MS"), NumberStyles.None, CultureInfo.InvariantCulture, out var n) && n > 0 ? n : 400;

    /// <summary>
    /// What every document of a run has in common, which the invariants that hold whatever the
    /// interleaving rest on.
    /// </summary>
    private enum Kind
    {
        /// <summary>Anything, every control local too: only what holds of every document can be checked.</summary>
        Anything,

        /// <summary>Every document drops <c>always.drop</c> and masks <c>always.mask</c> (and drops it in none).</summary>
        Redacted,

        /// <summary>Every document has <c>send_logs</c> false.</summary>
        LogsOff,
    }

    private enum ParentOf
    {
        None,
        RemoteSampled,
        RemoteUnsampled,
    }

    // What the invariants can and can't check, whatever the interleaving:
    // - no exception (in the applier, a worker, the gate or the collector), and no deadlock: the
    //   run is bounded, and every export the gate let through was one it could read (no failed
    //   export in export health);
    // - Redacted: always.drop (in any ASCII case) is never exported, on a span, event, link or log
    //   record, and always.mask only ever as [REDACTED];
    // - a key every document that sends the signal drops is never exported (#343): every document
    //   but the empty one and the quiet ones drops user.password, and a quiet one sends neither
    //   traces nor logs and keeps it, so an export read under a quiet document's policy and
    //   another's send_* gives it away;
    // - a child of a sampled local parent is always sampled: the trace cap (or anything else)
    //   never drops it;
    // - LogsOff: no logs export reaches the collector at all;
    // - one export, one document: the gate rewrites an export as a whole (every attribute list in
    //   it, of every span, event, link and record), so each export has to be what one document of
    //   the run makes of it: sent under it, every list redacted as it says, every record past its
    //   levels. A list redacted half under one document and half under another fails this too.
    //   Which document can't be known, and neither can the caps' buckets (they carry over between
    //   documents with the same rate, and the batches fall where they fall), so what the caps let
    //   through is checked only after the flipping;
    // - after the flipping, everything emitted under the last document is exactly what it says:
    //   which roots are sampled (its rates are 0 or 1, so this is known without the trace IDs),
    //   which spans and records the caps let through (on a clock that has moved on, so the
    //   buckets are full), at which levels, redacted how.
    [Fact]
    public void TheControlsHoldWhileTheSettingsFlipUnderLoad()
    {
        var replay = Environment.GetEnvironmentVariable("OPAMP_CONCURRENCY_SEED");
        var first = long.TryParse(replay, NumberStyles.None, CultureInfo.InvariantCulture, out var s) ? s : Random.Shared.NextInt64(1, 1L << 40);
        var seeds = replay is { Length: > 0 } ? [first] : Enumerable.Range(0, Runs).Select(i => first + i).ToList();
        output.WriteLine($"seeds {seeds[0]}..{seeds[^1]}, {FlipMs} ms each (OPAMP_PROP_RUNS, OPAMP_CONCURRENCY_MS, OPAMP_CONCURRENCY_SEED)");
        var failures = new List<string>();
        foreach (var seed in seeds)
        {
            var run = new Run(seed);
            var problems = run.Go(FlipMs);
            output.WriteLine(run.Summary);
            if (problems.Count > 0)
            {
                failures.Add($"seed {seed} ({run.Summary}): {problems.Count} problems, first: {string.Join("\n    ", problems.Take(5))}\n" +
                    $"  replay: OPAMP_CONCURRENCY_SEED={seed} dotnet test dotnet/Pydantic.OpAmp.AutoInstrumentation.Tests --filter FullyQualifiedName~{nameof(ConcurrencyTests)}");
            }
        }

        Assert.True(failures.Count == 0, string.Join("\n", failures));
    }

    // Controls.Apply put send_* in force before the policy the gates hold exports to, and a gate
    // read the two apart: between them, an export went out under the new document's send_* and the
    // old document's policy. From a document that stops the logs to one that drops "secret", a record
    // was sent with "secret" in it: neither document lets that through. The gates now read one
    // snapshot of both.
    [Fact]
    public void AnExportIsHeldToOneDocumentNeverToSendOfOneAndThePolicyOfAnother()
    {
        var controls = new Controls();
        var off = Vectors.Settings(Policy.Stop("traces", "logs"));
        var on = Vectors.Settings(Policy.Of(("attribute_redaction", """{"drop":["secret"]}""")));
        controls.Apply(off);
        var leaks = new Leaks();
        var logs = Logs(ScopeLogs("app", [Record(9, "r", Str("secret", "s"), Str("kept", "k"))]));
        var traces = Traces(Span("s", [Str("secret", "s"), Str("kept", "k")]));
        var stop = false;
        var applier = new Thread(() =>
        {
            while (!Volatile.Read(ref stop))
            {
                controls.Apply(on);
                controls.Apply(off);
            }
        });
        var senders = Enumerable.Range(0, 2).Select(i => new Thread(() =>
        {
            using var client = new HttpClient(new GateHandler(controls) { InnerHandler = leaks }, disposeHandler: false);
            while (!Volatile.Read(ref stop))
            {
                client.Send(new HttpRequestMessage(HttpMethod.Post, i == 0 ? "http://collector/v1/logs" : "http://collector/v1/traces")
                {
                    Content = new ByteArrayContent(i == 0 ? logs : traces) { Headers = { ContentType = new MediaTypeHeaderValue("application/x-protobuf") } },
                });
            }
        })).ToList();
        applier.Start();
        senders.ForEach(t => t.Start());
        var clock = Stopwatch.StartNew();
        while (clock.ElapsedMilliseconds < 500 && Volatile.Read(ref leaks.Leaked) == 0)
        {
            Thread.Yield();
        }

        Volatile.Write(ref stop, true);
        applier.Join();
        senders.ForEach(t => t.Join());
        Assert.Equal(0, leaks.Leaked);
        Assert.True(leaks.Sent > 0); // and under {"drop_attributes": "secret"}, the gate did send
    }

    /// <summary>A collector that counts the exports it gets, and those with a "secret" attribute in them.</summary>
    private sealed class Leaks : HttpMessageHandler
    {
        public int Leaked;
        public int Sent;

        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var message = request.Content!.ReadAsByteArrayAsync(cancellationToken).GetAwaiter().GetResult();
            var attributes = request.RequestUri!.AbsolutePath.EndsWith("/v1/logs", StringComparison.Ordinal)
                ? ReadLogs(message).SelectMany(r => r.Attributes)
                : ReadSpans(message).SelectMany(s => s.Attributes);
            Interlocked.Increment(ref this.Sent);
            if (attributes.Any(a => a.Key == "secret"))
            {
                Interlocked.Increment(ref this.Leaked);
            }

            return new HttpResponseMessage(HttpStatusCode.OK) { Content = new ByteArrayContent([]) };
        }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            Task.FromResult(this.Send(request, cancellationToken));
    }

    /// <summary>A root span to start, its event's and link's, and its children (each maybe with a child of its own).</summary>
    private sealed record SpanPlan(string Op, ActivityKind Kind, ParentOf Parent, Shapes Shapes, List<(Shapes Shapes, bool Grandchild)> Children);

    /// <summary>A span's attributes, its event's and its link's, by their shapes' indexes: in its name, so the collector knows what it started with.</summary>
    private readonly record struct Shapes(int Span, int Event, int Link)
    {
        public string Name(string op) => $"{op} s{this.Span} e{this.Event} l{this.Link}";

        public static Shapes Of(string name)
        {
            var parts = name.Split(' ');
            int At(int i) => int.Parse(parts[^i][1..], CultureInfo.InvariantCulture);
            return new Shapes(At(3), At(2), At(1));
        }
    }

    private sealed record LogPlan(string Category, LogLevel Level, int Shape);

    /// <summary>A policy and what the layer makes of it, with what it should do to telemetry, worked out here on its own.</summary>
    private sealed class Doc(string json, Settings settings)
    {
        public string Json { get; } = json;

        public Settings Settings { get; } = settings;

        public bool SendTraces => !this.Settings.HasSendTraces || this.Settings.SendTraces;

        public bool SendLogs => !this.Settings.HasSendLogs || this.Settings.SendLogs;

        /// <summary>Whether the document drops an attribute with this key.</summary>
        public bool Drops(string key) => this.Settings.DropAttributes.Any(p => Like(p, key, ignoreCase: true));

        /// <summary>An attribute list as the document redacts it.</summary>
        public List<KeyValue> Redact(IEnumerable<(string Key, object Value)> attributes) =>
            [.. from a in attributes
                where !this.Settings.DropAttributes.Any(p => Like(p, a.Key, ignoreCase: true))
                select this.Settings.MaskAttributes.Any(p => Like(p, a.Key, ignoreCase: true)) ? Kv(a.Key, Redactor.Masked) : Kv(a.Key, a.Value)];

        /// <summary>
        /// Whether a record is past the levels: the exact entry for its scope, else the longest
        /// prefix that matches, else send_logs_level; none of them, every record. Off: none.
        /// </summary>
        public bool Keeps(string scope, uint severity)
        {
            var levels = this.Settings.SendLogsScopeLevels;
            var exact = levels.FirstOrDefault(l => !l.Pattern.EndsWith('*') && l.Pattern == scope);
            var prefix = levels.Where(l => l.Pattern.EndsWith('*') && scope.StartsWith(l.Pattern[..^1], StringComparison.Ordinal)).MaxBy(l => l.Pattern.Length);
            var threshold = exact?.MinSeverity ?? prefix?.MinSeverity ?? (this.Settings.HasSendLogsMinSeverity ? this.Settings.SendLogsMinSeverity : 0);
            return threshold == 0 || (threshold != 0xFFFF && (severity == 0 || severity >= threshold));
        }

        /// <summary>
        /// Whether a root span is sampled, before the trace cap, where every rate is 0 or 1: with a
        /// remote parent, as it was; else the first rule it matches, else sampling_rate, else the
        /// application's sampler (always on).
        /// </summary>
        public bool Samples(SpanPlan p, IReadOnlyList<(string Key, object Value)> tags)
        {
            if (p.Parent != ParentOf.None)
            {
                return p.Parent == ParentOf.RemoteSampled;
            }

            var s = this.Settings;
            var rule = s.SamplingRules.FirstOrDefault(r =>
                (!r.HasName || Like(r.Name, p.Shapes.Name(p.Op), ignoreCase: false)) &&
                (!r.HasKind || r.Kind == KindOf(p.Kind)) &&
                !r.HasPath &&
                r.Attributes.All(a => tags.FirstOrDefault(t => t.Key == a.Key).Value switch
                {
                    string v => Like(a.Value, v, ignoreCase: false),
                    long v => Like(a.Value, v.ToString(CultureInfo.InvariantCulture), ignoreCase: false),
                    bool v => Like(a.Value, v ? "true" : "false", ignoreCase: false),
                    _ => false,
                }));
            var rate = rule?.Rate ?? (s.HasSamplingRate ? s.SamplingRate : 1);
            Assert.True(rate is 0 or 1, $"the last document's rates are 0 or 1: {this.Json}");
            return rate == 1;
        }

        private static SdkSpanKind KindOf(ActivityKind kind) => kind switch
        {
            ActivityKind.Server => SdkSpanKind.Server,
            ActivityKind.Client => SdkSpanKind.Client,
            ActivityKind.Producer => SdkSpanKind.Producer,
            ActivityKind.Consumer => SdkSpanKind.Consumer,
            _ => SdkSpanKind.Internal,
        };
    }

    /// <summary>A glob, as the settings mean it (<c>*</c>: any run of characters), by a regular expression of its own; keys ignoring ASCII case (every key here is ASCII).</summary>
    private static bool Like(string pattern, string text, bool ignoreCase) =>
        Regex.IsMatch(text, "^" + string.Join(".*", pattern.Split('*').Select(Regex.Escape)) + "$",
            RegexOptions.Singleline | RegexOptions.CultureInvariant | (ignoreCase ? RegexOptions.IgnoreCase : RegexOptions.None));

    /// <summary>An attribute as OTLP carries it.</summary>
    private static KeyValue Kv(string key, object value) => value switch
    {
        string s => Str(key, s),
        long n => Int(key, n),
        bool b => new KeyValue { Key = key, Value = new AnyValue { BoolValue = b } },
        string[] a => new KeyValue { Key = key, Value = new AnyValue { ArrayValue = new ArrayValue { Values = { a.Select(e => new AnyValue { StringValue = e }) } } } },
        _ => throw new ArgumentException($"{key}: {value}"),
    };

    private static string Text(IEnumerable<KeyValue> attributes) => string.Join(", ", attributes.Select(a => $"{a.Key}={a.Value}"));

    private static uint Severity(LogLevel level) => level switch
    {
        LogLevel.Trace => 1,
        LogLevel.Debug => 5,
        LogLevel.Information => 9,
        LogLevel.Warning => 13,
        LogLevel.Error => 17,
        _ => 21,
    };

    /// <summary>An exporter's client: the gate, watched for anything it throws, in front of the collector.</summary>
    private sealed class Watch(ConcurrentQueue<string> problems) : DelegatingHandler
    {
        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            try
            {
                return base.Send(request, cancellationToken);
            }
            catch (Exception e)
            {
                problems.Enqueue($"the gate threw: {e}");
                throw;
            }
        }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            try
            {
                return await base.SendAsync(request, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception e)
            {
                problems.Enqueue($"the gate threw: {e}");
                throw;
            }
        }
    }

    /// <summary>The collector: checks each export as it arrives (<see cref="Run.Traces"/>, <see cref="Run.Logs"/>), and answers OK.</summary>
    private sealed class Collector(Run run) : HttpMessageHandler
    {
        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var content = request.Content!;
            var grpc = content.Headers.ContentType?.MediaType == "application/grpc";
            try
            {
                var body = content.ReadAsByteArrayAsync(cancellationToken).GetAwaiter().GetResult();
                var message = grpc ? Ungrpc(body) : content.Headers.ContentEncoding.Contains("gzip") ? Gunzip(body) : body;
                var path = request.RequestUri!.AbsolutePath;
                if (path.EndsWith("/v1/traces", StringComparison.Ordinal) || path.Contains("TraceService", StringComparison.Ordinal))
                {
                    run.Traces(message);
                }
                else
                {
                    run.Logs(message);
                }
            }
            catch (Exception e)
            {
                run.Problem($"the collector couldn't read an export: {e}");
            }

            var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new ByteArrayContent([]) };
            if (grpc)
            {
                response.Headers.TryAddWithoutValidation("grpc-status", "0");
            }

            return response;
        }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            Task.FromResult(this.Send(request, cancellationToken));
    }

    /// <summary>One seeded run: its documents, its pipeline, its threads, and what they saw.</summary>
    private sealed class Run
    {
        // Attribute keys, each with its values: two spellings of always.drop (drop patterns ignore
        // ASCII case), integers, a boolean, an array (spans only: log records here carry scalars).
        private static readonly (string Key, Func<Random, object> Value)[] Keys =
        [
            ("always.drop", r => "d" + r.Next(100)),
            ("Always.Drop", r => "D" + r.Next(100)),
            ("always.mask", r => (long)r.Next(1000)),
            ("user.password", r => "p" + r.Next(100)),
            ("user.email", r => $"u{r.Next(100)}@example.com"),
            ("auth.token", r => "t" + r.Next(100)),
            ("card.number", r => 4111_0000L + r.Next(1000)),
            ("db.secret", r => "s" + r.Next(100)),
            ("tenant.id", r => new[] { "acme", "globex", "initech" }[r.Next(3)]),
            ("http.method", r => r.Next(2) == 0 ? "GET" : "POST"),
            ("retries", r => (long)r.Next(5)),
            ("ok", r => r.Next(2) == 0),
            ("tags", r => new[] { "a", "b" + r.Next(10) }),
        ];

        // Drop patterns that can't match always.mask, and those that can.
        private static readonly string[] SafeDrops = ["user.email", "auth.*", "*token*", "card.number", "DB.SECRET", "tenant.*", "always.drop", "ALWAYS.DROP", "always.d*"];
        private static readonly string[] AnyDrops = [.. SafeDrops, "*", "always.*", "*mask*", "*.*"];
        private static readonly string[] Masks = ["*", "user.*", "card.*", "*secret*", "http.*", "retries", "tags", "OK", "always.mask", "ALWAYS.MASK", "always.m*"];
        private static readonly string[] Ops = ["checkout.cart", "health", "db.query", "GET", "child"];
        private static readonly string[] Categories = ["app", "app.db", "app.dbx", "noisy", "lib.http", "other"];
        private static readonly string[] ScopePatterns = ["app", "app*", "app.db", "app.db*", "noisy", "*", "lib*"];
        private static readonly string[] Levels = ["trace", "debug", "info", "warn", "error", "fatal"];
        private static readonly double[] Caps = [0.5, 1, 2, 5, 1000];
        private static readonly ActivityKind[] Kinds = [ActivityKind.Internal, ActivityKind.Server, ActivityKind.Client, ActivityKind.Producer, ActivityKind.Consumer];

        private readonly long seed;
        private readonly Random rng;
        private readonly ConcurrentQueue<string> problems = new();
        private readonly List<(string Key, object Value)[]> shapes = [];
        private readonly List<Doc> docs = [];
        private readonly Doc last;
        private readonly Controls controls;
        private readonly object captureGate = new();
        private List<string>? capturedSpans;
        private List<string>? capturedLogs;
        private long clock;
        private int flips;
        private int spansExported;
        private int recordsExported;

        public Run(long seed)
        {
            this.seed = seed;
            this.rng = new Random(unchecked((int)seed ^ (int)(seed >> 32)));
            this.Kind = (Kind)(seed % 3);
            this.controls = new Controls(() => Interlocked.Read(ref this.clock));
            for (var i = 0; i < 12; i++)
            {
                this.shapes.Add(this.Shape());
            }

            for (var i = 0; i < 7; i++)
            {
                this.docs.Add(this.Document(last: false));
            }

            this.last = this.Document(last: true);
            this.docs.Add(this.last);
        }

        public Kind Kind { get; }

        public string Summary { get; private set; } = string.Empty;

        public void Problem(string problem) => this.problems.Enqueue(problem);

        /// <summary>Flip for <paramref name="flipMs"/>, then the last document: the problems found.</summary>
        public List<string> Go(int flipMs)
        {
            var started = Stopwatch.StartNew();
            var grpc = this.rng.Next(2) == 0;
            var gzip = this.rng.Next(2) == 0;
            OtlpExporterOptions Options(string path) => new()
            {
                Protocol = grpc ? OtlpExportProtocol.Grpc : OtlpExportProtocol.HttpProtobuf,
                Endpoint = new Uri(grpc ? "http://collector:4317" : "http://collector:4318" + path),
                Compression = gzip ? OtlpExportCompression.GZip : OtlpExportCompression.None,
                HttpClientFactory = () => new HttpClient(new Watch(this.problems) { InnerHandler = new GateHandler(this.controls) { InnerHandler = new Collector(this) } }),
            };

            this.controls.Apply(this.docs[0].Settings);
            this.controls.Sampler.Wrap(new ParentBasedSampler(new AlwaysOnSampler()));
            using var source = new ActivitySource($"concurrency-{this.seed}");
            var flipping = new Pipeline(this.controls.Sampler, source, Options);
            var factory = flipping.Factory;

            // The flipping: the applier through the documents, on a clock it moves on too; the
            // workers, each with its own seeded plan, emitting as fast as they can.
            var stop = 0;
            var threads = new List<Thread>
            {
                new(() =>
                {
                    var r = new Random(unchecked((int)this.seed * 7919));
                    this.Guard("the applier", () =>
                    {
                        while (Volatile.Read(ref stop) == 0)
                        {
                            this.controls.Apply(this.docs[r.Next(this.docs.Count)].Settings);
                            this.flips++;
                            Interlocked.Add(ref this.clock, r.Next(300) * 1_000_000L);
                            if (r.Next(16) == 0)
                            {
                                Thread.Yield();
                            }
                        }
                    });
                }),
            };
            for (var w = 0; w < Workers; w++)
            {
                var r = new Random(unchecked((int)this.seed * 31) + w);
                threads.Add(new Thread(() => this.Guard("a worker", () =>
                {
                    while (Volatile.Read(ref stop) == 0)
                    {
                        if (r.Next(2) == 0)
                        {
                            this.Emit(source, this.NewSpanPlan(r));
                        }
                        else
                        {
                            this.Emit(factory, this.NewLogPlan(r));
                        }
                    }
                })) { IsBackground = true });
            }

            threads[0].IsBackground = true;
            threads.ForEach(t => t.Start());
            Thread.Sleep(flipMs);
            Volatile.Write(ref stop, 1);
            foreach (var t in threads)
            {
                if (!t.Join(Bound > started.Elapsed ? Bound - started.Elapsed : TimeSpan.Zero))
                {
                    this.Problem($"a thread didn't stop within {Bound} (a deadlock?)");
                    return [.. this.problems];
                }
            }

            // Whatever was emitted under the flipping goes out now, before the last document.
            if (!flipping.Stop())
            {
                this.Problem("the batch processors didn't stop (a deadlock?)");
                return [.. this.problems];
            }

            this.Last(source, Options);
            var counts = this.controls.Tally.Counts();
            if (counts.Traces.ExportsFailed + counts.Logs.ExportsFailed > 0)
            {
                this.Problem($"exports failed at the gate: {counts}");
            }

            this.Summary = $"{this.Kind}, {(grpc ? "gRPC" : "http/protobuf")}{(gzip ? " gzip" : string.Empty)}: {this.flips} flips, " +
                $"{this.spansExported} spans and {this.recordsExported} log records exported, {started.ElapsedMilliseconds} ms";
            return [.. this.problems];
        }

        /// <summary>
        /// The last document, on a clock an hour on (every cap's bucket full again), then a plan of
        /// spans and records emitted one at a time: exported exactly as the document says.
        /// </summary>
        private void Last(ActivitySource source, Func<string, OtlpExporterOptions> options)
        {
            this.controls.Apply(this.last.Settings);
            Interlocked.Add(ref this.clock, 3_600_000_000_000L);
            lock (this.captureGate)
            {
                this.capturedSpans = [];
                this.capturedLogs = [];
            }

            var pipeline = new Pipeline(this.controls.Sampler, source, options);
            var factory = pipeline.Factory;

            var s = this.last.Settings;
            var r = new Random(unchecked((int)this.seed) ^ 0x5eed);
            var wantSpans = new List<string>();
            var traceTokens = s.HasSamplingTracesPerSecond ? Math.Max(s.SamplingTracesPerSecond, 1) : double.PositiveInfinity;
            for (var i = 0; i < 40; i++)
            {
                var plan = this.NewSpanPlan(r);
                var sampled = this.last.Samples(plan, this.shapes[plan.Shapes.Span]);
                if (sampled && traceTokens < 1)
                {
                    sampled = false; // capped
                }
                else if (sampled)
                {
                    traceTokens--;
                }

                var got = this.Emit(source, plan);
                if (got != sampled)
                {
                    this.Problem($"under the last document {this.last.Json}, {plan.Shapes.Name(plan.Op)} ({plan.Kind}, parent {plan.Parent}) was {(got ? string.Empty : "not ")}sampled");
                }

                if (sampled && this.last.SendTraces)
                {
                    wantSpans.Add(this.Expected(plan.Shapes.Name(plan.Op), plan.Shapes));
                    foreach (var (child, grandchild) in plan.Children)
                    {
                        wantSpans.Add(this.Expected(child.Name("child"), child));
                        if (grandchild)
                        {
                            wantSpans.Add(this.Expected(child.Name("grandchild"), child));
                        }
                    }
                }
            }

            var wantLogs = new List<string>();
            for (var i = 0; i < 60; i++)
            {
                var plan = this.NewLogPlan(r);
                this.Emit(factory, plan);
                if (this.last.SendLogs && this.last.Keeps(plan.Category, Severity(plan.Level)))
                {
                    wantLogs.Add($"{plan.Category} {Severity(plan.Level)} {Template(this.LogShape(plan.Shape))}: {Text(this.last.Redact(this.LogShape(plan.Shape)))}"
                        .Replace("{shape}", plan.Shape.ToString(CultureInfo.InvariantCulture), StringComparison.Ordinal));
                }
            }

            if (!pipeline.Stop())
            {
                this.Problem("the batch processors didn't stop (a deadlock?)");
            }

            List<string> gotSpans, gotLogs;
            lock (this.captureGate)
            {
                gotSpans = this.capturedSpans!;
                gotLogs = this.capturedLogs!;
                this.capturedSpans = this.capturedLogs = null;
            }

            // Spans go out as they end (children first), and an export groups its log records by
            // scope: both are compared as multisets.
            var (missing, extra) = Diff(wantSpans, gotSpans);
            if (missing.Count + extra.Count > 0)
            {
                this.Problem($"under the last document {this.last.Json}, spans missing: [{string.Join(" | ", missing.Take(3))}] ({missing.Count}), extra: [{string.Join(" | ", extra.Take(3))}] ({extra.Count})");
            }

            // The log cap lets through the first records of each export, in the export's order (by
            // scope), and the batches fall where they fall: with fewer tokens than records past the
            // levels, exactly that many of them go, whichever they are.
            var tokens = s.HasSendLogsPerSecond ? (int)Math.Floor(Math.Max(s.SendLogsPerSecond, 1)) : int.MaxValue;
            (missing, extra) = Diff(wantLogs, gotLogs);
            if (extra.Count > 0 || gotLogs.Count != Math.Min(tokens, wantLogs.Count))
            {
                this.Problem($"under the last document {this.last.Json}, {gotLogs.Count} log records of {wantLogs.Count} past the levels ({tokens} tokens), " +
                    $"missing: [{string.Join(" | ", missing.Take(3))}] ({missing.Count}), extra: [{string.Join(" | ", extra.Take(3))}] ({extra.Count})");
            }
        }

        /// <summary>What <paramref name="got"/> lacks of <paramref name="want"/>, and has besides, as multisets.</summary>
        private static (List<string> Missing, List<string> Extra) Diff(IEnumerable<string> want, IEnumerable<string> got)
        {
            var missing = want.ToList();
            var extra = got.Where(g => !missing.Remove(g)).ToList();
            return (missing, extra);
        }

        /// <summary>
        /// The SDK's pipeline: a tracer provider and a logger factory, each with a batch processor
        /// and an OTLP exporter whose client is the gate in front of the collector. Stopping it waits
        /// for the exporters' threads: a processor's ForceFlush can return while its last export is
        /// still on its way (a batch leaves the queue before it is sent), so only a shutdown says
        /// every export has arrived.
        /// </summary>
        private sealed class Pipeline
        {
            private readonly BatchActivityExportProcessor spans;
            private readonly BatchLogRecordExportProcessor logs;
            private readonly TracerProvider tracing;

            public Pipeline(Sampler sampler, ActivitySource source, Func<string, OtlpExporterOptions> options)
            {
                this.spans = new BatchActivityExportProcessor(new OtlpTraceExporter(options("/v1/traces")), maxQueueSize: 1024, scheduledDelayMilliseconds: 5, exporterTimeoutMilliseconds: 30_000, maxExportBatchSize: 64);
                this.logs = new BatchLogRecordExportProcessor(new OtlpLogExporter(options("/v1/logs")), maxQueueSize: 1024, scheduledDelayMilliseconds: 5, exporterTimeoutMilliseconds: 30_000, maxExportBatchSize: 64);
                this.tracing = Sdk.CreateTracerProviderBuilder().AddSource(source.Name).SetSampler(sampler).AddProcessor(this.spans).Build();
                this.Factory = LoggerFactory.Create(b => b.SetMinimumLevel(LogLevel.Trace).AddOpenTelemetry(o => o.AddProcessor(this.logs)));
            }

            public ILoggerFactory Factory { get; }

            /// <summary>Export what is left and stop: false if it didn't within the bound.</summary>
            public bool Stop()
            {
                var stopped = this.spans.Shutdown((int)Bound.TotalMilliseconds) & this.logs.Shutdown((int)Bound.TotalMilliseconds);
                this.tracing.Dispose();
                this.Factory.Dispose();
                return stopped;
            }
        }

        private string Expected(string name, Shapes shapes) =>
            $"{name}: {Text(this.last.Redact(this.shapes[shapes.Span]))}; event {Text(this.last.Redact(this.shapes[shapes.Event]))}; link {Text(this.last.Redact(this.shapes[shapes.Link]))}";

        /// <summary>A traces export: held to one document of the run, and to what every document has in common.</summary>
        public void Traces(byte[] message)
        {
            var exported = ReadSpans(message);
            Interlocked.Add(ref this.spansExported, exported.Count);
            var lists = new List<(List<KeyValue> Original, List<KeyValue> Got, string Where)>();
            foreach (var span in exported)
            {
                var shapes = Shapes.Of(span.Name);
                lists.Add((this.shapes[shapes.Span].Select(a => Kv(a.Key, a.Value)).ToList(), span.Attributes, span.Name));
                lists.AddRange(span.Events.Select(e => (this.shapes[shapes.Event].Select(a => Kv(a.Key, a.Value)).ToList(), e, span.Name + " (event)")));
                lists.AddRange(span.Links.Select(l => (this.shapes[shapes.Link].Select(a => Kv(a.Key, a.Value)).ToList(), l, span.Name + " (link)")));
                if (span.Events.Count != 1 || span.Links.Count != 1)
                {
                    this.Problem($"{span.Name}: {span.Events.Count} events and {span.Links.Count} links, not one of each");
                }
            }

            this.Common(lists, "traces");
            var one = this.docs.Where(d => d.SendTraces && exported.All(span =>
            {
                var shapes = Shapes.Of(span.Name);
                return d.Redact(this.shapes[shapes.Span]).SequenceEqual(span.Attributes) &&
                    span.Events.All(e => d.Redact(this.shapes[shapes.Event]).SequenceEqual(e)) &&
                    span.Links.All(l => d.Redact(this.shapes[shapes.Link]).SequenceEqual(l));
            }));
            if (!one.Any())
            {
                this.Problem($"a traces export no one document makes: {string.Join(" | ", exported.Take(3).Select(s => $"{s.Name}: {Text(s.Attributes)}"))}");
            }

            this.Capture(() => this.capturedSpans!.AddRange(exported.Select(s =>
                $"{s.Name}: {Text(s.Attributes)}; event {Text(s.Events.SingleOrDefault() ?? [])}; link {Text(s.Links.SingleOrDefault() ?? [])}")));
        }

        /// <summary>A logs export: held to one document of the run, and to what every document has in common.</summary>
        public void Logs(byte[] message)
        {
            var records = ReadLogs(message);
            Interlocked.Add(ref this.recordsExported, records.Count);
            if (this.Kind == Kind.LogsOff)
            {
                this.Problem($"a logs export with send_logs false in every document: {records.Count} records");
            }

            int ShapeOf(string body) => int.Parse(body.Split(' ')[0][1..], CultureInfo.InvariantCulture);
            this.Common([.. records.Select(r => (this.LogShape(ShapeOf(r.Body)).Select(a => Kv(a.Key, a.Value)).ToList(), r.Attributes, r.Body))], "logs");
            var one = this.docs.Where(d => d.SendLogs && records.All(r =>
                d.Keeps(r.Scope, r.Severity) && d.Redact(this.LogShape(ShapeOf(r.Body))).SequenceEqual(r.Attributes)));
            if (!one.Any())
            {
                this.Problem($"a logs export no one document makes: {string.Join(" | ", records.Take(3).Select(r => $"{r.Scope} {r.Severity} {r.Body}: {Text(r.Attributes)}"))}");
            }

            this.Capture(() => this.capturedLogs!.AddRange(records.Select(r => $"{r.Scope} {r.Severity} {r.Body}: {Text(r.Attributes)}")));
        }

        /// <summary>
        /// What every document that sends the signal has in common: a key each of them drops never
        /// goes; and in a Redacted run, always.drop never goes, always.mask only masked.
        /// </summary>
        private void Common(List<(List<KeyValue> Original, List<KeyValue> Got, string Where)> lists, string signal)
        {
            var senders = this.docs.Where(d => signal == "traces" ? d.SendTraces : d.SendLogs).ToList();
            foreach (var (_, got, where) in lists)
            {
                foreach (var a in got.Where(a => senders.All(d => d.Drops(a.Key))))
                {
                    this.Problem($"{signal}: {where} exported {a.Key}, dropped by every document that sends {signal}: {Text(got)}");
                }
            }

            if (this.Kind != Kind.Redacted)
            {
                return;
            }

            foreach (var (_, got, where) in lists)
            {
                if (got.Any(a => a.Key.Equals("always.drop", StringComparison.OrdinalIgnoreCase)))
                {
                    this.Problem($"{signal}: {where} exported always.drop, dropped in every document: {Text(got)}");
                }

                if (got.Any(a => a.Key == "always.mask" && a.Value.StringValue != Redactor.Masked))
                {
                    this.Problem($"{signal}: {where} exported always.mask unmasked, masked in every document: {Text(got)}");
                }
            }
        }

        private void Capture(Action add)
        {
            lock (this.captureGate)
            {
                if (this.capturedSpans is not null)
                {
                    add();
                }
            }
        }

        private void Guard(string who, Action body)
        {
            try
            {
                body();
            }
            catch (Exception e)
            {
                this.Problem($"{who} threw: {e}");
            }
        }

        /// <summary>Start a plan's spans: whether its root was sampled. A child of a sampled span has to be sampled too.</summary>
        private bool Emit(ActivitySource source, SpanPlan plan)
        {
            var parent = plan.Parent == ParentOf.None ? default : new ActivityContext(
                ActivityTraceId.CreateRandom(), ActivitySpanId.CreateRandom(), plan.Parent == ParentOf.RemoteSampled ? ActivityTraceFlags.Recorded : ActivityTraceFlags.None, isRemote: true);
            using var root = this.Start(source, plan.Shapes.Name(plan.Op), plan.Kind, parent, plan.Shapes);
            if (root is null)
            {
                return false;
            }

            foreach (var (shapes, grandchild) in plan.Children)
            {
                using var child = this.Start(source, shapes.Name("child"), ActivityKind.Internal, root.Context, shapes);
                this.Followed(root, child);
                if (grandchild && child is not null)
                {
                    using var g = this.Start(source, shapes.Name("grandchild"), ActivityKind.Internal, child.Context, shapes);
                    this.Followed(child, g);
                }
            }

            return root.Recorded;
        }

        private void Followed(Activity parent, Activity? child)
        {
            if (parent.Recorded && child is not { Recorded: true })
            {
                this.Problem($"a child of the sampled local span {parent.DisplayName} wasn't sampled");
            }
        }

        private Activity? Start(ActivitySource source, string name, ActivityKind kind, ActivityContext parent, Shapes shapes)
        {
            var link = new ActivityLink(
                new ActivityContext(ActivityTraceId.CreateRandom(), ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded),
                [.. this.shapes[shapes.Link].Select(a => new KeyValuePair<string, object?>(a.Key, a.Value))]);
            var activity = source.StartActivity(name, kind, parent, [.. this.shapes[shapes.Span].Select(a => new KeyValuePair<string, object?>(a.Key, a.Value))], [link]);
            activity?.AddEvent(new ActivityEvent("event", tags: [.. this.shapes[shapes.Event].Select(a => new KeyValuePair<string, object?>(a.Key, a.Value))]));
            return activity;
        }

        private void Emit(ILoggerFactory factory, LogPlan plan)
        {
            var attributes = this.LogShape(plan.Shape);
#pragma warning disable CA2254 // the template is the record's identity: its shape's
            factory.CreateLogger(plan.Category).Log(plan.Level, Template(attributes).Replace("{shape}", plan.Shape.ToString(CultureInfo.InvariantCulture), StringComparison.Ordinal),
                [.. attributes.Select(a => a.Value)]);
#pragma warning restore CA2254
        }

        /// <summary>A record's template: its shape's index (the collector reads it back from the body), then a hole for each attribute.</summary>
        private static string Template(IEnumerable<(string Key, object Value)> attributes) =>
            "r{shape} " + string.Join(" ", attributes.Select(a => "{" + a.Key + "}"));

        private (string Key, object Value)[] LogShape(int shape) => [.. this.shapes[shape].Where(a => a.Value is not string[])];

        private SpanPlan NewSpanPlan(Random r) => new(
            Ops[r.Next(Ops.Length)],
            Kinds[r.Next(Kinds.Length)],
            r.Next(6) switch { 0 => ParentOf.RemoteSampled, 1 => ParentOf.RemoteUnsampled, _ => ParentOf.None },
            this.NewShapes(r),
            [.. Enumerable.Range(0, r.Next(4)).Select(_ => (this.NewShapes(r), r.Next(3) == 0))]);

        private Shapes NewShapes(Random r) => new(r.Next(this.shapes.Count), r.Next(this.shapes.Count), r.Next(this.shapes.Count));

        private LogPlan NewLogPlan(Random r) => new(Categories[r.Next(Categories.Length)], (LogLevel)r.Next(6), r.Next(this.shapes.Count));

        /// <summary>An attribute list: a few distinct keys, always.drop and always.mask often among them.</summary>
        private (string Key, object Value)[] Shape()
        {
            var keys = Keys.OrderBy(_ => this.rng.Next()).Take(this.rng.Next(1, 7)).ToList();
            foreach (var always in Keys.Take(4))
            {
                if (this.rng.Next(3) == 0 && !keys.Contains(always))
                {
                    keys.Insert(this.rng.Next(keys.Count + 1), always);
                }
            }

            return [.. keys.Select(k => (k.Key, k.Value(this.rng)))];
        }

        /// <summary>A policy of the run's kind; the last one's rates 0 or 1 (so whether a root is sampled is known).</summary>
        private Doc Document(bool last)
        {
            // Each control the policy overrides, with the fields of its override; the rest are local.
            var d = new Dictionary<string, JsonObject>();
            JsonObject Override(string control) => d.TryGetValue(control, out var o) ? o : d[control] = [];
            var r = this.rng;
            T Pick<T>(T[] from) => from[r.Next(from.Length)];
            double Rate() => last ? r.Next(2) : Pick(new[] { 0, 0.25, 0.5, 1 });
            List<string> Some(string[] from, int most) => [.. from.OrderBy(_ => r.Next()).Take(r.Next(most + 1))];

            if (this.Kind == Kind.Anything && r.Next(6) == 0)
            {
                return new Doc(Policy.Local, Vectors.Settings(Policy.Local)); // every control local
            }

            if (r.Next(2) == 0)
            {
                Override("head_sampling")["ratio"] = Rate();
            }

            if (r.Next(2) == 0)
            {
                // Rules that overlap (a name, a kind, an attribute, or nothing: every span), in any order.
                var rules = new JsonArray();
                for (var i = r.Next(1, 5); i > 0; i--)
                {
                    var rule = new JsonObject { ["rate"] = Rate() };
                    if (r.Next(2) == 0)
                    {
                        rule["name"] = Pick(new[] { "checkout*", "health", "*", "GET", "db.*", "* s1 *", "*e2*" });
                    }

                    if (r.Next(3) == 0)
                    {
                        rule["kind"] = Pick(new[] { "server", "client", "internal", "producer", "consumer" });
                    }

                    if (r.Next(3) == 0)
                    {
                        rule["attributes"] = new JsonObject { ["tenant.id"] = Pick(new[] { "acme", "glo*", "*" }) };
                    }

                    rules.Add(rule);
                }

                Override("sampling_rules")["rules"] = rules;
            }

            // A quiet policy sends neither traces nor logs, and keeps user.password; every other
            // one drops it. (Not the last: what comes out after the flipping is checked against it.)
            var quiet = !last && r.Next(4) == 0;
            foreach (var signal in new[] { "traces", "metrics", "logs" })
            {
                if ((signal == "logs" && this.Kind == Kind.LogsOff) || (quiet && signal != "metrics"))
                {
                    Override($"signal_exports.{signal}")["enabled"] = false;
                }
                else if (r.Next(3) == 0)
                {
                    Override($"signal_exports.{signal}")["enabled"] = r.Next(3) != 0;
                }
            }

            if (r.Next(3) == 0)
            {
                Override("log_levels")["level"] = Pick(Levels);
            }

            if (r.Next(3) == 0)
            {
                var scopes = Some(ScopePatterns, 3);
                if (scopes.Count > 0)
                {
                    Override("log_levels")["scopes"] = new JsonObject(scopes.Select(p =>
                        KeyValuePair.Create(p, (JsonNode?)Pick(Levels.Append("off").ToArray()))));
                }
            }

            var drops = Some(this.Kind == Kind.Anything ? AnyDrops : SafeDrops, 3);
            var masks = Some(Masks, 3);
            if (this.Kind == Kind.Redacted)
            {
                drops.Add(Pick(new[] { "always.drop", "ALWAYS.DROP", "always.d*" }));
                masks.Add(Pick(new[] { "always.mask", "ALWAYS.MASK", "always.m*" }));
            }

            if (!quiet)
            {
                drops.Add("*password*");
            }

            if (drops.Count > 0)
            {
                Override("attribute_redaction")["drop"] = new JsonArray([.. drops.Distinct().Select(x => (JsonNode)x)]);
            }

            if (masks.Count > 0)
            {
                Override("attribute_redaction")["mask"] = new JsonArray([.. masks.Distinct().Select(x => (JsonNode)x)]);
            }

            if (r.Next(3) == 0)
            {
                Override("trace_rate_limit")["traces_per_second"] = Pick(Caps);
            }

            if (r.Next(3) == 0)
            {
                Override("log_rate_limit")["records_per_second"] = Pick(Caps);
            }

            switch (r.Next(4))
            {
                case 0: // a debug window, over
                    Override("debug")["until"] = "2026-10-14T11:00:00Z";
                    break;
                case 1: // a debug window, open at the vectors' clock
                    Override("debug")["until"] = "2026-10-14T12:30:00Z";
                    if (r.Next(2) == 0)
                    {
                        Override("debug")["sampling_ratio"] = Rate();
                    }

                    if (r.Next(2) == 0)
                    {
                        Override("debug")["log_level"] = Pick(Levels);
                    }

                    break;
            }

            var json = Policy.Of([.. d.Select(kv => (kv.Key, kv.Value.ToJsonString()))]);
            return new Doc(json, Vectors.Settings(json));
        }
    }
}
