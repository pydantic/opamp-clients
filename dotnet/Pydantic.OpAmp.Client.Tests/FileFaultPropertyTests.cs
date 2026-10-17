// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text;
using FsCheck;
using FsCheck.Fluent;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Faults in the file system under a client, through its file operations' seam: into one
/// operation of one of its first saves, an error (EIO); a write that writes half and fails (ENOSPC);
/// a slow operation (0.5-2 s, and then until the client has sent two messages since it began); a
/// rename that lands with a directory flush that fails; a failed write whose temporary file can't
/// be removed either (left over); and for downloads, a downloads
/// directory that can't be made, or a discard whose removal fails. Whatever the fault: the client
/// keeps heartbeating (a slow save holds up nothing: it holds no thread of the pool the client
/// runs on, and the messages it waits for come), every save gets at most one answer and all
/// but one still writing at stop exactly one, a save that failed is followed by one that is
/// stored, stop returns within its bound, the state file holds a whole save no older than the last
/// stored, opamp-tap holds the tap, and the next client on the state file (with a leftover
/// temporary file, as a crash leaves) starts from what was saved and saves.
/// Runs: OPAMP_FAULT_RUNS (default 6), seeds from a random base, printed on failure; replay one
/// with OPAMP_FAULT_SEED. OPAMP_KEEP_FILES keeps each run's directory (state file, taps).
/// </summary>
public class FileFaultPropertyTests
{
    private static readonly byte[] Package = Encoding.ASCII.GetBytes("the package, version 1");
    private static readonly TimeSpan StopTimeout = TimeSpan.FromMilliseconds(500);

    /// <summary>How long a slow operation waits, past its time, for the client's next two messages.</summary>
    private static readonly TimeSpan SlowHold = TimeSpan.FromSeconds(10);

    private static readonly Gen<Plan> Plans =
        from kind in Gen.Elements(Kind.Eio, Kind.HalfWrite, Kind.Slow, Kind.LandsButFails, Kind.LeftOver)
        from save in Gen.Choose(0, 2)
        from op in kind switch
        {
            Kind.Eio => Gen.Elements("remove", "create", "write", "sync", "rename", "syncdir"),
            Kind.Slow => Gen.Elements("remove", "create", "write", "sync", "rename", "syncdir"),
            Kind.LandsButFails => Gen.Constant("syncdir"),
            _ => Gen.Constant("write"),
        }
        from slowMs in Gen.Choose(500, 2000)
        from download in Gen.Elements(DownloadFault.None, DownloadFault.NoDirectory, DownloadFault.DiscardFails)
        select new Plan(kind, save, op, slowMs, download);

    private readonly ITestOutputHelper output;

    public FileFaultPropertyTests(ITestOutputHelper output) => this.output = output;

    internal enum Kind
    {
        Eio,
        HalfWrite,
        Slow,
        LandsButFails,
        LeftOver,
    }

    internal enum DownloadFault
    {
        None,
        NoDirectory,
        DiscardFails,
    }

    // covers: RULE-SHELL-07, RULE-SHELL-09
    [Fact]
    public async Task WhateverTheFileSystemDoesTheClientKeepsItsPromises()
    {
        var runs = int.TryParse(Environment.GetEnvironmentVariable("OPAMP_FAULT_RUNS"), out var n) ? n : 6;
        var replay = ulong.TryParse(Environment.GetEnvironmentVariable("OPAMP_FAULT_SEED"), out var s) ? s : (ulong?)null;
        var first = replay ?? (ulong)Random.Shared.NextInt64(1, long.MaxValue / 2);
        var seeds = replay is not null ? [first] : Enumerable.Range(0, runs).Select(i => first + (ulong)i).ToArray();
        if (TapKit.Checker() is null)
        {
            this.output.WriteLine("opamp-tap: none that runs here (set OPAMP_TAP_BIN, or build target/release/opamp-tap): taps not checked");
        }

        foreach (var seed in seeds)
        {
            var plan = Gen.Sample(Plans, 1, new Rnd(seed), 20)[0];
            var log = new ConcurrentQueue<string>();
            try
            {
                await RunAsync(plan, log);
            }
            catch (Exception e)
            {
                this.output.WriteLine(string.Join('\n', log));
                Assert.Fail($"seed {seed} failed: {e.Message}\nplan: {plan}\n"
                    + $"replay: OPAMP_FAULT_SEED={seed} dotnet test Pydantic.OpAmp.Client.Tests --filter {nameof(FileFaultPropertyTests)}\n{e}");
            }

            this.output.WriteLine($"seed {seed}: {plan}: held");
        }
    }

    private static ServerToAgent Reply(AgentToServer msg, int port, int n)
    {
        var reply = TapKit.Offer(msg, $"http://127.0.0.1:{port}/pkg", Package);
        var hash = ByteString.CopyFromUtf8($"config-{n}");
        var config = new AgentConfigMap();
        config.ConfigMap[string.Empty] = new AgentConfigObject { Body = hash, ContentType = "text/plain" };
        reply.RemoteConfig = new AgentRemoteConfig { Config = config, ConfigHash = hash };
        return reply;
    }

    private static async Task<bool> UntilAsync(Func<bool> condition, double seconds = 15)
    {
        var until = DateTime.UtcNow.AddSeconds(seconds);
        while (!condition())
        {
            if (DateTime.UtcNow > until)
            {
                return false;
            }

            await Task.Delay(20);
        }

        return true;
    }

    /// <summary>The tap's records so far (none if it isn't there yet).</summary>
    private static List<TapRecord> Records(string tap)
    {
        try
        {
            return TapKit.Read(tap);
        }
        catch (Exception e) when (e is IOException or InvalidOperationException or ArgumentOutOfRangeException)
        {
            return [];
        }
    }

    private static int Answered(List<TapRecord> records, Input.KindOneofCase kind) =>
        TapKit.Inputs(records).Count(i => i.Input.KindCase == kind);

    private static async Task RunAsync(Plan plan, ConcurrentQueue<string> log)
    {
        var dir = Directory.CreateTempSubdirectory("opamp-faults-").FullName;
        var state = Path.Combine(dir, "state");
        var tapA = Path.Combine(dir, "a.tap");
        var tapB = Path.Combine(dir, "b.tap");
        var messages = 0;
        var since = Stopwatch.StartNew();
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                if (line.StartsWith("POST", StringComparison.Ordinal))
                {
                    Interlocked.Increment(ref messages);
                    var port = ((System.Net.IPEndPoint)c.Tcp.Client.LocalEndPoint!).Port;
                    // A new remote config every 250 ms: a save each time.
                    await c.AnswerAsync(headers, m => Reply(m, port, (int)(since.ElapsedMilliseconds / 250)));
                }
                else
                {
                    await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {Package.Length}\r\n\r\n", Package);
                }
            }
        });

        var faulty = new FaultyFiles(plan, state, log, () => Volatile.Read(ref messages));
        var options = new OpAmpClientOptions
        {
            Mode = OpAmpMode.DotNet,
            Endpoint = new Uri(server.Url("http")),
            Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig),
            HeartbeatInterval = TimeSpan.FromMilliseconds(100),
            StopTimeout = StopTimeout,
            StateFile = state,
            AcceptPackages = true,
        };
        var installs = new ConcurrentQueue<string>();
        OpAmpClient? a = null;
        void Notify(OpAmpNotification note)
        {
            if (note is OpAmpNotification.InstallPackage install)
            {
                installs.Enqueue(install.Path);
                a!.PackageInstalled(install.Name, install.Package.Hash.ToByteArray());
            }
        }

        var clock = new Stopwatch();
        StateFile.Saving += faulty.SaveStarts;
        try
        {
            a = OpAmpClient.Start(options, new OpAmpStatus(), Notify, new Seams { Files = faulty, Tap = tapA });

            // The fault comes, and the client goes on: it heartbeats, and the next save after a
            // failed one is stored.
            if (!await UntilAsync(() => faulty.Fired && faulty.Done))
            {
                throw new InvalidOperationException($"the fault never came ({faulty.Saves} saves)");
            }

            var after = Volatile.Read(ref messages);
            if (!await UntilAsync(() => Volatile.Read(ref messages) >= after + 3))
            {
                throw new InvalidOperationException("no heartbeats after the fault");
            }

            if (faulty.SlowOnPool)
            {
                throw new InvalidOperationException("a slow save held a thread of the pool the client runs on");
            }

            if (faulty.SlowFrom is { } from && faulty.SlowTo is { } to && to - from < 2)
            {
                throw new InvalidOperationException($"heartbeats held up by a slow save: {to - from} messages while it waited {SlowHold.TotalSeconds} s for two");
            }

            if (!await UntilAsync(() => Answered(Records(tapA), Input.KindOneofCase.Stored) > faulty.StoredBefore))
            {
                throw new InvalidOperationException("no save stored after the fault");
            }

            if (plan.Download != DownloadFault.None && !await UntilAsync(() => faulty.DownloadFaulted))
            {
                throw new InvalidOperationException("the download fault never came");
            }

            if (plan.Download == DownloadFault.None && !await UntilAsync(() => !installs.IsEmpty))
            {
                throw new InvalidOperationException("no package installed");
            }
        }
        finally
        {
            clock.Restart();
            if (a is not null)
            {
                await a.StopAsync().WaitAsync(TimeSpan.FromSeconds(30));
            }

            clock.Stop();
            StateFile.Saving -= faulty.SaveStarts;
        }

        // Stop's bound: the stop timeout (the goodbye), the work let go within it, and a save
        // still writing.
        var bound = StopTimeout * 2 + TimeSpan.FromMilliseconds(plan.Kind == Kind.Slow ? plan.SlowMs : 0) + TimeSpan.FromSeconds(3);
        if (clock.Elapsed > bound)
        {
            throw new InvalidOperationException($"stop took {clock.ElapsedMilliseconds} ms, over {bound.TotalMilliseconds} ms");
        }

        StateFile.Settled(state);
        var records = TapKit.Read(tapA);
        foreach (var r in records.Where(r => r.Kind is 3 or FileJournal.Kind))
        {
            log.Enqueue("a: " + r);
        }

        CheckAnswers(records);
        var stores = TapKit.Stores(records);
        var content = File.Exists(state) ? await File.ReadAllBytesAsync(state) : null;
        var version = content is null ? -1 : stores.FindLastIndex(x => x.Data.AsSpan().SequenceEqual(content));
        if (content is not null && version < 0)
        {
            throw new InvalidOperationException($"the state file holds {content.Length} bytes that are no whole save");
        }

        var stored = TapKit.Inputs(records).Where(i => i.Input.KindCase == Input.KindOneofCase.Stored)
            .Select(i => stores.FindIndex(x => x.Id == i.Input.Stored.Id_)).DefaultIfEmpty(-1).Max();
        if (version < stored)
        {
            throw new InvalidOperationException($"the state file holds save {version}, older than save {stored}, which was stored");
        }

        if (TapKit.Check(tapA) is { Code: not 0 } verdict)
        {
            throw new InvalidOperationException($"opamp-tap flagged the first client's tap:\n{verdict.Output}");
        }

        // The next client, with a temporary file left as a crash leaves it: it starts from what
        // was saved, saves, and the downloads left are gone.
        var leftovers = Directory.Exists(Executor.DownloadsDir(state)) ? Directory.GetFiles(Executor.DownloadsDir(state)) : [];
        await File.WriteAllBytesAsync(state + ".tmp", Encoding.ASCII.GetBytes("half a save"));
        var b = OpAmpClient.Start(options, new OpAmpStatus(), null, new Seams { Tap = tapB });
        try
        {
            if (!await UntilAsync(() => Answered(Records(tapB), Input.KindOneofCase.Stored) > 0))
            {
                throw new InvalidOperationException("the next client saved nothing");
            }
        }
        finally
        {
            await b.StopAsync().WaitAsync(TimeSpan.FromSeconds(30));
        }

        var next = TapKit.Read(tapB);
        var saved = Pydantic.OpAmp.Core.Config.Parser.ParseFrom(next.First(r => r.Kind == 1).Bytes).SavedState.ToByteArray();
        if (!saved.AsSpan().SequenceEqual(content ?? []))
        {
            throw new InvalidOperationException("the next client didn't start from the state file");
        }

        if (leftovers.FirstOrDefault(File.Exists) is { } left)
        {
            throw new InvalidOperationException($"a download left by the first client is still there: {left}");
        }

        if (TapKit.Check(tapB) is { Code: not 0 } v2)
        {
            throw new InvalidOperationException($"opamp-tap flagged the next client's tap:\n{v2.Output}");
        }

        if (Environment.GetEnvironmentVariable("OPAMP_KEEP_FILES") is not { Length: > 0 })
        {
            Directory.Delete(dir, recursive: true); // else kept: to check the taps elsewhere
        }
    }

    /// <summary>Every save answered at most once; all answered or cancelled but the last, if still writing at stop.</summary>
    private static void CheckAnswers(List<TapRecord> records)
    {
        var answers = TapKit.Inputs(records)
            .Select(i => i.Input.KindCase switch
            {
                Input.KindOneofCase.Stored => i.Input.Stored.Id_,
                Input.KindOneofCase.StoreFailed => i.Input.StoreFailed.Id,
                _ => (ulong?)null,
            })
            .OfType<ulong>()
            .GroupBy(id => id)
            .ToDictionary(g => g.Key, g => g.Count());
        var cancelled = TapKit.Effects(records).Where(e => e.Effect.KindCase == Effect.KindOneofCase.Cancel).Select(e => e.Effect.Cancel.Id_).ToHashSet();
        var stores = TapKit.Stores(records);
        for (var i = 0; i < stores.Count; i++)
        {
            var count = answers.GetValueOrDefault(stores[i].Id);
            if (count > 1)
            {
                throw new InvalidOperationException($"store {stores[i].Id} answered {count} times");
            }

            if (count == 0 && !cancelled.Contains(stores[i].Id) && i != stores.Count - 1)
            {
                throw new InvalidOperationException($"store {stores[i].Id} never answered, though later saves were");
            }
        }
    }

    internal sealed record Plan(Kind Kind, int Save, string Op, int SlowMs, DownloadFault Download)
    {
        public override string ToString() =>
            $"{this.Kind} on the {this.Op} of save {this.Save}{(this.Kind == Kind.Slow ? $" ({this.SlowMs} ms)" : string.Empty)}, download: {this.Download}";
    }

    /// <summary>The real file system, with the plan's faults.</summary>
    private sealed class FaultyFiles(Plan plan, string state, ConcurrentQueue<string> log, Func<int> messages) : IFileOps
    {
        private readonly IFileOps real = FileOps.Real;
        private readonly string full = Path.GetFullPath(state);
        private readonly string downloads = Path.GetFullPath(Executor.DownloadsDir(state));
        private int save = -1;
        private int removes;
        private int fired;
        private int done;
        private int downloadFaulted;

        /// <summary>Whether the fault came.</summary>
        public bool Fired => Volatile.Read(ref this.fired) == 1;

        /// <summary>Whether the save it came in is over.</summary>
        public bool Done => Volatile.Read(ref this.done) == 1;

        public bool DownloadFaulted => Volatile.Read(ref this.downloadFaulted) == 1;

        public Plan Plan => plan;

        public int Saves => Volatile.Read(ref this.save) + 1;

        /// <summary>The server's messages as a slow operation started and ended.</summary>
        public int? SlowFrom { get; private set; }

        public int? SlowTo { get; private set; }

        /// <summary>Whether the slow operation ran on a thread of the pool.</summary>
        public bool SlowOnPool { get; private set; }

        /// <summary>Saves stored before the faulted one started (from the tap, by the test).</summary>
        public int StoredBefore { get; private set; }

        public void SaveStarts(string path)
        {
            if (Path.GetFullPath(path) != this.full)
            {
                return;
            }

            if (this.Fired)
            {
                Volatile.Write(ref this.done, 1); // the faulted save is over: saves are one at a time
            }

            var n = Interlocked.Increment(ref this.save);
            this.removes = 0;
            if (n == plan.Save)
            {
                // Saves stored so far, in order: the next stored is a later save.
                this.StoredBefore = n;
            }
        }

        public IWritingFile CreateNew(string path, bool download)
        {
            this.Fault("create", path);
            return new FaultyFile(this, this.real.CreateNew(path, download), path, download);
        }

        public void Rename(string from, string to)
        {
            this.Fault("rename", to);
            this.real.Rename(from, to);
        }

        public bool SyncDir(string dir)
        {
            this.Fault("syncdir", dir);
            return this.real.SyncDir(dir);
        }

        public bool Remove(string path)
        {
            if (this.IsDownload(path))
            {
                if (plan.Download == DownloadFault.DiscardFails && File.Exists(path))
                {
                    Volatile.Write(ref this.downloadFaulted, 1);
                    log.Enqueue($"fault: removing {path} fails");
                    throw new IOException("EIO: removing the download");
                }

                return this.real.Remove(path);
            }

            if (this.removes++ == 0)
            {
                this.Fault("remove", path);
            }
            else if (plan.Kind == Kind.LeftOver && this.save == plan.Save)
            {
                log.Enqueue("fault: the temporary file can't be removed either");
                throw new IOException("EIO: removing the temporary file");
            }

            return this.real.Remove(path);
        }

        public bool MakeDir(string dir)
        {
            if (plan.Download == DownloadFault.NoDirectory)
            {
                Volatile.Write(ref this.downloadFaulted, 1);
                log.Enqueue("fault: the downloads directory can't be made");
                throw new IOException("EACCES: making the downloads directory");
            }

            return this.real.MakeDir(dir);
        }

        public bool Clear(string dir) => this.real.Clear(dir);

        private bool IsDownload(string path) => Path.GetFullPath(path).StartsWith(this.downloads + Path.DirectorySeparatorChar, StringComparison.Ordinal);

        /// <summary>
        /// Before operation <paramref name="op"/> of a save: the plan's fault, if this is its
        /// save and operation. Slow: sleeps, then goes on; the others throw.
        /// </summary>
        private void Fault(string op, string path)
        {
            if (this.IsDownload(path) || op != plan.Op || Volatile.Read(ref this.save) != plan.Save || this.Fired)
            {
                return;
            }

            Volatile.Write(ref this.fired, 1);
            log.Enqueue($"fault: {plan.Kind} on {op} of save {plan.Save}");
            switch (plan.Kind)
            {
                case Kind.Slow:
                    // Slow for its time, and then until the client has sent two messages since it
                    // began: in a busy process heartbeats can be late for reasons of their own,
                    // but they come, unless the save holds them up. Only then does it give up.
                    this.SlowOnPool = Thread.CurrentThread.IsThreadPoolThread;
                    var from = this.SlowFrom = messages();
                    Thread.Sleep(plan.SlowMs);
                    for (var waited = Stopwatch.StartNew(); messages() < from + 2 && waited.Elapsed < SlowHold;)
                    {
                        Thread.Sleep(10);
                    }

                    this.SlowTo = messages();
                    return;
                case Kind.HalfWrite:
                    return; // in the write itself
                default:
                    throw new IOException($"EIO: {op}");
            }
        }

        private sealed class FaultyFile(FaultyFiles files, IWritingFile file, string path, bool download) : IWritingFile
        {
            public void Write(ReadOnlySpan<byte> data)
            {
                files.Fault("write", path);
                if (!download && files.Fired && !files.Done && files.Plan.Kind == Kind.HalfWrite && Volatile.Read(ref files.save) == files.Plan.Save)
                {
                    file.Write(data[..(data.Length / 2)]);
                    throw new IOException("ENOSPC: no space left on device");
                }

                file.Write(data);
            }

            public ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct) => file.WriteAsync(data, ct);

            public void Sync()
            {
                files.Fault("sync", path);
                file.Sync();
            }

            public void Dispose() => file.Dispose();
        }
    }
}
