// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Security.Cryptography;
using System.Text;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Downloads follow no redirect: whatever the status, the body goes to the file, hashed, and the
/// status goes to the core, with a redirect's Location resolved by .NET's URL library and its origin.
/// </summary>
public class DownloadTests
{
    private static readonly byte[] Body = Encoding.ASCII.GetBytes(string.Concat(Enumerable.Repeat("package", 100)));

    [Fact]
    public async Task EveryStatusIsReportedWithItsBodyHashedAndNoRedirectFollowed()
    {
        var gets = new ConcurrentQueue<string>();
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                var path = line.Split(' ')[1];
                gets.Enqueue(path);
                var (status, location, body) = path switch
                {
                    "/file" when headers.GetValueOrDefault("Authorization") == "t" => ("200 OK", null, Body),
                    "/moved" => ("302 Found", "/file", Encoding.ASCII.GetBytes("moved")),
                    "/a/b/dots" => ("301 Moved Permanently", "../c/./d/../file?x=1", Array.Empty<byte>()),
                    "/other" => ("307 Temporary Redirect", "https://other.example:8443/x", Array.Empty<byte>()),
                    "/default" => ("308 Permanent Redirect", "https://other.example:443/y", Array.Empty<byte>()),
                    "/v6" => ("302 Found", "http://[::1]:9/z", Array.Empty<byte>()),
                    "/scheme" => ("302 Found", "//cdn.example/f", Array.Empty<byte>()),
                    "/bad" => ("302 Found", "http://[bad/", Array.Empty<byte>()),
                    "/none" => ("302 Found", null, Array.Empty<byte>()),
                    "/ok-with-location" => ("200 OK", "/elsewhere", Body),
                    _ => ("404 Not Found", null, Encoding.ASCII.GetBytes("no")),
                };
                var head = $"HTTP/1.1 {status}\r\nContent-Length: {body.Length}\r\n" + (location is null ? string.Empty : $"Location: {location}\r\n") + "\r\n";
                await Http1.WriteAsync(c.Stream, head, body);
            }
        });
        var loop = TestLoop.New();
        var state = loop.State!;
        ulong next = 0;
        Input Get(string path, ulong max = 1 << 20)
        {
            var d = new Download { Id = ++next, Url = server.Url("http", path), MaxBytes = max };
            d.Headers.Add(new Header { Name = "Authorization", Value = "t" });
            loop.Run(new Effect { Download = d });
            var input = loop.Next();
            Assert.Equal(next, input.DownloadResponse?.Id ?? input.DownloadFailed.Id);
            return input;
        }

        DownloadResponse Response(string path, uint status, byte[] body)
        {
            var r = Get(path).DownloadResponse;
            Assert.NotNull(r);
            Assert.Equal(status, r.Status);
            Assert.Equal(SHA256.HashData(body), r.Sha256.ToByteArray());
            Assert.Equal(body, File.ReadAllBytes(loop.X.DownloadPath(next)));
            return r;
        }

        var root = $"http://127.0.0.1:{server.Port}";
        var ok = Response("/file", 200, Body);
        Assert.Equal(string.Empty, ok.Location);
        Assert.Null(ok.LocationOrigin);
        loop.Run(new Effect { Discard = new Id { Id_ = next } });
        Assert.False(File.Exists(loop.X.DownloadPath(next)));

        // Relative, and with dot segments: resolved against the URL requested.
        var moved = Response("/moved", 302, Encoding.ASCII.GetBytes("moved"));
        Assert.Equal($"{root}/file", moved.Location);
        Assert.Equal(new Origin { Scheme = "http", Host = "127.0.0.1", Port = (uint)server.Port }, moved.LocationOrigin);
        Assert.Equal($"{root}/a/c/file?x=1", Response("/a/b/dots", 301, []).Location);

        // Absolute, to another origin; the scheme's default port is 0; IPv6; scheme-relative.
        var other = Response("/other", 307, []);
        Assert.Equal("https://other.example:8443/x", other.Location);
        Assert.Equal(new Origin { Scheme = "https", Host = "other.example", Port = 8443 }, other.LocationOrigin);
        var dflt = Response("/default", 308, []);
        Assert.Equal("https://other.example/y", dflt.Location);
        Assert.Equal(new Origin { Scheme = "https", Host = "other.example", Port = 0 }, dflt.LocationOrigin);
        Assert.Equal(new Origin { Scheme = "http", Host = "[::1]", Port = 9 }, Response("/v6", 302, []).LocationOrigin);
        Assert.Equal(new Origin { Scheme = "http", Host = "cdn.example", Port = 0 }, Response("/scheme", 302, []).LocationOrigin);

        // One that doesn't resolve goes as given, with no origin; none, or not a 3xx: neither.
        var bad = Response("/bad", 302, []);
        Assert.Equal("http://[bad/", bad.Location);
        Assert.Null(bad.LocationOrigin);
        var none = Response("/none", 302, []);
        Assert.Equal(string.Empty, none.Location);
        Assert.Null(none.LocationOrigin);
        Assert.Equal(string.Empty, Response("/ok-with-location", 200, Body).Location);

        // Any status: the core decides what it means.
        Response("/missing", 404, Encoding.ASCII.GetBytes("no"));

        // Too large, or no server: failed, and nothing left.
        Assert.Equal(Input.KindOneofCase.DownloadFailed, Get("/file", 10).KindCase);
        Assert.False(File.Exists(loop.X.DownloadPath(next)));
        Assert.DoesNotContain(gets, g => g is "/elsewhere" or "/x" or "/y");

        // What was downloaded before a restart goes.
        await File.WriteAllBytesAsync(loop.X.DownloadPath(99), new byte[] { 1 });
        Executor.DeleteDownloads(state);
        Assert.False(Directory.Exists(Executor.DownloadsDir(state)));
    }

    // A download's blocking file work (making its file, the flush to the disk, deleting what a
    // failed one left) runs on a thread of its own, as a save is written: on one of the pool's,
    // for as long as the disk took, it kept a thread the loop, transport and timers need.
    // covers: RULE-SHELL-07
    [Fact]
    public async Task ADownloadsFileWorkHoldsNoThreadOfThePool()
    {
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                await c.ReadHeadAsync();
                await Http1.WriteAsync(c.Stream, $"HTTP/1.1 200 OK\r\nContent-Length: {Body.Length}\r\n\r\n", Body);
            }
        });
        var files = new HeldFiles(FileOps.Real);
        var loop = TestLoop.New(files);
        loop.Run(new Effect { Download = new Download { Id = 1, Url = server.Url("http", "/file"), MaxBytes = 1 << 20 } });
        Assert.Equal(1u, loop.Next().DownloadResponse.Id);
        loop.Run(new Effect { Download = new Download { Id = 2, Url = server.Url("http", "/file"), MaxBytes = 10 } });
        Assert.Equal(2u, loop.Next().DownloadFailed.Id); // over its size: the file it made goes
        Assert.False(File.Exists(loop.X.DownloadPath(2)));
        Assert.Equal(["create 1", "sync 1", "create 2", "remove 2 True"], files.Log.Select(l => l.Replace(Path.GetFileName(loop.X.DownloadPath(1)), "1").Replace(Path.GetFileName(loop.X.DownloadPath(2)), "2")).Where(l => !l.StartsWith("write", StringComparison.Ordinal) && !l.StartsWith("close", StringComparison.Ordinal)));
        Assert.Empty(files.OnPool);
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    [Fact]
    public async Task ADownloadWithNoServerFailsAndLeavesNothing()
    {
        var loop = TestLoop.New();
        loop.Run(new Effect { Download = new Download { Id = 1, Url = "http://127.0.0.1:1/file", MaxBytes = 10 } });
        Assert.Equal(Input.KindOneofCase.DownloadFailed, loop.Next().KindCase);
        Assert.False(File.Exists(loop.X.DownloadPath(1)));
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    [Fact]
    public async Task StopWaitsForCancelledDownloadsToLetGoOfTheirFiles()
    {
        // S3: nothing of a download runs on after stop to touch the next client's files.
        using var server = new RawServer(async c =>
        {
            await c.ReadHeadAsync();
            await Http1.WriteAsync(c.Stream, "HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n", new byte[400_000]);
            await Task.Delay(Timeout.Infinite);
        });
        var loop = TestLoop.New();
        var file = loop.X.DownloadPath(1);
        loop.Run(new Effect { Download = new Download { Id = 1, Url = server.Url("http", "/file"), MaxBytes = 1 << 20 } });
        Assert.True(await Wait.ForAsync(() => File.Exists(file) && new FileInfo(file).Length > 0));

        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));

        Assert.False(File.Exists(file));
        StateFile.Settled(loop.State!); // nothing left under way on the state file's files
        Assert.Empty(loop.Drain());

        // Another client's download of the same id has a file of its own.
        Assert.NotEqual(Path.GetFileName(file), Path.GetFileName(new TestLoop(ClientTransport.Create(null, null, direct: true), loop.State).X.DownloadPath(1)));
    }
}
