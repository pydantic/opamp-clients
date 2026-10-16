// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Text.Json;
using System.Text.Json.Nodes;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>Debug mode: the layer's Load, Decide and Refresh through the agent, on its clocks, and the one timer that ends it.</summary>
public class DebugTests
{
    private const ulong Noon = Vectors.Now; // 2026-10-14T12:00:00Z
    private const ulong Minute = 60_000;

    private readonly List<(TimeSpan Delay, Action Fire, Scheduled Handle)> timers = [];
    private ulong wall = Noon;
    private long nanos;

    private Clock Clock => new(() => this.wall, () => this.nanos, (delay, fire) =>
    {
        var handle = new Scheduled();
        this.timers.Add((delay, fire, handle));
        return handle;
    });

    /// <summary>The timer armed last, if it is still armed.</summary>
    private (TimeSpan Delay, Action Fire)? Armed =>
        this.timers.Count > 0 && !this.timers[^1].Handle.Disposed ? (this.timers[^1].Delay, this.timers[^1].Fire) : null;

    private Agent Start(string? stateDir)
    {
        var agent = Agent.Prepare(
            n => n switch
            {
                "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp",
                "OTEL_OPAMP_STATE_DIR" => stateDir ?? "none",
                _ => null,
            },
            (_, _) => { },
            this.Clock)!;
        Assert.NotNull(agent);
        return agent;
    }

    private static string Effective(EffectiveConfig e) =>
        e.ConfigMap?.ConfigMap.TryGetValue("elastic", out var c) == true ? c.Body.ToStringUtf8() : "{}";

    // What the effective config names as set aside (its elastic.set_aside entry): "null" without one.
    private static string SetAside(EffectiveConfig e) =>
        e.ConfigMap?.ConfigMap.TryGetValue("elastic.set_aside", out var c) == true ? c.Body.ToStringUtf8() : "null";

    // Every case of testdata/debug.json and saving.json, each step through the agent the plugin
    // runs: a decide is a remote config (kept applied as the client keeps it, where there is a state
    // directory), a load a restart (a new agent on the same state directory), a refresh the timer's
    // call, each at the step's time on the agent's clock. saving.json's cases whose save fails are
    // the layer's: here the client keeps the config applied, and its save failing holds APPLIED back
    // rather than refusing the config (the client's tests).
    [Fact]
    public void EveryDebugAndSavingVectorThroughTheAgent()
    {
        var failed = new List<string>();
        var cases = Vectors.Cases("debug.json").Concat(Vectors.Cases("saving.json").Where(c =>
            !c.GetProperty("steps").EnumerateArray().Any(s => s.TryGetProperty("save", out var v) && v.GetString() == "failed")));
        foreach (var c in cases)
        {
            var name = c.GetProperty("name").GetString()!;
            var steps = c.GetProperty("steps").EnumerateArray().ToList();
            var canSave = steps.All(s => !s.TryGetProperty("save", out var v) || v.GetString() != "nowhere");
            var dir = canSave ? Path.Combine(Path.GetTempPath(), "opamp-debug-" + Guid.NewGuid()) : null;
            this.wall = steps[0].GetProperty("now_unix_ms").GetUInt64();
            var agent = this.Start(dir);
            try
            {
                for (var i = 0; i < steps.Count; i++)
                {
                    var step = steps[i];
                    this.wall = step.GetProperty("now_unix_ms").GetUInt64();
                    RemoteConfigStatus? status = null;
                    switch (step.GetProperty("call").GetString())
                    {
                        case "decide":
                            var document = step.GetProperty("document").GetString()!;
                            status = agent.RemoteConfig(Vectors.Config(document));
                            if (status.Status == RemoteConfigStatuses.Applied && dir != null)
                            {
                                Kept.KeepApplied(dir, document); // as the client does, before APPLIED goes out
                            }

                            break;
                        case "load":
                            agent.Stop(TimeSpan.FromSeconds(1));
                            agent = this.Start(dir);
                            break;
                        default:
                            agent.Sdk.Refresh(this.wall);
                            break;
                    }

                    var want = step.GetProperty("want");
                    var where = $"{name}: step {i}";
                    if (want.TryGetProperty("status", out var s) &&
                        status?.Status != (s.GetString() == "applied" ? RemoteConfigStatuses.Applied : RemoteConfigStatuses.Failed))
                    {
                        failed.Add($"{where}: status {status?.Status}");
                    }

                    if (want.TryGetProperty("error", out var error) &&
                        status?.ErrorMessage.StartsWith(error.GetString()!, StringComparison.Ordinal) != true)
                    {
                        failed.Add($"{where}: error {status?.ErrorMessage}");
                    }

                    if (want.TryGetProperty("sampling_rate", out var rate))
                    {
                        var inForce = agent.Sdk.InForce;
                        double? got = inForce.HasSamplingRate ? inForce.SamplingRate : null;
                        double? expected = rate.ValueKind == JsonValueKind.Null ? null : rate.GetDouble();
                        if (got != expected || agent.Controls.Sampler.Policy.Rate != expected)
                        {
                            failed.Add($"{where}: sampling_rate {got}, the sampler's {agent.Controls.Sampler.Policy.Rate}, not {expected}");
                        }
                    }

                    if (want.TryGetProperty("refresh_at_unix_ms", out var at) && agent.Sdk.RefreshAt != at.GetUInt64())
                    {
                        failed.Add($"{where}: refresh at {agent.Sdk.RefreshAt}");
                    }

                    if (want.TryGetProperty("effective", out var effective) &&
                        !JsonNode.DeepEquals(JsonNode.Parse(Effective(agent.Effective)), JsonNode.Parse(effective.GetRawText())))
                    {
                        failed.Add($"{where}: effective {Effective(agent.Effective)}");
                    }

                    if (want.TryGetProperty("set_aside", out var names) &&
                        !JsonNode.DeepEquals(JsonNode.Parse(SetAside(agent.Effective)), JsonNode.Parse(names.GetRawText())))
                    {
                        failed.Add($"{where}: set aside {SetAside(agent.Effective)}");
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
    public void TheTimerEndsTheWindowOnTheMonotonicClockWhateverTheWallClockDoes()
    {
        var agent = this.Start(null);
        try
        {
            Assert.Null(this.Armed); // no window, no timer
            var status = agent.RemoteConfig(Vectors.Config(
                "{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:10:00Z\",\"debug_sampling_rate\":\"1\"}"));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            Assert.Equal(1, agent.Controls.Sampler.Policy.Rate);
            Assert.Contains("debug_until", Effective(agent.Effective));
            var (delay, fire) = this.Armed!.Value;
            Assert.Equal(TimeSpan.FromMinutes(10), delay);

            // The wall clock jumps back an hour; the process's own clock reaches the deadline.
            this.wall = Noon - (60 * Minute);
            this.nanos = (long)TimeSpan.FromMinutes(10).TotalMilliseconds * 1_000_000;
            fire();
            Assert.Equal(0.1, agent.Controls.Sampler.Policy.Rate);
            Assert.DoesNotContain("debug_until", Effective(agent.Effective));
            Assert.Null(this.Armed);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    [Fact]
    public void ATimerAFireEarlyOrAWallClockJumpForwardDoesntEndTheWindowEarly()
    {
        var agent = this.Start(null);
        try
        {
            agent.RemoteConfig(Vectors.Config("{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:10:00Z\"}"));
            var (_, fire) = this.Armed!.Value;
            this.wall = Noon + (20 * Minute); // the wall clock jumped forward
            this.nanos = (long)TimeSpan.FromMinutes(5).TotalMilliseconds * 1_000_000;
            fire(); // early, by the process's clock
            Assert.Equal(1, agent.Controls.Sampler.Policy.Rate); // the debug rate (1 by default)
            Assert.Equal(TimeSpan.FromMinutes(5), this.Armed!.Value.Delay); // armed again for the rest
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    [Fact]
    public void AFailedConfigAfterTheWallClockPassedDebugUntilEndsDebugMode()
    {
        var agent = this.Start(null);
        try
        {
            agent.RemoteConfig(Vectors.Config("{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:10:00Z\"}"));
            Assert.Equal(1, agent.Controls.Sampler.Policy.Rate);

            // The machine sleeps: the wall clock passes the end, the process's clock doesn't move,
            // so the timer hasn't gone off. Then a bad document comes.
            this.wall = Noon + (25 * Minute);
            var status = agent.RemoteConfig(Vectors.Config("{\"sampling_rate\":\"7\"}"));
            Assert.Equal(RemoteConfigStatuses.Failed, status.Status);
            Assert.Equal("{\"sampling_rate\":\"0.1\"}", Effective(agent.Effective));
            Assert.Equal(0.1, agent.Controls.Sampler.Policy.Rate);
            Assert.Null(this.Armed);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    [Fact]
    public void AStaleTimerDoesNothingAfterANewerDecision()
    {
        var agent = this.Start(null);
        try
        {
            agent.RemoteConfig(Vectors.Config("{\"debug_until\":\"2026-10-14T12:10:00Z\",\"debug_sampling_rate\":\"0.5\"}"));
            var (_, stale) = this.Armed!.Value;
            agent.RemoteConfig(Vectors.Config("{\"debug_until\":\"2026-10-14T12:20:00Z\",\"debug_sampling_rate\":\"0.5\"}"));
            Assert.Equal(TimeSpan.FromMinutes(20), this.Armed!.Value.Delay);
            this.nanos = (long)TimeSpan.FromMinutes(10).TotalMilliseconds * 1_000_000;
            this.wall = Noon + (10 * Minute);
            stale(); // the first window's timer, already running when the second came
            Assert.Equal(0.5, agent.Controls.Sampler.Policy.Rate);
            Assert.Contains("12:20:00Z", Effective(agent.Effective));

            // A config without debug_until ends it at once, and the timer with it.
            agent.RemoteConfig(Vectors.Config("{\"send_logs\":\"true\"}"));
            Assert.Null(agent.Controls.Sampler.Policy.Rate);
            Assert.Null(this.Armed);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    [Fact]
    public void DebugSetsTheRulesAndTheScopeLevelsAsideAndTheCapsStay()
    {
        var agent = this.Start(null);
        try
        {
            var status = agent.RemoteConfig(Vectors.Config(
                "{\"sampling_rules\":[{\"rate\":0,\"name\":\"GET\"}],\"send_logs_level_by_scope\":\"noisy.*=error\"," +
                "\"sampling_traces_per_second\":\"5\",\"send_logs_per_second\":\"7\"," +
                "\"debug_until\":\"2026-10-14T12:10:00Z\",\"debug_send_logs_level\":\"debug\"}"));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            Assert.Empty(agent.Controls.Sampler.Policy.Rules);
            Assert.Equal(5, agent.Controls.Sampler.Policy.Cap!.PerSecond);
            Assert.Equal(7, agent.Controls.Policy(Signal.Logs).Cap!.PerSecond);
            Assert.True(agent.Controls.LogLevels.Keeps("noisy.db", 5)); // debug: the scope's error level is aside

            this.nanos = (long)TimeSpan.FromMinutes(10).TotalMilliseconds * 1_000_000;
            this.wall = Noon + (10 * Minute); // exactly at debug_until: over (the window is half-open)
            this.Armed!.Value.Fire();
            Assert.Single(agent.Controls.Sampler.Policy.Rules);
            Assert.False(agent.Controls.LogLevels.Keeps("noisy.db", 13));
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    [Fact]
    public void ThePluginDeclaresDebugMode()
    {
        var agent = this.Start(null);
        try
        {
            foreach (var key in new[] { "debug_until", "debug_sampling_rate", "debug_send_logs_level" })
            {
                Assert.Contains(key, agent.HostNow(providersBuilt: false).Live);
                Assert.Contains(key, agent.HostNow(providersBuilt: true).Live);
            }
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    private sealed class Scheduled : IDisposable
    {
        public bool Disposed { get; private set; }

        public void Dispose() => this.Disposed = true;
    }
}
