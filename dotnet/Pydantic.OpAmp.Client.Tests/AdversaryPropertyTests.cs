// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Security.Cryptography;
using System.Text;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The shell contract (spec/shell-contract.md) against a hostile core: the client runs
/// the adversary (testdata/core/opamp_core_adversary.wasm, a module with the core's ABI that hands
/// the shell effect sequences the real core never makes) in place of the embedded core, against a
/// local server that does what each URL's query says. The harness is the one every SDK runs: one client per
/// seed, several at once; it reads the adversary's notes, calls, stops, violations and verdict
/// from the callback, and checks what only it can see (callbacks, stop's bound, connections and
/// requests after stop, downloads after stop, the state file). Seeds: OPAMP_ADVERSARY_SEEDS
/// (default 24) from OPAMP_ADVERSARY_BASE (default 1); OPAMP_ADVERSARY_SEED=S replays one;
/// OPAMP_ADVERSARY_WASM overrides the module. The main property and the cancel productions run in
/// both modes. In Rust mode each run has a raw server of its own (AdversaryRawServer.cs), which the
/// adversary opens streams to (raw=PORT, tls=NAME in the endpoint's query): it hands the shell the
/// stream effects, and the platform's as well (it plays the core, not the shell inside it), which
/// reach the platform executor beside Rust mode's, as a core's saves do. So the streams themselves,
/// the choice of which of the two takes an effect, and Rust mode's stop and shutdown all run
/// against a hostile core.
/// </summary>
public partial class AdversaryPropertyTests
{
    private const string Prefix = "opamp-adversary";
    private const int AtOnce = 4;
    private const int FocusSeeds = 3;
    private const int MaxFocusSeeds = 10;

    // The adversary's margin for late timers and overdue answers: its default (3 s). Saves run
    // beside the client's loop (S4, fixed), so a store_burst no longer holds the loop up.
    private const int Margin = 3_000;

    private static readonly TimeSpan StopBound = TimeSpan.FromSeconds(15);
    private static readonly TimeSpan Settle = TimeSpan.FromSeconds(3);
    private static readonly TimeSpan AskBound = TimeSpan.FromSeconds(90);

    /// <summary>
    /// How long after the harness's own marker a request or upgrade the client had already sent may
    /// arrive. The marker is a request the harness sends the moment stop returns, on a connection of
    /// its own (<see cref="Mark"/>): it waits for the server's accept and read as whatever the client
    /// sent before then does, so a server late to both is late to the marker too. Measured from the
    /// moment stop returned, as it was, the 50 ms called that lateness the client's: on loaded CI
    /// runners Node's server has taken 171 ms and 348 ms to get to a request already on its way.
    /// </summary>
    private static readonly TimeSpan ArrivalGrace = TimeSpan.FromMilliseconds(50);

    /// <summary>The marker is answered within this of being sent: a server that never answers it fails the seed, and doesn't hang it.</summary>
    private static readonly TimeSpan MarkBound = TimeSpan.FromSeconds(30);

    /// <summary>
    /// The productions that trigger a bug this client is known to have, with its S-number (spec/shell-contract.md),
    /// why, and what the failing run says. The main run leaves them out; a second test checks
    /// each still fails. None now: S7 (wake_huge, wake_max, timeout_huge) is fixed, and the
    /// productions of S1 and S2 (stop_while_opening, ws_hold_past_timeout), a shell's duties
    /// without cancels, are gone from the adversary: their forms with cancels (stop_cancel,
    /// cancel_ws_opening) run in the main batch and in <see cref="NothingAfterACancel"/>, and pass.
    /// </summary>
    private static readonly Dictionary<string, Known> KnownBugs = new()
    {
    };

    private static readonly Lazy<Func<ICoreEngine>> Adversary =
        new(() => WasmCore.Of(File.ReadAllBytes(WasmPath())));

    private readonly ITestOutputHelper output;

    public AdversaryPropertyTests(ITestOutputHelper output) => this.output = output;

    // covers: RULE-SHELL-01, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09, RULE-SHELL-10
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task TheShellKeepsTheContractAgainstAHostileCore(OpAmpMode mode)
    {
        var replay = Env("OPAMP_ADVERSARY_SEED");
        var count = Env("OPAMP_ADVERSARY_SEEDS") ?? 24;
        var first = Env("OPAMP_ADVERSARY_BASE") ?? 1;
        var seeds = replay is { } one ? [one] : Enumerable.Range(0, (int)count).Select(i => first + (ulong)i).ToArray();
        using var server = new AdversaryServer();
        var results = await RunAllAsync(server, seeds, s => Query(s, off: KnownBugs.Keys, margin: Margin), mode);
        foreach (var r in results)
        {
            foreach (var t in r.Tolerated)
            {
                this.output.WriteLine($"seed {r.Seed}: tolerated {t}");
            }
        }

        var failed = results.Where(r => r.Failures.Count > 0).ToArray();
        var report = string.Join("\n", failed.Select(r =>
            $"seed {r.Seed}: {string.Join("; ", r.Failures)}\n  replay: OPAMP_ADVERSARY_SEED={r.Seed} dotnet test Pydantic.OpAmp.Client.Tests --filter {nameof(AdversaryPropertyTests)}"));
        this.output.WriteLine($"{mode} mode: {seeds.Length} seeds, {failed.Length} failed; known bugs left out: {string.Join(", ", KnownBugs.Keys)}");
        Assert.True(failed.Length == 0, $"{failed.Length} of {seeds.Length} seeds broke the contract:\n{report}");

        // A production that never ran tested nothing (a replay of one seed can't run them all).
        if (replay is not null)
        {
            return;
        }

        var enabled = results.SelectMany(r => r.Enabled).ToHashSet();
        var used = results.SelectMany(r => r.Used).ToHashSet();
        var never = enabled.Except(used).Order().ToArray();
        this.output.WriteLine($"{mode} mode: {used.Count} of {enabled.Count} productions ran ({used.Count(p => StreamProduction(p))} of the streams'); never: {string.Join(", ", never)}");

        // A production is drawn at random, and runs only if the run's timing leaves it room, so one
        // can miss a batch by chance (with Rust mode's streams the grammar is half as large again):
        // each missed one runs in focus, on two seeds past the batch's, as in Java's, Node's and
        // Go's harnesses.
        var last = seeds[^1];
        var still = new List<string>();
        var broke = new List<string>();
        foreach (var p in never)
        {
            var focused = await RunAllAsync(server, [last + 1, last + 2], s => Query(s, off: KnownBugs.Keys, focus: p, margin: Margin), mode);
            broke.AddRange(focused.Where(r => r.Failures.Count > 0).Select(r =>
                $"seed {r.Seed}, {p} in focus: {string.Join("; ", r.Failures)}\n  replay: endpoint query {Query(r.Seed, off: KnownBugs.Keys, focus: p, margin: Margin)}"));
            this.output.WriteLine($"{mode} mode: {p} never ran in seeds {seeds[0]} to {last}; in focus in {last + 1} and {last + 2}: {(focused.Any(r => r.Used.Contains(p)) ? "ran" : "never ran")}");
            if (!focused.Any(r => r.Used.Contains(p)))
            {
                still.Add(p);
            }
        }

        Assert.True(broke.Count == 0, string.Join("\n", broke));
        Assert.True(still.Count == 0, $"enabled but never used in seeds {seeds[0]} to {last}, nor in focus in {last + 1} and {last + 2}: {string.Join(", ", still)}");
    }

    // covers: RULE-SHELL-01, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08
    [Fact]
    public async Task EachKnownBugStillBreaksTheContract()
    {
        var first = Env("OPAMP_ADVERSARY_BASE") ?? 1;
        var checks = KnownBugs.Select(async kv =>
        {
            using var server = new AdversaryServer();
            var off = KnownBugs.Keys.Where(k => k != kv.Key);
            var seen = new List<string>();
            var ran = new List<ulong>();
            for (var seed = first; seed < first + MaxFocusSeeds && ran.Count < FocusSeeds; seed++)
            {
                var r = await RunSeedAsync(server, seed, Query(seed, off, focus: kv.Key), tolerateS1: false);
                if (r.Failures.Any(f => f.Contains(kv.Value.Says, StringComparison.Ordinal)))
                {
                    return (kv.Key, Fixed: (string?)null, Seen: $"seed {r.Seed}: {string.Join("; ", r.Failures)}");
                }

                // Only a run that drew it tells: the stop productions need what they stop on.
                if (r.Used.Contains(kv.Key))
                {
                    ran.Add(seed);
                }

                seen.AddRange(r.Failures.Select(f => $"seed {r.Seed}: {f}"));
            }

            var why = ran.Count < FocusSeeds
                ? $"{kv.Key} ({kv.Value.Bug}) ran in only {ran.Count} of {MaxFocusSeeds} seeds in focus from {first}: the check can't tell"
                : $"{kv.Key} ({kv.Value.Bug}) ran in seeds {string.Join(", ", ran)} in focus and no longer fails (as \"{kv.Value.Says}\"): "
                    + $"the bug looks fixed, take it off {nameof(KnownBugs)}.";
            return (kv.Key, Fixed: why + (seen.Count > 0 ? $" Other failures: {string.Join("; ", seen)}" : string.Empty), Seen: string.Empty);
        }).ToArray();
        var results = await Task.WhenAll(checks);
        foreach (var r in results.Where(r => r.Fixed is null))
        {
            this.output.WriteLine($"{r.Key} ({KnownBugs[r.Key].Bug}): still fails, as expected: {Short(r.Seen, 400)}");
        }

        var fixedOnes = results.Where(r => r.Fixed is not null).Select(r => r.Fixed!).ToArray();
        Assert.True(fixedOnes.Length == 0, string.Join("\n", fixedOnes));
    }

    /// <summary>The cancel productions (RULE-SHELL-06), in every run's grammar.</summary>
    private static readonly string[] Cancels =
    [
        "cancel_download", "cancel_http", "cancel_http_done", "cancel_store", "cancel_twice",
        "cancel_ws_closing", "cancel_ws_opening", "stop_cancel",
    ];

    /// <summary>The cancel productions of the streams and their files (RULE-SHELL-06), in a Rust mode run's grammar.</summary>
    private static readonly string[] StreamCancels =
    [
        "cancel_file", "cancel_splice", "cancel_stream_open", "cancel_stream_opening", "cancel_stream_resolving",
    ];

    /// <summary>
    /// Every cancel production is enabled, and each runs in focus, two seeds each: in Rust mode
    /// those of the streams too.
    /// </summary>
    // covers: RULE-SHELL-06
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task NothingAfterACancel(OpAmpMode mode)
    {
        using var server = new AdversaryServer();
        var seed = Env("OPAMP_ADVERSARY_BASE") ?? 1;
        string[] cancels = mode == OpAmpMode.Rust ? [.. Cancels, .. StreamCancels] : Cancels;
        var probe = await RunSeedAsync(server, seed, Query(seed, KnownBugs.Keys, steps: 3), mode: mode);
        Assert.True(probe.Verdict, $"seed {seed}: no verdict: {string.Join("; ", probe.Failures)}");
        var missing = cancels.Where(p => !probe.Enabled.Contains(p)).ToArray();
        Assert.True(missing.Length == 0, $"cancel productions not enabled: {string.Join(", ", missing)} (enabled: {string.Join(",", probe.Enabled)})");

        // Seeds base + 10 (i + 1) + k, k = 0, 1, for the i-th production, in focus.
        var focus = new Dictionary<ulong, string>();
        for (var i = 0; i < cancels.Length; i++)
        {
            for (ulong k = 0; k < 2; k++)
            {
                focus[seed + (10 * (ulong)(i + 1)) + k] = cancels[i];
            }
        }

        var results = await RunAllAsync(server, [.. focus.Keys], s => Query(s, KnownBugs.Keys, focus: focus[s], margin: Margin), mode);
        var failed = results.Where(r => r.Failures.Count > 0).Select(r => $"seed {r.Seed}, {focus[r.Seed]} in focus: {string.Join("; ", r.Failures)}").ToArray();
        Assert.True(failed.Length == 0, string.Join("\n", failed));
        var unused = cancels.Where(p => !results.Any(r => focus[r.Seed] == p && r.Used.Contains(p))).ToArray();
        this.output.WriteLine($"{mode} mode: {results.Length} runs in focus; in focus and never used: {string.Join(", ", unused)}");
    }

    /// <summary>
    /// The server's own measure (a nightly's seed 20729001 counted a request as arriving after stop
    /// returned): a request arrives when its line and headers have reached the server, however long
    /// the server then takes to handle it; a head that comes in two parts arrives with the second.
    /// </summary>
    // covers: RULE-SHELL-07
    [Fact]
    public async Task AnArrivalIsWhenItsHeadCame()
    {
        using var server = new AdversaryServer();
        server.AfterHead = () => Task.Delay(2_000); // a loaded host, between the bytes and the handling
        var sent = await RequestAsync(server, "GET /d?run=whole HTTP/1.1\r\nHost: x\r\n\r\n");
        var late = Stopwatch.GetElapsedTime(sent, server.For("whole").LastArrival);
        Assert.True(late < TimeSpan.FromSeconds(1), $"a head sent whole counted as arriving {late.TotalMilliseconds:F0} ms after it was sent: when the server handled it");

        server.AfterHead = () => Task.CompletedTask;
        var second = await RequestAsync(server, "GET /d?run=split HTTP/1.1\r\n", "Host: x\r\n\r\n");
        Assert.True(server.For("split").LastArrival >= second, "a head sent in two parts counted as arriving before its second part was sent");
    }

    /// <summary>
    /// The harness's own measure of what arrived after stop (Node's seed 5 failed twice on loaded
    /// CI runners, 171 ms and 348 ms "after stop returned", and passes elsewhere): a request sent
    /// before stop returned isn't the client's to answer for however late the server takes it, and
    /// one sent after stop returned still is.
    /// </summary>
    // covers: RULE-SHELL-07
    [Fact]
    public async Task AnArrivalIsJudgedAgainstTheMarker()
    {
        using var server = new AdversaryServer();

        // Sent before stop returned, to a server 400 ms late to accept anything: by the moment
        // stop returned it arrived late, by the marker it didn't.
        server.Late = TimeSpan.FromMilliseconds(400);
        using var before = new TcpClient { NoDelay = true };
        before.Connect(IPAddress.Loopback, server.Port);
        before.GetStream().Write(Encoding.ASCII.GetBytes("GET /d?run=late HTTP/1.1\r\nHost: x\r\n\r\n"));
        var stopAt = Stopwatch.GetTimestamp();
        Assert.Null(await Mark(server.Port, "late"));
        using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
        Assert.True(await before.GetStream().ReadAsync(new byte[1], cts.Token) > 0, "no answer");
        var late = Stopwatch.GetElapsedTime(stopAt, server.For("late").LastArrival);
        Assert.True(late > ArrivalGrace, $"the server was to be late: it took the request {late.TotalMilliseconds:F0} ms after stop returned");
        var said = server.For("late").AfterStop(stopAt);
        this.output.WriteLine($"a request sent before stop returned, taken {late.TotalMilliseconds:F0} ms after it: {said ?? "passes"}");
        Assert.Null(said);

        // Sent after stop returned: 100 ms after the marker sent then was answered.
        server.Late = TimeSpan.Zero;
        stopAt = Stopwatch.GetTimestamp();
        Assert.Null(await Mark(server.Port, "after"));
        await Task.Delay(100);
        await RequestAsync(server, "GET /d?run=after HTTP/1.1\r\nHost: x\r\n\r\n");
        said = server.For("after").AfterStop(stopAt);
        this.output.WriteLine($"a request sent 100 ms after stop returned: {said ?? "passes"}");
        Assert.True(said is not null && said.Contains("RULE-SHELL-07", StringComparison.Ordinal), "a request sent 100 ms after stop returned passed");
    }

    /// <summary>
    /// Send the harness's marker for <paramref name="run"/> on a connection of its own: the request
    /// is written before this returns. The task gives what went wrong; null: it was answered 204.
    /// </summary>
    private static Task<string?> Mark(int port, string run)
    {
        var c = new TcpClient { NoDelay = true };
        try
        {
            c.Connect(IPAddress.Loopback, port);
            c.GetStream().Write(Encoding.ASCII.GetBytes($"GET /marker?run={run} HTTP/1.1\r\nHost: x\r\n\r\n"));
        }
        catch (Exception e) when (e is SocketException or IOException)
        {
            c.Dispose();
            return Task.FromResult<string?>(e.Message);
        }

        return MarkAnsweredAsync(c);
    }

    private static async Task<string?> MarkAnsweredAsync(TcpClient c)
    {
        using (c)
        {
            try
            {
                using var cts = new CancellationTokenSource(MarkBound);
                using var reader = new StreamReader(c.GetStream(), Encoding.ASCII);
                var status = await reader.ReadLineAsync(cts.Token);
                return status is not null && status.StartsWith("HTTP/1.1 204 ", StringComparison.Ordinal) ? null : $"answered {status ?? "nothing"}";
            }
            catch (OperationCanceledException)
            {
                return $"not answered within {MarkBound.TotalSeconds} s";
            }
            catch (Exception e) when (e is SocketException or IOException)
            {
                return e.Message;
            }
        }
    }

    /// <summary>Send a request's head (in two writes 300 ms apart if <paramref name="rest"/>), read the answer: when the last part went.</summary>
    private static async Task<long> RequestAsync(AdversaryServer server, string head, string? rest = null)
    {
        using var c = new TcpClient { NoDelay = true };
        await c.ConnectAsync(IPAddress.Loopback, server.Port);
        var s = c.GetStream();
        var sent = Stopwatch.GetTimestamp();
        await s.WriteAsync(Encoding.ASCII.GetBytes(head));
        if (rest is not null)
        {
            await Task.Delay(300);
            sent = Stopwatch.GetTimestamp();
            await s.WriteAsync(Encoding.ASCII.GetBytes(rest));
        }

        using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
        Assert.True(await s.ReadAsync(new byte[1], cts.Token) > 0, "no answer");
        return sent;
    }

    /// <summary>
    /// The id of the adversary's store whose data <paramref name="data"/> is ("opamp-adversary seed S
    /// store ID ..."), if it is one of seed <paramref name="seed"/>'s.
    /// </summary>
    private static ulong? StoreId(byte[] data, ulong seed)
    {
        var prefix = System.Text.Encoding.ASCII.GetBytes($"{Prefix} seed {seed} store ");
        if (!data.AsSpan().StartsWith(prefix))
        {
            return null;
        }

        var rest = data.AsSpan(prefix.Length);
        var end = rest.IndexOf((byte)' ');
        return ulong.TryParse(System.Text.Encoding.ASCII.GetString(end < 0 ? rest : rest[..end]), NumberStyles.None, CultureInfo.InvariantCulture, out var id)
            ? id
            : null;
    }

    /// <summary>Whether <paramref name="production"/> is one of Rust mode's: of the streams, or of the files written through them.</summary>
    private static bool StreamProduction(string production) =>
        production.Contains("stream", StringComparison.Ordinal) || production.Contains("splice", StringComparison.Ordinal) || production.Contains("file", StringComparison.Ordinal);

    private static ulong? Env(string name) =>
        ulong.TryParse(Environment.GetEnvironmentVariable(name), NumberStyles.None, CultureInfo.InvariantCulture, out var v) ? v : null;

    private static string Short(string s, int max = 300) => s.Length <= max ? s : s[..max] + "...";

    /// <summary>The module: OPAMP_ADVERSARY_WASM, or testdata/core/opamp_core_adversary.wasm in the repository above the tests.</summary>
    private static string WasmPath()
    {
        if (Environment.GetEnvironmentVariable("OPAMP_ADVERSARY_WASM") is { Length: > 0 } path)
        {
            return File.Exists(path) ? path : throw new FileNotFoundException($"OPAMP_ADVERSARY_WASM={path}: no such file");
        }

        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            var testdata = Path.Combine(dir.FullName, "testdata", "core");
            if (Directory.Exists(testdata))
            {
                var wasm = Path.Combine(testdata, "opamp_core_adversary.wasm");
                return File.Exists(wasm)
                    ? wasm
                    : throw new FileNotFoundException($"{wasm} is missing: fetch it with scripts/fetch_core.py (or set OPAMP_ADVERSARY_WASM)");
            }
        }

        throw new DirectoryNotFoundException($"no testdata/core above {AppContext.BaseDirectory} (set OPAMP_ADVERSARY_WASM)");
    }

    private static string Query(ulong seed, IEnumerable<string> off, string? focus = null, int? steps = null, int? margin = null)
    {
        var q = $"seed={seed}";
        var offList = string.Join(',', off.Order());
        if (offList.Length > 0)
        {
            q += $"&off={offList}";
        }

        if (focus is not null)
        {
            q += $"&focus={focus}";
        }

        if (steps is not null)
        {
            q += $"&steps={steps}";
        }

        if (margin is not null)
        {
            q += $"&margin={margin}";
        }

        return q;
    }

    private static async Task<SeedResult[]> RunAllAsync(AdversaryServer server, ulong[] seeds, Func<ulong, string> query, OpAmpMode mode = OpAmpMode.DotNet)
    {
        using var slots = new SemaphoreSlim(AtOnce);
        return await Task.WhenAll(seeds.Select(async seed =>
        {
            await slots.WaitAsync();
            try
            {
                return await RunSeedAsync(server, seed, query(seed), mode: mode);
            }
            finally
            {
                slots.Release();
            }
        }));
    }

    private static async Task<SeedResult> RunSeedAsync(AdversaryServer server, ulong seed, string query, bool tolerateS1 = false, OpAmpMode mode = OpAmpMode.DotNet)
    {
        var run = new SeedRun(server, seed, query, tolerateS1, mode);
        try
        {
            await Task.Run(run.RunAsync);
        }
        catch (Exception e)
        {
            run.Fail($"the harness: {e}");
        }
        finally
        {
            server.Drop(seed); // whatever a hung client still holds
            run.Cleanup();
        }

        return run.Result();
    }

    private static Dictionary<string, (long Size, DateTime Written)> Listing(string dir)
    {
        try
        {
            return new DirectoryInfo(dir).GetFiles().ToDictionary(f => f.Name, f => (f.Length, f.LastWriteTimeUtc));
        }
        catch (DirectoryNotFoundException)
        {
            return new();
        }
    }

    private sealed record Known(string Bug, string Why, string Says);

    private sealed record SeedResult(ulong Seed, List<string> Failures, List<string> Tolerated, string[] Used, string[] Enabled, bool Verdict);

    /// <summary>One seed: one client on the adversary, and what its callback saw.</summary>
    /// <remarks>
    /// With <paramref name="tolerateS1"/>, a stop that hangs on a WebSocket open at the server is
    /// S1 (any upgrade still unanswered at stop can open after it): the server drops it, and
    /// if stop then returns the run goes on, logged.
    /// </remarks>
    private sealed class SeedRun(AdversaryServer server, ulong seed, string query, bool tolerateS1, OpAmpMode mode)
    {
        // The run's raw server, for its streams (Rust mode).
        private readonly AdversaryRaw? raw = mode == OpAmpMode.Rust ? new AdversaryRaw(Pki.Server) : null;
        private readonly string dir = Directory.CreateTempSubdirectory("opamp-adversary-").FullName;
        private readonly TaskCompletionSource<OpAmpClient> handle = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource<Task> stopAsked = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource stoppedTold = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly List<string> failures = new();
        private readonly List<string> tolerated = new();
        private readonly List<string> verdicts = new();
        private int inProgress;
        private int startReturned;
        private int stopped;
        private ulong notes;
        private int early;

        private string StateFile => Path.Combine(this.dir, "state");

        public void Fail(string what)
        {
            lock (this.failures)
            {
                this.failures.Add(Short(what, 600));
            }
        }

        public async Task RunAsync()
        {
            // Rust mode: the streams go to the run's raw server, whose certificate (for TlsName) is
            // under the CA the client is given.
            var streams = this.raw is null ? string.Empty : $"&raw={this.raw.Port}&tls={TlsName}";
            var client = OpAmpClient.Start(
                new OpAmpClientOptions
                {
                    Endpoint = new Uri($"http://127.0.0.1:{server.Port}/opamp?{query}{streams}"),
                    StateFile = this.StateFile,
                    Mode = mode,
                    Tls = this.raw is null ? null : new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca) },
                },
                new OpAmpStatus(),
                this.Notify,
                Adversary.Value);
            Volatile.Write(ref this.startReturned, 1);
            this.handle.SetResult(client);

            // The script runs until its stop note (or the client stops on its own).
            await Task.WhenAny(this.stopAsked.Task, this.stoppedTold.Task, Task.Delay(AskBound));
            Task stopping;
            if (this.stopAsked.Task.IsCompleted)
            {
                stopping = await this.stopAsked.Task;
            }
            else
            {
                if (!this.stoppedTold.Task.IsCompleted)
                {
                    this.Fail($"no stop asked for within {AskBound.TotalSeconds} s");
                }

                stopping = client.StopAsync();
            }

            var returned = await Task.WhenAny(stopping, Task.Delay(StopBound)) == stopping;
            if (!returned && tolerateS1 && server.For(seed).Open > 0)
            {
                server.Drop(seed);
                returned = await Task.WhenAny(stopping, Task.Delay(TimeSpan.FromSeconds(5))) == stopping;
                if (returned)
                {
                    this.tolerated.Add("S1: StopAsync waited on a WebSocket that opened after stop, until the server dropped it");
                }
            }

            var stopAt = Stopwatch.GetTimestamp();

            // Sent now, on a connection of its own: what the client sent before stop returned
            // reaches the server by the time this does (ArrivalGrace).
            var marked = returned ? Mark(server.Port, seed.ToString(CultureInfo.InvariantCulture)) : null;

            // And the raw server's, as a connection of the harness's own. From here a connection
            // the server was told not to read is read: only that tells it the connection has gone.
            var rawMarked = returned && this.raw is not null ? RawMark(this.raw.Port) : null;
            this.raw?.Release();
            if (!returned)
            {
                this.Fail($"StopAsync didn't return within {StopBound.TotalSeconds} s (RULE-SHELL-07)");
            }
            else if (stopping.Exception is { } e)
            {
                this.Fail($"StopAsync threw: {e.InnerException}");
            }

            // The run gets a moment for its connections to close; then nothing is left.
            var downloads = Executor.DownloadsDir(this.StateFile);
            var before = Listing(downloads);
            await Task.Delay(Settle);
            var counts = server.For(seed);
            if (counts.Open > 0)
            {
                this.Fail($"{counts.Open} WebSocket connection(s) still open {Settle.TotalSeconds} s after stop (RULE-SHELL-07)");
            }

            // A request the client had sent before stop returned may reach the server's read after
            // it, but not after the marker sent then has. With a stop that never returned there is
            // no marker, and nothing to measure from.
            if (marked is not null)
            {
                var late = Stopwatch.GetElapsedTime(stopAt, counts.LastArrival);
                if (await marked is { } markerError)
                {
                    this.Fail($"the harness's marker request failed: {markerError}");
                }
                else if (counts.AfterStop(stopAt) is { } broke)
                {
                    this.Fail(broke);
                }
                else if (late > TimeSpan.Zero)
                {
                    this.tolerated.Add($"a request or upgrade ({Short(counts.LastWhat, 120)}) arrived {late.TotalMilliseconds:F1} ms after stop returned, no more than {ArrivalGrace.TotalMilliseconds} ms after a request the harness sent then");
                }
            }

            // The streams: every connection gone, and none made after stop returned.
            if (this.raw is not null)
            {
                if (rawMarked is not null)
                {
                    if (await rawMarked is { } rawMarkerError)
                    {
                        this.Fail($"the harness's marker on the raw server failed: {rawMarkerError}");
                    }
                    else if (this.raw.AfterMark() is { } lateStream)
                    {
                        this.Fail(lateStream);
                    }
                }

                var (all, anonymous) = this.raw.Open();
                if (all > 0)
                {
                    this.Fail($"RULE-SHELL-07: {all} stream connection(s) still open {Settle.TotalSeconds} s after stop returned ({anonymous} never said who they were)");
                }
            }

            var after = Listing(downloads);
            var changed = after.Where(f => !before.TryGetValue(f.Key, out var b) || b != f.Value).Select(f => f.Key).ToArray();
            if (changed.Length > 0)
            {
                this.Fail($"download file(s) {string.Join(", ", changed)} appeared or changed after stop (S3, RULE-SHELL-07)");
            }

            if (Volatile.Read(ref this.stopped) != 1)
            {
                this.Fail($"Stopped told {this.stopped} times");
            }

            this.CheckVerdict();
        }

        public void Cleanup()
        {
            this.raw?.Dispose();
            try
            {
                Directory.Delete(this.dir, recursive: true);
            }
            catch (IOException)
            {
            }
        }

        public SeedResult Result()
        {
            string? verdict;
            lock (this.verdicts)
            {
                verdict = this.verdicts.Count == 1 ? this.verdicts[0] : null;
            }

            var fields = verdict is null ? new() : Fields(verdict);
            string[] Names(string key) => fields.TryGetValue(key, out var v) && v.Length > 0 ? v.Split(',') : [];
            if (this.early > 0)
            {
                this.tolerated.Add($"S5: {this.early} callback(s) before Start returned");
            }

            lock (this.failures)
            {
                return new SeedResult(seed, this.failures.ToList(), this.tolerated.ToList(), Names("used"), Names("enabled"), verdict is not null);
            }
        }

        private static Dictionary<string, string> Fields(string verdict) =>
            verdict.Split(' ').Skip(2).Select(t => t.Split('=', 2)).Where(kv => kv.Length == 2).ToDictionary(kv => kv[0], kv => kv[1]);

        private void CheckVerdict()
        {
            string[] verdicts;
            lock (this.verdicts)
            {
                verdicts = this.verdicts.ToArray();
            }

            if (verdicts.Length != 1)
            {
                this.Fail(verdicts.Length == 0 ? "no verdict" : $"{verdicts.Length} verdicts");
                return;
            }

            var v = Fields(verdicts[0]);
            if (v.GetValueOrDefault("violations") is not "0")
            {
                this.Fail($"the verdict counts violations: {Short(verdicts[0], 200)}");
            }

            // RULE-SHELL-09: the state file holds what was last answered stored, or the data of a
            // later store (one cancelled, or cut off by stop, while it was writing: it lands whole,
            // unreported).
            var stored = v.GetValueOrDefault("stored", "-");
            var bytes = File.Exists(this.StateFile) ? File.ReadAllBytes(this.StateFile) : null;
            var file = bytes is null ? null : Convert.ToHexString(bytes).ToLowerInvariant();
            var landed = bytes is null ? null : StoreId(bytes, seed);
            var later = stored == "-" ? landed is not null : landed > StoreId(Convert.FromHexString(stored), seed);
            if (stored == "-" ? !string.IsNullOrEmpty(file) && !later : file != stored && !later)
            {
                this.Fail($"the state file holds {Short(file ?? "nothing", 80)}, the verdict says stored={Short(stored, 80)} (RULE-SHELL-09)");
            }
        }

        /// <summary>The callback.</summary>
        private void Notify(OpAmpNotification n)
        {
            if (Interlocked.Increment(ref this.inProgress) > 1)
            {
                this.Fail("two callbacks at once (RULE-SHELL-01)");
            }

            try
            {
                if (Volatile.Read(ref this.startReturned) == 0)
                {
                    this.early++; // S5: the loop starts inside Start; tolerated, logged
                }

                if (Volatile.Read(ref this.stopped) > 0)
                {
                    this.Fail($"a callback after Stopped: {Short(n.ToString(), 120)} (RULE-SHELL-01)");
                }

                switch (n)
                {
                    case OpAmpNotification.Stopped s:
                        Interlocked.Increment(ref this.stopped);
                        if (s.Error is not null)
                        {
                            this.Fail($"the client stopped with an error: {s.Error.GetType().Name}: {s.Error.Message}");
                        }

                        this.stoppedTold.TrySetResult();
                        break;
                    case OpAmpNotification.BadMessage { Detail: var d } when d.StartsWith(Prefix, StringComparison.Ordinal):
                        this.OnAdversary(d);
                        break;
                }
            }
            finally
            {
                Interlocked.Decrement(ref this.inProgress);
            }
        }

        private void OnAdversary(string detail)
        {
            var what = detail[(Prefix.Length + 1)..];
            if (what.StartsWith("note ", StringComparison.Ordinal))
            {
                var n = ulong.Parse(what[5..], CultureInfo.InvariantCulture);
                if (n != this.notes + 1)
                {
                    this.Fail($"note {n} after note {this.notes} (RULE-SHELL-01)");
                }

                this.notes = Math.Max(this.notes, n);
            }
            else if (what == "call inside")
            {
                // "inside": the adversary checks this call reaches it before anything the client
                // takes in next (RULE-SHELL-10). One made from outside, for want of the handle,
                // says otherwise: the adversary owes itself none until a call from this
                // application has shown it holds its client.
                if (this.handle.Task.IsCompleted)
                {
                    this.handle.Task.Result.SetHealth(new ComponentHealth { Healthy = true, Status = "inside" });
                }
                else
                {
                    this.tolerated.Add("S5: a call due from inside the callback before Start returned; made from outside");
                    this.Outside(c => c.SetHealth(new ComponentHealth { Healthy = true, Status = "inside, late" }));
                }
            }
            else if (what == "call outside")
            {
                this.Outside(c => c.SetHealth(new ComponentHealth { Healthy = false, Status = "outside" }));
            }
            else if (what == "stop inside" && this.handle.Task.IsCompleted)
            {
                this.stopAsked.TrySetResult(this.handle.Task.Result.StopAsync()); // requested, not waited for
            }
            else if (what is "stop inside" or "stop outside")
            {
                this.Outside(c => this.stopAsked.TrySetResult(c.StopAsync()));
            }
            else if (what.StartsWith("violation ", StringComparison.Ordinal))
            {
                this.Fail(detail);
            }
            else if (what.StartsWith("verdict ", StringComparison.Ordinal))
            {
                lock (this.verdicts)
                {
                    this.verdicts.Add(detail);
                }
            }
        }

        /// <summary>Call the client from another task, once it has its handle.</summary>
        private void Outside(Action<OpAmpClient> call) => _ = Task.Run(async () =>
        {
            try
            {
                call(await this.handle.Task);
            }
            catch (Exception e)
            {
                this.Fail($"a call from outside threw: {e}");
            }
        });
    }

    /// <summary>
    /// The adversary's local server: POST /h, GET /d and the GET /w
    /// upgrade do what their query says. Per run (the <c>run</c> parameter) it counts the WebSocket
    /// connections open and remembers when the last request or upgrade arrived.
    /// </summary>
    private sealed class AdversaryServer : IDisposable
    {
        private readonly TcpListener listener = new(IPAddress.Loopback, 0);
        private readonly ConcurrentDictionary<string, Counts> runs = new();
        private readonly ConcurrentDictionary<TcpClient, string> live = new();

        public AdversaryServer()
        {
            this.listener.Start();
            _ = Task.Run(this.AcceptAsync);
        }

        public int Port => ((IPEndPoint)this.listener.LocalEndpoint).Port;

        /// <summary>A test's pause in this server's own handling of a request, once its head is read.</summary>
        public Func<Task> AfterHead { get; set; } = () => Task.CompletedTask;

        /// <summary>A test's: a loaded host's pause before this server accepts a connection that has come.</summary>
        public TimeSpan Late { get; set; }

        public Counts For(ulong seed) => this.For(seed.ToString(CultureInfo.InvariantCulture));

        public Counts For(string run) => this.runs.GetOrAdd(run, _ => new Counts());

        /// <summary>Close every connection of run <paramref name="seed"/>.</summary>
        public void Drop(ulong seed)
        {
            var run = seed.ToString(CultureInfo.InvariantCulture);
            foreach (var (c, r) in this.live)
            {
                if (r == run)
                {
                    c.Dispose();
                }
            }
        }

        public void Dispose()
        {
            this.listener.Stop();
            foreach (var c in this.live.Keys)
            {
                c.Dispose();
            }
        }

        /// <summary>The body pattern, the raw server's too: byte i is i % 251.</summary>
        public static byte[] Body(long size)
        {
            var b = new byte[size];
            for (var i = 0L; i < size; i++)
            {
                b[i] = (byte)(i % 251);
            }

            return b;
        }

        private static Dictionary<string, string> QueryOf(string target)
        {
            var q = target.IndexOf('?') is var i and >= 0 ? target[(i + 1)..] : string.Empty;
            return q.Split('&', StringSplitOptions.RemoveEmptyEntries)
                .Select(p => p.Split('=', 2))
                .GroupBy(p => p[0])
                .ToDictionary(g => g.Key, g => g.Last().Length > 1 ? Uri.UnescapeDataString(g.Last()[1]) : string.Empty);
        }

        /// <summary>Wait <paramref name="ms"/> (-1: until the client goes, or 30 s); false if the client went.</summary>
        private static async Task<bool> HoldAsync(Stream s, long ms)
        {
            if (ms == 0)
            {
                return true;
            }

            // Nothing more comes from the client before the answer: a read ends only when it goes.
            var probe = s.ReadAsync(new byte[1]).AsTask();
            var held = Task.Delay(ms < 0 ? 30_000 : (int)Math.Min(ms, int.MaxValue));
            return await Task.WhenAny(probe, held) == held;
        }

        /// <summary>
        /// A request's head, read a byte at a time (nothing past it is read), and when (a Stopwatch
        /// timestamp) it had reached this server: the moment the bytes waiting were seen to hold
        /// its last byte. A request arrives then, not when this server has got round to reading and
        /// parsing it: on a loaded host that lags the bytes, and a request sent before stop
        /// returned was counted as arriving after it (as Java's harness found).
        /// </summary>
        private static async Task<(string Line, Dictionary<string, string> Headers, long At)?> ReadHeadAsync(TcpClient tcp, Stream s)
        {
            var head = new List<byte>();
            var one = new byte[1];
            var covered = 0L; // bytes known to have arrived by at
            var at = 0L;
            while (true)
            {
                if (await s.ReadAsync(one) == 0)
                {
                    return null;
                }

                head.Add(one[0]);
                if (head.Count > covered)
                {
                    covered = head.Count + tcp.Available;
                    at = Stopwatch.GetTimestamp();
                }

                if (head.Count >= 4 && head[^4] == '\r' && head[^3] == '\n' && head[^2] == '\r' && head[^1] == '\n')
                {
                    break;
                }
            }

            var lines = Encoding.ASCII.GetString(head.ToArray()).Split("\r\n", StringSplitOptions.RemoveEmptyEntries);
            var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            foreach (var l in lines.Skip(1))
            {
                var colon = l.IndexOf(':');
                headers[l[..colon].Trim()] = l[(colon + 1)..].Trim();
            }

            return (lines[0], headers, at);
        }

        private async Task AcceptAsync()
        {
            while (true)
            {
                TcpClient c;
                try
                {
                    c = await this.listener.AcceptTcpClientAsync();
                }
                catch (Exception e) when (e is SocketException or ObjectDisposedException)
                {
                    return;
                }

                if (this.Late > TimeSpan.Zero)
                {
                    await Task.Delay(this.Late);
                }

                _ = Task.Run(() => this.ServeAsync(c));
            }
        }

        private async Task ServeAsync(TcpClient tcp)
        {
            this.live[tcp] = string.Empty;
            try
            {
                using (tcp)
                {
                    tcp.NoDelay = true;
                    var s = tcp.GetStream();
                    if (await ReadHeadAsync(tcp, s) is not { } head)
                    {
                        return;
                    }

                    var (line, headers, at) = head;
                    await this.AfterHead();
                    var q = QueryOf(line.Split(' ')[1]);
                    var run = q.GetValueOrDefault("run", string.Empty);
                    this.live[tcp] = run;
                    var counts = this.runs.GetOrAdd(run, _ => new Counts());
                    if (line.Split(' ')[1].StartsWith("/marker?", StringComparison.Ordinal))
                    {
                        // The harness's own, not the client's: when what was sent as stop returned got here.
                        counts.Marked(at);
                        await s.WriteAsync(Encoding.ASCII.GetBytes("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"));
                        return;
                    }

                    counts.Arrived(line, at);
                    long Num(string key, long otherwise) =>
                        q.TryGetValue(key, out var v) && long.TryParse(v, CultureInfo.InvariantCulture, out var n) ? n : otherwise;

                    if (headers.TryGetValue("Upgrade", out var upgrade) && upgrade.Equals("websocket", StringComparison.OrdinalIgnoreCase))
                    {
                        await WebSocketAsync(tcp, s, headers, counts, Num);
                        return;
                    }

                    if (headers.TryGetValue("Content-Length", out var cl) && int.TryParse(cl, CultureInfo.InvariantCulture, out var length) && length > 0)
                    {
                        await s.ReadExactlyAsync(new byte[length]);
                    }

                    if (!await HoldAsync(s, Num("delay", 0)) || Num("drop", 0) == 1)
                    {
                        return; // the client went, or the connection drops without an answer
                    }

                    var status = Num("status", 200);
                    var size = Num("size", 0);
                    var noBody = status is 204 or 304 or < 200;
                    var reply = $"HTTP/1.1 {status} Adversary\r\n" + (noBody ? string.Empty : $"Content-Length: {size}\r\n") + "Connection: close\r\n\r\n";
                    await s.WriteAsync(Encoding.ASCII.GetBytes(reply));
                    if (!noBody)
                    {
                        await s.WriteAsync(Body(size));
                    }

                    await s.FlushAsync();
                    tcp.Client.Shutdown(SocketShutdown.Send);
                }
            }
            catch (Exception e) when (e is IOException or SocketException or ObjectDisposedException or WebSocketException or InvalidOperationException)
            {
                // the client went
            }
            finally
            {
                this.live.TryRemove(tcp, out _);
            }
        }

        private static async Task WebSocketAsync(TcpClient tcp, Stream s, Dictionary<string, string> headers, Counts counts, Func<string, long, long> num)
        {
            var delay = num("delay", 0);
            if (delay < 0)
            {
                await HoldAsync(s, delay); // never answered
                return;
            }

            if (delay > 0)
            {
                await Task.Delay((int)Math.Min(delay, int.MaxValue));
            }

            if (num("status", 0) is var status and > 0)
            {
                await s.WriteAsync(Encoding.ASCII.GetBytes($"HTTP/1.1 {status} Refused\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"));
                return;
            }

            var accept = Convert.ToBase64String(SHA1.HashData(Encoding.ASCII.GetBytes(
                headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")));
            await s.WriteAsync(Encoding.ASCII.GetBytes(
                "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" + $"Sec-WebSocket-Accept: {accept}\r\n\r\n"));
            counts.Opened();
            try
            {
                using var ws = WebSocket.CreateFromStream(s, new WebSocketCreationOptions { IsServer = true, KeepAliveInterval = TimeSpan.Zero });
                var gate = new SemaphoreSlim(1, 1);
                async Task SendAsync(Func<Task> send)
                {
                    await gate.WaitAsync();
                    try
                    {
                        await send();
                    }
                    catch (Exception e) when (e is WebSocketException or IOException or ObjectDisposedException or InvalidOperationException)
                    {
                        // it is going
                    }
                    finally
                    {
                        gate.Release();
                    }
                }

                for (var i = 0; i < num("push", 0); i++)
                {
                    var data = Enumerable.Range(0, 16).Select(b => (byte)(i + b)).ToArray();
                    await SendAsync(() => ws.SendAsync(data, WebSocketMessageType.Binary, true, CancellationToken.None));
                }

                if (num("close", 0) is var close and > 0)
                {
                    _ = Task.Delay((int)close).ContinueWith(
                        _ => SendAsync(() => ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None)),
                        TaskScheduler.Default).Unwrap();
                }

                if (num("drop", 0) is var drop and > 0)
                {
                    _ = Task.Delay((int)drop).ContinueWith(_ => tcp.Dispose(), TaskScheduler.Default);
                }

                var echo = num("echo", 0) == 1;
                var buf = new byte[65536];
                using var message = new MemoryStream();
                while (true)
                {
                    var r = await ws.ReceiveAsync(buf, CancellationToken.None);
                    if (r.MessageType == WebSocketMessageType.Close)
                    {
                        if (ws.State == WebSocketState.CloseReceived)
                        {
                            await SendAsync(() => ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None));
                        }

                        return;
                    }

                    message.Write(buf, 0, r.Count);
                    if (r.EndOfMessage)
                    {
                        var data = message.ToArray();
                        message.SetLength(0);
                        if (echo && r.MessageType == WebSocketMessageType.Binary)
                        {
                            await SendAsync(() => ws.SendAsync(data, WebSocketMessageType.Binary, true, CancellationToken.None));
                        }
                    }
                }
            }
            finally
            {
                counts.Closed();
            }
        }
    }

    /// <summary>A run's counts at the server.</summary>
    private sealed class Counts
    {
        private readonly object gate = new();
        private int open;
        private long last;
        private string lastWhat = string.Empty;
        private long marker;

        public int Open => Volatile.Read(ref this.open);

        public long LastArrival
        {
            get
            {
                lock (this.gate)
                {
                    return this.last;
                }
            }
        }

        /// <summary>The last request line that arrived.</summary>
        public string LastWhat
        {
            get
            {
                lock (this.gate)
                {
                    return this.lastWhat;
                }
            }
        }

        /// <summary>A request or upgrade whose head reached the server at <paramref name="at"/> (a Stopwatch timestamp).</summary>
        public void Arrived(string what, long at)
        {
            lock (this.gate)
            {
                if (at > this.last)
                {
                    (this.last, this.lastWhat) = (at, what);
                }
            }
        }

        /// <summary>The harness's marker (GET /marker, sent as stop returned), whose head reached the server at <paramref name="at"/>.</summary>
        public void Marked(long at)
        {
            lock (this.gate)
            {
                this.marker = at;
            }
        }

        /// <summary>
        /// What breaks RULE-SHELL-07 in when this run's requests and upgrades arrived, stop having
        /// returned at <paramref name="stopAt"/> (null: nothing does): one that arrived more than
        /// <see cref="ArrivalGrace"/> after the marker the harness sent then.
        /// </summary>
        public string? AfterStop(long stopAt)
        {
            lock (this.gate)
            {
                var late = Stopwatch.GetElapsedTime(this.marker, this.last);
                return late > ArrivalGrace
                    ? $"a request or upgrade ({Short(this.lastWhat, 120)}) arrived {Stopwatch.GetElapsedTime(stopAt, this.last).TotalMilliseconds:F0} ms after stop returned, "
                        + $"{late.TotalMilliseconds:F0} ms after a request the harness sent then (RULE-SHELL-07)"
                    : null;
            }
        }

        public void Opened() => Interlocked.Increment(ref this.open);

        public void Closed() => Interlocked.Decrement(ref this.open);
    }
}
