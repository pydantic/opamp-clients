// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Text.RegularExpressions;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Xunit;
using Header = Pydantic.OpAmp.Core.Header;

namespace Pydantic.OpAmp.Client.Tests;

public class CoreTests
{
    private static readonly Input Start = new() { Start = new Empty() };
    private static readonly Input Timer = new() { Timer = new Empty() };

    private static ByteString Uid(byte b) => ByteString.CopyFrom(Enumerable.Repeat(b, 16).ToArray());

    [Fact]
    public void StartSendsTheFirstReportAndHonoursARetryAfterDate()
    {
        using var core = new CoreInstance(new Config
        {
            InstanceUid = Uid(7),
            Endpoint = "http://server/v1/opamp",
        });
        var output = core.Step(0, 0, Start);
        var request = Assert.Single(output.Effects).HttpRequest;
        Assert.Equal("http://server/v1/opamp", request.Url);
        var msg = AgentToServer.Parser.ParseFrom(request.Body);
        Assert.Equal(Uid(7), msg.InstanceUid);
        Assert.NotEqual(0UL, msg.Capabilities & 1);
        Assert.True(output.HasWakeAtMs);

        // A 503 with Retry-After as a date, 30 s after the response came: nothing before then, and
        // the next request within the client's own spread after it (a quarter of the wait: 7.5 s).
        var came = new DateTimeOffset(2026, 9, 26, 12, 0, 0, TimeSpan.Zero);
        var busy = new HttpResponse { Id = request.Id, Status = 503, UnixMs = (ulong)came.ToUnixTimeMilliseconds() };
        busy.Headers.Add(new Header { Name = "Retry-After", Value = came.AddSeconds(30).ToString("r") });
        core.Step(10, 0, new Input { HttpResponse = busy });
        static bool Sends(Output o) => o.Effects.Any(e => e.KindCase == Effect.KindOneofCase.HttpRequest);
        var last = output;
        foreach (var at in new[] { 30_000L, 30_009L })
        {
            last = core.Step(at, 0, Timer);
            Assert.False(Sends(last), $"at {at} ms");
        }
        long now = 30_009;
        for (var i = 0; i < 10 && !Sends(last); i++)
        {
            var wake = (long)last.WakeAtMs;
            Assert.True(wake > now, $"wake at {wake} ms");
            Assert.False(Sends(core.Step(wake - 1, 0, Timer)), $"at {wake - 1} ms");
            last = core.Step(wake, 0, Timer);
            now = wake;
        }
        Assert.True(Sends(last) && now >= 30_010 && now <= 30_010 + 7_500, $"sent at {now} ms");
    }

    [Fact]
    public void ABadConfigIsAnError() =>
        Assert.Throws<CoreException>(() => new CoreInstance(new Config { InstanceUid = ByteString.CopyFrom(1, 2) }));

    [Fact]
    public void CoresAreIndependent()
    {
        using var a = new CoreInstance(new Config { InstanceUid = Uid(1), Endpoint = "http://server/v1/opamp" });
        using var b = new CoreInstance(new Config { InstanceUid = Uid(2), Endpoint = "http://server/v1/opamp" });
        Assert.Equal(1, a.Step(0, 0, Start).InstanceUid[0]);
        Assert.Equal(2, b.Step(0, 0, Start).InstanceUid[0]);
    }

    // The core tap: every call into the core, as it crossed, for the effects checker.
    [Fact]
    public void TheTapRecordsEveryCrossing()
    {
        var dir = Directory.CreateTempSubdirectory();
        var path = Path.Combine(dir.FullName, "core.tap");
        string? Tap(string name) => name == "OPAMP_CORE_TAP" ? path : null;
        var config = new Config { InstanceUid = Uid(3), Endpoint = "http://server/v1/opamp" };
        // The process's first tap takes the path as it is; a later one, `<path>.<pid>.<n>`. Other
        // tests tap in parallel (each in a directory of its own), so which this one is, and its
        // n, are found from the directory rather than from CoreTap.Count read beforehand.
        string[] Taps() => dir.GetFiles("core.tap*").Select(f => f.FullName).Order().ToArray();
        bool Later(string name) => Regex.IsMatch(name, $@"^{Regex.Escape(path)}\.{Environment.ProcessId}\.[0-9]+$");
        using (var core = new CoreInstance(config, Tap))
        {
            core.Step(5, 1234, Start);
        }

        var name = Assert.Single(Taps());
        Assert.True(name == path || Later(name), name);
        var records = Records(File.ReadAllBytes(name));
        Assert.Equal(new byte[] { 1, 2, 3, 4 }, records.Select(r => r.Kind).ToArray());
        Assert.Equal(config, Config.Parser.ParseFrom(records[0].Bytes));
        Assert.Equal(new Step { NowMs = 5, Input = Start, UnixMs = 1234 }, Step.Parser.ParseFrom(records[2].Bytes));
        Assert.Equal(Uid(3), Reply.Parser.ParseFrom(records[1].Bytes).Output.InstanceUid);
        Assert.NotEmpty(Reply.Parser.ParseFrom(records[3].Bytes).Output.Effects);
        Assert.True(records[0].Nanos <= records[1].Nanos && records[1].Nanos <= records[2].Nanos && records[2].Nanos <= records[3].Nanos);

        // A later core in the same process: a file of its own.
        using (var second = new CoreInstance(config, Tap))
        {
        }

        var later = Assert.Single(Taps().Where(t => t != name));
        Assert.True(Later(later), later);
        Assert.Equal(2, Records(File.ReadAllBytes(later)).Count); // init in and out

        // A tap that can't be opened: the core runs without it.
        using (var core = new CoreInstance(config, _ => Path.Combine(dir.FullName, "missing", "x.tap")))
        {
            core.Step(0, 0, Start);
        }

        // Unset: no file.
        var before = dir.GetFiles().Length;
        using (var core = new CoreInstance(config, _ => null))
        {
            core.Step(0, 0, Start);
        }

        using (var core = new CoreInstance(config, _ => string.Empty))
        {
            core.Step(0, 0, Start);
        }

        Assert.Equal(before, dir.GetFiles().Length);
    }

    private static List<(byte Kind, ulong Nanos, byte[] Bytes)> Records(byte[] file)
    {
        var records = new List<(byte, ulong, byte[])>();
        for (var at = 0; at < file.Length;)
        {
            var len = (int)System.Buffers.Binary.BinaryPrimitives.ReadUInt32LittleEndian(file.AsSpan(at + 1));
            var nanos = System.Buffers.Binary.BinaryPrimitives.ReadUInt64LittleEndian(file.AsSpan(at + 5));
            records.Add((file[at], nanos, file.AsSpan(at + 13, len).ToArray()));
            at += 13 + len;
        }

        return records;
    }
}
