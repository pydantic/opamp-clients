// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The flush of a directory (RULE-SHELL-09: a save answered stored is on disk). On macOS fsync(2)
/// leaves the entries in the drive's cache: fcntl(F_FULLFSYNC) writes them out, with fsync the
/// fallback where the file system refuses it. Played on any host with stand-ins for the C library.
/// </summary>
public sealed class DirSyncTests
{
    private const int Fd = 7;

    [Fact]
    public void OnMacOsADirectoryIsFlushedWithFullFsync()
    {
        var calls = new List<string>();
        Assert.True(FileOps.Posix.SyncDir("/d", Lib(calls, fullFsync: 0)));
        Assert.Equal(["open /d", "F_FULLFSYNC", "close"], calls);
    }

    [Fact]
    public void OnMacOsAFileSystemWithoutFullFsyncIsFlushedWithFsync()
    {
        var calls = new List<string>();
        Assert.True(FileOps.Posix.SyncDir("/d", Lib(calls, fullFsync: -1)));
        Assert.Equal(["open /d", "F_FULLFSYNC", "fsync", "close"], calls);
    }

    [Fact]
    public void ElsewhereADirectoryIsFlushedWithFsync()
    {
        var calls = new List<string>();
        Assert.True(FileOps.Posix.SyncDir("/d", Lib(calls, fullFsync: null)));
        Assert.Equal(["open /d", "fsync", "close"], calls);
    }

    [Fact]
    public void AFailedFsyncAfterAFailedFullFsyncFailsTheFlush()
    {
        var calls = new List<string>();
        Assert.Throws<IOException>(() => FileOps.Posix.SyncDir("/d", Lib(calls, fullFsync: -1, fsync: -1)));
        Assert.Equal(["open /d", "F_FULLFSYNC", "fsync", "close"], calls);
    }

    /// <summary>
    /// A C library that notes its calls: fcntl answers <paramref name="fullFsync"/> to F_FULLFSYNC
    /// (null: there is none, as off macOS), fsync <paramref name="fsync"/>.
    /// </summary>
    private static FileOps.Posix.Fns Lib(List<string> calls, int? fullFsync, int fsync = 0) => new(
        (path, flags) =>
        {
            calls.Add($"open {path}");
            return Fd;
        },
        fd =>
        {
            Assert.Equal(Fd, fd);
            calls.Add("fsync");
            return fsync;
        },
        fd =>
        {
            Assert.Equal(Fd, fd);
            calls.Add("close");
            return 0;
        },
        fullFsync is { } answer
            ? (fd, cmd) =>
            {
                Assert.Equal(Fd, fd);
                Assert.Equal(FileOps.Posix.FFullFsync, cmd);
                calls.Add("F_FULLFSYNC");
                return answer;
            }
            : null);
}
