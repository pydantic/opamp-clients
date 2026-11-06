// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using OpAmp.Proto.V1;
using Logfire.Fleet.Core;
using Xunit;

namespace Logfire.Fleet.Client.Tests;

/// <summary>
/// The file operations of saves, as the core tap records them: the recipe of a save (a temporary
/// file made new, written, flushed, renamed over the state file, the directory flushed, and only
/// then stored), checked by opamp-tap on a disk that can crash at any point.
/// </summary>
public class FileJournalTests
{
    private readonly ITestOutputHelper output;

    public FileJournalTests(ITestOutputHelper output) => this.output = output;

    // covers: RULE-SHELL-09
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task ASaveIsJournaledAndTheCheckerHoldsIt(OpAmpMode mode)
    {
        var dir = Directory.CreateTempSubdirectory("opamp-journal-").FullName;
        var state = Path.Combine(dir, "state");
        var tap = Path.Combine(dir, "core.tap");

        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (_, headers) = await c.ReadHeadAsync();
                await c.AnswerAsync(headers, TapKit.ConfigOffer);
            }
        });

        var configs = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Url("http")),
                HeartbeatInterval = TimeSpan.FromMilliseconds(200),
                StateFile = state,
                Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig,
                Mode = mode, DirectConnection = mode == OpAmpMode.Core,
            },
            new OpAmpStatus(),
            n =>
            {
                if (n is OpAmpNotification.RemoteConfig)
                {
                    configs.TrySetResult();
                }
            },
            new Seams { Tap = tap });
        await configs.Task.WaitAsync(TimeSpan.FromSeconds(20));
        await Task.Delay(300); // the save of the remote config
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        var records = TapKit.Read(tap);
        foreach (var r in records.Where(r => r.Kind is 3 or FileJournal.Kind))
        {
            this.output.WriteLine(r.ToString());
        }

        var files = TapKit.Files(records);
        Assert.Equal("state", files[0].Field("op"));
        Assert.Equal("state", files[0].Field("path"));
        Assert.True(records.Take(files[0].At).All(r => r.Kind is 1 or 2), "the state file is named right after init");

        // The first save.
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
        Assert.DoesNotContain(files, f => f.Field("op") is "open" or "mkdir");

        this.CheckTap(tap);
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
