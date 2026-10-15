// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net;
using System.Net.Http.Headers;
using System.Text.Json;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Xunit;
using static Pydantic.OpAmp.AutoInstrumentation.Tests.Otlp;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>Export health: what the gates count of each attempt, and the agent's timer asking the layer and sending it.</summary>
public class HealthTests
{
    private const long Second = 1_000_000_000;
    private readonly List<(Action Fire, Scheduled Handle)> timers = [];
    private ulong wall = Vectors.Now;
    private long nanos;

    private Clock Clock => new(() => this.wall, () => this.nanos, (_, fire) =>
    {
        var handle = new Scheduled();
        this.timers.Add((fire, handle));
        return handle;
    });

    /// <summary>Fire the timer armed last, as it would go off.</summary>
    private void Fire()
    {
        var (fire, handle) = this.timers[^1];
        Assert.False(handle.Disposed);
        fire();
    }

    private Agent Prepared(string stateDir = "none") => Agent.Prepare(
        n => n switch { "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp", "OTEL_OPAMP_STATE_DIR" => stateDir, _ => null },
        (_, _) => { },
        this.Clock)!;

    private Agent Started()
    {
        var agent = this.Prepared();
        agent.Start([new("service.name", "health-test")]);
        return agent;
    }

    private static HttpResponseMessage Post(Controls controls, Sink sink, string path, byte[] body, bool grpc = false, Signal? signal = null)
    {
        using var client = new HttpClient(new GateHandler(controls, signal) { InnerHandler = sink });
        var content = new ByteArrayContent(grpc ? Grpc(body) : body);
        content.Headers.ContentType = new MediaTypeHeaderValue(grpc ? "application/grpc" : "application/x-protobuf");
        var request = new HttpRequestMessage(HttpMethod.Post, "http://collector" + path) { Content = content };
        return grpc ? client.SendAsync(request).GetAwaiter().GetResult() : client.Send(request);
    }

    private static readonly byte[] ThreeSpans = Traces(Span("a", []), Span("b", []), Span("c", []));

    [Fact]
    public void EachAttemptIsCountedByItsStatus()
    {
        var controls = new Controls();
        var sink = new Sink();
        Post(controls, sink, "/v1/traces", ThreeSpans, signal: Signal.Traces);
        sink.Answer = _ => new HttpResponseMessage(HttpStatusCode.ServiceUnavailable) { Content = new ByteArrayContent([]) };
        Post(controls, sink, "/v1/traces", ThreeSpans, signal: Signal.Traces); // the exporter's retry: a second attempt
        Post(controls, sink, "/v1/traces", ThreeSpans, signal: Signal.Traces);
        var t = controls.Tally.Counts().Traces;
        Assert.Equal(1UL, t.ExportsOk);
        Assert.Equal(2UL, t.ExportsFailed);
        Assert.Equal(6UL, t.ItemsFailed);
        Assert.True(t.LastFailed);
        Assert.Equal("HTTP 503 Service Unavailable", t.LastError);

        sink.Answer = null;
        Post(controls, sink, "/v1/traces", ThreeSpans, signal: Signal.Traces);
        Assert.False(controls.Tally.Counts().Traces.LastFailed);
    }

    [Fact]
    public void GrpcsStatusIsReadFromTheTrailers()
    {
        var controls = new Controls();
        var sink = new Sink
        {
            Answer = _ =>
            {
                var r = new HttpResponseMessage(HttpStatusCode.OK) { Content = new ByteArrayContent([]) };
                r.TrailingHeaders.TryAddWithoutValidation("grpc-status", "14");
                r.TrailingHeaders.TryAddWithoutValidation("grpc-message", "upstream%20gone");
                return r;
            },
        };
        var path = "/opentelemetry.proto.collector.logs.v1.LogsService/Export";
        var logs = Logs(ScopeLogs("s", [Record(9, "1"), Record(9, "2")]));
        Post(controls, sink, path, logs, grpc: true); // the hosting gate: the signal by the path
        var l = controls.Tally.Counts().Logs;
        Assert.Equal(1UL, l.ExportsFailed);
        Assert.Equal(2UL, l.ItemsFailed);
        Assert.Equal("gRPC status 14: upstream gone", l.LastError);

        sink.Answer = null; // grpc-status 0 in the headers
        Post(controls, sink, path, logs, grpc: true);
        Assert.Equal(1UL, controls.Tally.Counts().Logs.ExportsOk);
        Assert.False(controls.Tally.Counts().Logs.LastFailed);
    }

    [Fact]
    public void AnExceptionIsAFailedExportAndStillThrown()
    {
        var controls = new Controls();
        var sink = new Sink { Answer = _ => throw new HttpRequestException("Connection refused (collector:4318)") };
        Assert.Throws<HttpRequestException>(() => Post(controls, sink, "/v1/metrics", Traces(), signal: Signal.Metrics));
        var m = controls.Tally.Counts().Metrics;
        Assert.Equal(1UL, m.ExportsFailed);
        Assert.Equal("Connection refused (collector:4318)", m.LastError);
    }

    [Fact]
    public void WhatTheLevelsTheCapsAndRedactionsFailClosedDropIsCounted()
    {
        var now = 0L;
        var controls = new Controls(() => now);
        var sink = new Sink();
        controls.Apply(Vectors.Settings("{\"send_logs_level\":\"warn\",\"send_logs_per_second\":\"1\",\"sampling_traces_per_second\":\"1\"}"));
        Post(controls, sink, "/v1/logs", Logs(ScopeLogs("s", [Record(9, "info"), Record(13, "w1"), Record(13, "w2")])), signal: Signal.Logs);
        var l = controls.Tally.Counts().Logs;
        Assert.Equal(2UL, l.ItemsFiltered); // the info record by the level, one warning by the cap
        Assert.Equal(1UL, l.ExportsOk);
        Post(controls, sink, "/v1/logs", Logs(ScopeLogs("s", [Record(13, "w3")])), signal: Signal.Logs); // all capped: not sent
        Assert.Equal(3UL, controls.Tally.Counts().Logs.ItemsFiltered);
        Assert.Equal(1UL, controls.Tally.Counts().Logs.ExportsOk);

        // The trace cap's refusals are filtered spans.
        var root = new OpenTelemetry.Trace.SamplingParameters(default, System.Diagnostics.ActivityTraceId.CreateRandom(), "r", System.Diagnostics.ActivityKind.Server);
        controls.Sampler.ShouldSample(root);
        controls.Sampler.ShouldSample(root);
        Assert.Equal(1UL, controls.Tally.Counts().Traces.ItemsFiltered);

        // Under redaction an export the gate can't read isn't sent: a failure, not a filter.
        controls.Apply(Vectors.Settings("{\"drop_attributes\":\"secret\"}"));
        Post(controls, sink, "/v1/traces", [0xff, 0xff, 0xff], signal: Signal.Traces);
        Assert.Equal(1UL, controls.Tally.Counts().Traces.ExportsFailed);
        Assert.False(controls.Tally.Counts().Traces.LastError.Length == 0);
    }

    [Fact]
    public void ASignalTurnedOffIsntCountedTheLayerReportsItOff()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings("{\"send_traces\":\"false\"}"));
        Post(controls, new Sink(), "/v1/traces", ThreeSpans, signal: Signal.Traces);
        var t = controls.Tally.Counts().Traces;
        Assert.Equal(0UL, t.ExportsOk + t.ExportsFailed + t.ItemsFiltered);
    }

    // Every case of testdata/health.json through the agent: its counts reached through the tally's
    // own calls, its settings as a remote config; the first step the agent's start (export health
    // goes with the first message), each next the timer going off 30 s after the last.
    [Fact]
    public void EveryHealthVectorThroughTheAgentsTimer()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("health.json"))
        {
            var name = c.GetProperty("name").GetString()!;
            this.timers.Clear();
            this.nanos = 0;
            var steps = c.GetProperty("steps").EnumerateArray().ToList();
            this.wall = steps[0].GetProperty("now_unix_ms").GetUInt64();
            // A state directory: the first step's settings, decided before the start, are what it loads.
            var agent = this.Prepared(Path.Combine(Path.GetTempPath(), "opamp-health-" + Guid.NewGuid()));
            try
            {
                var had = new Dictionary<Signal, SignalCounts>();
                for (var i = 0; i < steps.Count; i++)
                {
                    var step = steps[i];
                    var where = $"{name}: step {i}";
                    this.wall = step.GetProperty("now_unix_ms").GetUInt64();
                    if (i > 0)
                    {
                        this.nanos += 30 * Second;
                    }

                    var settings = step.TryGetProperty("settings", out var doc) ? doc.GetRawText() : "{}";
                    Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Vectors.Config(settings)).Status);
                    foreach (var signal in Enum.GetValues<Signal>())
                    {
                        var want = step.TryGetProperty("counts", out var counts) && counts.TryGetProperty(signal.ToString().ToLowerInvariant(), out var sc)
                            ? Counts(sc) : new SignalCounts();
                        Reach(agent.Controls.Tally, signal, had.GetValueOrDefault(signal) ?? new SignalCounts(), want);
                        had[signal] = want;
                    }

                    var before = agent.Sdk.HealthSent;
                    if (i == 0)
                    {
                        agent.Start([new("service.name", "health-test")]);
                    }
                    else
                    {
                        this.Fire();
                    }

                    var report = agent.Sdk.LastHealth!;
                    Check(step.GetProperty("want"), report, where, failed);
                    var sent = !ReferenceEquals(before, agent.Sdk.HealthSent) &&
                               agent.Sdk.HealthSent!.ComponentHealthMap.ContainsKey("exporters");
                    if (sent != report.Send)
                    {
                        failed.Add($"{where}: sent {sent}, the report said {report.Send}");
                    }

                    if (report.Send && (agent.Sdk.HealthSent!.ComponentHealthMap["exporters"] != report.Exporters || !agent.Sdk.HealthSent.Healthy))
                    {
                        failed.Add($"{where}: the health sent isn't the application's with the report's exporters entry");
                    }
                }
            }
            finally
            {
                agent.Stop(TimeSpan.FromSeconds(1));
            }
        }

        Assert.Empty(failed);
    }

    [Fact]
    public void TheApplicationsHealthAndExportHealthGoTogether()
    {
        var agent = this.Started();
        try
        {
            // Export health went with the first message, and the timer is armed for 30 s on.
            Assert.True(agent.Sdk.HealthSent!.ComponentHealthMap.ContainsKey("exporters"));
            Assert.False(this.timers[^1].Handle.Disposed);
            agent.Sdk.SetHealth(new ComponentHealth { Healthy = false, Status = "degraded" });
            Assert.False(agent.Sdk.HealthSent.Healthy);
            Assert.Equal("degraded", agent.Sdk.HealthSent.Status);
            Assert.Equal("ok", agent.Sdk.HealthSent.ComponentHealthMap["exporters"].Status);

            // A failing exporter makes the exporters entry unhealthy, never the top level.
            agent.Sdk.SetHealth(new ComponentHealth { Healthy = true });
            agent.Controls.Tally.Failed(Signal.Traces, 5, "connection refused");
            this.nanos += 30 * Second;
            this.Fire();
            Assert.True(agent.Sdk.HealthSent.Healthy);
            Assert.False(agent.Sdk.HealthSent.ComponentHealthMap["exporters"].Healthy);
            Assert.Equal("traces: connection refused", agent.Sdk.HealthSent.ComponentHealthMap["exporters"].LastError);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    private static SignalCounts Counts(JsonElement e) => new()
    {
        ExportsOk = e.TryGetProperty("exports_ok", out var a) ? a.GetUInt64() : 0,
        ExportsFailed = e.TryGetProperty("exports_failed", out var b) ? b.GetUInt64() : 0,
        ItemsFailed = e.TryGetProperty("items_failed", out var d) ? d.GetUInt64() : 0,
        ItemsFiltered = e.TryGetProperty("items_filtered", out var f) ? f.GetUInt64() : 0,
        LastFailed = e.TryGetProperty("last_failed", out var g) && g.GetBoolean(),
        LastError = e.TryGetProperty("last_error", out var h) ? h.GetString() : string.Empty,
    };

    /// <summary>The tally from <paramref name="had"/> to <paramref name="want"/>, by its own calls: the last one decides last_failed.</summary>
    private static void Reach(ExportTally tally, Signal signal, SignalCounts had, SignalCounts want)
    {
        var oks = (long)(want.ExportsOk - had.ExportsOk);
        var fails = (long)(want.ExportsFailed - had.ExportsFailed);
        var items = (long)(want.ItemsFailed - had.ItemsFailed);
        tally.Filtered(signal, (long)(want.ItemsFiltered - had.ItemsFiltered));
        void Oks()
        {
            for (var i = 0; i < oks; i++)
            {
                tally.Ok(signal);
            }
        }

        void Fails()
        {
            for (var i = 0; i < fails; i++)
            {
                tally.Failed(signal, i == fails - 1 ? items : 0, want.LastError);
            }
        }

        if (want.LastFailed)
        {
            Oks();
            Fails();
        }
        else
        {
            Fails();
            Oks();
        }

        var got = tally.Counts();
        var counts = signal switch { Signal.Traces => got.Traces, Signal.Metrics => got.Metrics, _ => got.Logs };
        Assert.Equal(want.ExportsOk, counts.ExportsOk);
        Assert.Equal(want.ExportsFailed, counts.ExportsFailed);
        Assert.Equal(want.LastFailed, counts.LastFailed);
        if (want.LastFailed)
        {
            Assert.Equal(want.LastError, counts.LastError);
        }
    }

    private static void Check(JsonElement want, HealthReport report, string where, List<string> failed)
    {
        if (want.TryGetProperty("send", out var send) && send.GetBoolean() != report.Send)
        {
            failed.Add($"{where}: send {report.Send}");
        }

        if (want.TryGetProperty("sent_unix_ms", out var at) && at.GetUInt64() != report.SentUnixMs)
        {
            failed.Add($"{where}: sent at {report.SentUnixMs}");
        }

        Entry(want, report.Exporters, where, failed);
        if (want.TryGetProperty("signals", out var signals))
        {
            foreach (var s in signals.EnumerateObject())
            {
                if (!report.Exporters.ComponentHealthMap.TryGetValue(s.Name, out var child))
                {
                    failed.Add($"{where}: no {s.Name}");
                    continue;
                }

                Entry(s.Value, child, $"{where} {s.Name}", failed);
                if (s.Value.TryGetProperty("counters", out var counters))
                {
                    foreach (var counter in counters.EnumerateObject())
                    {
                        var got = child.Attributes.FirstOrDefault(a => a.Key == counter.Name)?.Value?.IntValue;
                        if (got != counter.Value.GetInt64())
                        {
                            failed.Add($"{where} {s.Name}: {counter.Name} {got}");
                        }
                    }
                }
            }
        }
    }

    private static void Entry(JsonElement want, ComponentHealth got, string where, List<string> failed)
    {
        if (want.TryGetProperty("healthy", out var h) && h.GetBoolean() != got.Healthy)
        {
            failed.Add($"{where}: healthy {got.Healthy}");
        }

        if (want.TryGetProperty("status", out var s) && s.GetString() != got.Status)
        {
            failed.Add($"{where}: status {got.Status}");
        }

        if (want.TryGetProperty("last_error", out var e) && e.GetString() != got.LastError)
        {
            failed.Add($"{where}: last_error {got.LastError}");
        }

        if (want.TryGetProperty("start_time_unix_nano", out var st) && st.GetUInt64() != got.StartTimeUnixNano)
        {
            failed.Add($"{where}: start_time_unix_nano {got.StartTimeUnixNano}");
        }

        if (want.TryGetProperty("status_time_unix_nano", out var t) && t.GetUInt64() != got.StatusTimeUnixNano)
        {
            failed.Add($"{where}: status_time_unix_nano {got.StatusTimeUnixNano}");
        }
    }

    private sealed class Scheduled : IDisposable
    {
        public bool Disposed { get; private set; }

        public void Dispose() => this.Disposed = true;
    }
}
