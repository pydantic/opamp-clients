// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Security.Cryptography;
using System.Text;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The file operations of saves and downloads, as the core tap records them: the recipe of a
/// save (a temporary file made new, written, flushed, renamed over the state file, the directory
/// flushed, and only then stored), of a download's file (made new, written, flushed, and only then
/// answered), checked by opamp-tap on a disk that can crash at any point.
/// </summary>
public class FileJournalTests
{
    private static readonly byte[] Package = Encoding.ASCII.GetBytes("the package, version 1");

    private readonly ITestOutputHelper output;

    public FileJournalTests(ITestOutputHelper output) => this.output = output;

    // covers: RULE-SHELL-09
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task ASaveAndADownloadAreJournaledAndTheCheckerHoldsThem(OpAmpMode mode)
    {
        var dir = Directory.CreateTempSubdirectory("opamp-journal-").FullName;
        var state = Path.Combine(dir, "state");
        var tap = Path.Combine(dir, "core.tap");

        // A file left in the downloads directory by a run that crashed: never used.
        Directory.CreateDirectory(Executor.DownloadsDir(state));
        await File.WriteAllBytesAsync(Path.Combine(Executor.DownloadsDir(state), "leftover"), [1, 2, 3]);

        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                if (line.StartsWith("POST", StringComparison.Ordinal))
                {
                    await c.AnswerAsync(headers, m => TapKit.Offer(m, $"http://127.0.0.1:{((System.Net.IPEndPoint)c.Tcp.Client.LocalEndPoint!).Port}/pkg", Package));
                }
                else
                {
                    await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {Package.Length}\r\n\r\n", Package);
                }
            }
        });

        var installed = new TaskCompletionSource<(string Path, byte[] Hash)>(TaskCreationOptions.RunContinuationsAsynchronously);
        OpAmpClient? client = null;
        client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Url("http")),
                HeartbeatInterval = TimeSpan.FromMilliseconds(200),
                StateFile = state,
                AcceptPackages = true,
                Mode = mode, DirectConnection = mode == OpAmpMode.Core,
            },
            new OpAmpStatus(),
            n =>
            {
                if (n is OpAmpNotification.InstallPackage install)
                {
                    installed.TrySetResult((install.Path, SHA256.HashData(File.ReadAllBytes(install.Path))));
                }
            },
            new Seams { Tap = tap });
        var (path, hash) = await installed.Task.WaitAsync(TimeSpan.FromSeconds(20));
        await Task.Delay(300); // the save of what was downloaded
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        Assert.Equal(SHA256.HashData(Package), hash);
        Assert.False(File.Exists(Path.Combine(Executor.DownloadsDir(state), "leftover")));

        var records = TapKit.Read(tap);
        foreach (var r in records.Where(r => r.Kind is 3 or FileJournal.Kind))
        {
            this.output.WriteLine(r.ToString());
        }

        var files = TapKit.Files(records);
        Assert.Equal("state", files[0].Field("op"));
        Assert.Equal("state", files[0].Field("path"));
        Assert.True(records.Take(files[0].At).All(r => r.Kind is 1 or 2), "the state file is named right after init");

        // The first save: its file records, those of the download left out.
        var (id, data) = TapKit.Stores(records)[0];
        var save = files.Where(f => f.Field("path") is "state.tmp" or "state" or "." || f.Field("op") == "rename").ToList();
        var create = save.FindIndex(f => f.Field("op") == "create");
        Assert.True(create >= 1, "no create of the temporary file");
        Assert.True(create == 1 || (create == 2 && save[1].Field("op") == "remove" && save[1].Field("path") == "state.tmp"), "only a leftover temporary file's removal comes before");
        // Windows can't flush a directory (docs/state-file.md, step 5), so none is journaled there.
        string[] expected = OperatingSystem.IsWindows()
            ? ["create state.tmp", "write state.tmp", "sync state.tmp", "rename state.tmp state"]
            : ["create state.tmp", "write state.tmp", "sync state.tmp", "rename state.tmp state", "syncdir ."];
        var recipe = save.Skip(create).Take(expected.Length).ToList();
        Assert.Equal(
            expected,
            recipe.Select(f => $"{f.Field("op")} {(f.Field("op") == "rename" ? $"{f.Field("from")} {f.Field("to")}" : f.Field("path"))}"));
        Assert.Equal(Convert.ToHexString(data).ToLowerInvariant(), recipe[1].Field("data"));
        var stored = TapKit.Inputs(records).First(i => i.Input.Stored?.Id_ == id).Record;
        Assert.True(stored.At > recipe[^1].At, "stored before the rename was flushed");

        // The download: its directory made, its file made new, written, flushed, then answered.
        var name = "state.downloads/" + Path.GetFileName(path);
        var download = files.Where(f => f.Field("path") == name || f.Field("op") == "mkdir").ToList();
        Assert.Equal("mkdir state.downloads", $"{download[0].Field("op")} {download[0].Field("path")}");
        Assert.Equal("create", download[1].Field("op"));
        var writes = download.Skip(2).TakeWhile(f => f.Field("op") == "write").ToList();
        Assert.Equal(Package.Length, writes.Sum(w => int.Parse(w.Field("len"), System.Globalization.CultureInfo.InvariantCulture)));
        var sync = download[2 + writes.Count];
        Assert.Equal("sync", sync.Field("op"));
        // (core mode: the shell answers the core's download itself; what crosses the tap is the
        // file's downloaded.)
        var answer = mode == OpAmpMode.Core ? Input.KindOneofCase.Downloaded : Input.KindOneofCase.DownloadResponse;
        var response = TapKit.Inputs(records).First(i => i.Input.KindCase == answer).Record;
        Assert.True(response.At > sync.At, "the download was answered before its file was flushed");
        Assert.DoesNotContain(files, f => f.Field("op") == "open");

        this.CheckTap(tap);
    }

    // A file already at the path a download would use (left by a run that crashed) is neither
    // written over nor handed over: a download's file is made new.
    [Fact]
    public async Task AFileAlreadyWhereADownloadGoesIsNeverReused()
    {
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                await c.ReadHeadAsync();
                await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {Package.Length}\r\n\r\n", Package);
            }
        });
        var loop = TestLoop.New();
        var stale = loop.X.DownloadPath(1);
        Directory.CreateDirectory(Path.GetDirectoryName(stale)!);
        await File.WriteAllBytesAsync(stale, Encoding.ASCII.GetBytes("half a package from a run that crashed"));

        loop.Run(new Effect { Download = new Download { Id = 1, Url = server.Url("http", "/pkg"), MaxBytes = 1 << 20 } });
        var input = loop.Next();

        Assert.Equal(Input.KindOneofCase.DownloadFailed, input.KindCase);
        Assert.Equal("half a package from a run that crashed", await File.ReadAllTextAsync(stale));

        // The next download (another id: another file) is made new and handed over whole.
        loop.Run(new Effect { Download = new Download { Id = 2, Url = server.Url("http", "/pkg"), MaxBytes = 1 << 20 } });
        var ok = loop.Next().DownloadResponse;
        Assert.Equal(SHA256.HashData(Package), ok.Sha256.ToByteArray());
        Assert.Equal(Package, await File.ReadAllBytesAsync(loop.X.DownloadPath(2)));
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    // S3, second half: a client stopped mid-download and the next on the same state file, whose
    // download gets the same id. The first's download, let go after the second started, lands on
    // nothing of the second's: the file handed over is the one offered.
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task S3_ADownloadDoesNotOutliveStop(OpAmpMode mode)
    {
        var file = Encoding.ASCII.GetBytes("the package, version 1");
        using var held = new SemaphoreSlim(0);
        using var release = new ManualResetEventSlim();
        var gets = 0;
        var firstAborted = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                if (line.StartsWith("POST", StringComparison.Ordinal))
                {
                    var port = ((System.Net.IPEndPoint)c.Tcp.Client.LocalEndPoint!).Port;
                    await c.AnswerAsync(headers, m => TapKit.Offer(m, $"http://127.0.0.1:{port}/pkg", file));
                    continue;
                }

                if (Interlocked.Increment(ref gets) == 1)
                {
                    held.Release();
                    var ended = c.EndsWithinAsync(TimeSpan.FromSeconds(30));
                    await Task.Run(() => release.Wait(TimeSpan.FromSeconds(30)));
                    if (ended.IsCompleted && await ended)
                    {
                        firstAborted.TrySetResult();
                        return;
                    }

                    var old = Enumerable.Repeat((byte)'O', 300).ToArray();
                    await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {old.Length}\r\n\r\n", old);
                    return;
                }

                await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {file.Length}\r\n\r\n", file);
            }
        });

        var state = Path.Combine(Directory.CreateTempSubdirectory("opamp-s3-").FullName, "state");
        var options = new OpAmpClientOptions
        {
            Endpoint = new Uri(server.Url("http")),
            InstanceUid = Enumerable.Repeat((byte)0x42, 16).ToArray(),
            HeartbeatInterval = TimeSpan.FromHours(1),
            StateFile = state,
            AcceptPackages = true,
            Mode = mode, DirectConnection = mode == OpAmpMode.Core,
        };
        var clock = System.Diagnostics.Stopwatch.StartNew();
        var a = OpAmpClient.Start(options, new OpAmpStatus());
        Assert.True(await held.WaitAsync(TimeSpan.FromSeconds(20)), "no download");
        this.output.WriteLine($"{clock.ElapsedMilliseconds} ms: the first download is held");
        await a.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        this.output.WriteLine($"{clock.ElapsedMilliseconds} ms: the first client stopped");

        var paths = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
        var b = OpAmpClient.Start(options, new OpAmpStatus(), n =>
        {
            if (n is OpAmpNotification.InstallPackage install)
            {
                paths.TrySetResult(install.Path);
            }
        });
        try
        {
            var path = await paths.Task.WaitAsync(TimeSpan.FromSeconds(20));
            this.output.WriteLine($"{clock.ElapsedMilliseconds} ms: the second client's package is handed over");
            release.Set(); // the first client's download, if it is still there, goes on now
            await Task.Delay(300);
            Assert.Equal(SHA256.HashData(file), SHA256.HashData(await File.ReadAllBytesAsync(path)));
            Assert.True(firstAborted.Task.IsCompleted, "the first client's download outlived its stop");
        }
        finally
        {
            release.Set();
            await b.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
    }

    /// <summary>opamp-tap on the tap: it must hold (exit 0), if there is a checker that runs here.</summary>
    private void CheckTap(string tap)
    {
        if (TapKit.Check(tap) is not { } result)
        {
            this.output.WriteLine("opamp-tap: none that runs here (set OPAMP_TAP_BIN, or build target/release/opamp-tap): not checked");
            return;
        }

        Assert.True(result.Code == 0, $"opamp-tap flagged the tap:\n{result.Output}");
    }
}
