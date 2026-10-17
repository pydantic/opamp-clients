// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Runtime.Versioning;
using System.Security.AccessControl;
using System.Security.Principal;
using Google.Protobuf;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

public sealed class StateFileTests : IDisposable
{
    private readonly string dir = Directory.CreateTempSubdirectory("opamp-state-").FullName;

    public void Dispose() => Directory.Delete(this.dir, recursive: true);

    [Fact]
    public void SavesReplaceWhatWasSavedPrivately()
    {
        var path = Path.Combine(this.dir, "state.bin");
        Assert.Empty(StateFile.Load(path));
        StateFile.Save(path, [1]);
        StateFile.Save(path, [2, 3]);
        Assert.Equal(new byte[] { 2, 3 }, StateFile.Load(path));
        Assert.False(File.Exists(path + ".tmp"));
        if (!OperatingSystem.IsWindows())
        {
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(path));
        }
    }

    [Fact]
    public void ALeftoverTemporaryFileIsReplacedNotReused()
    {
        var path = Path.Combine(this.dir, "state.bin");
        File.WriteAllBytes(path + ".tmp", [9, 9, 9]);
        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(path + ".tmp", UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.GroupRead | UnixFileMode.OtherRead);
        }

        StateFile.Save(path, [1]);
        Assert.Equal(new byte[] { 1 }, StateFile.Load(path));
        if (!OperatingSystem.IsWindows())
        {
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(path));
        }
    }

    [Fact]
    public void DownloadsArePrivate()
    {
        var path = new TestLoop(ClientTransport.Create(null, null), Path.Combine(this.dir, "state.bin")).X.DownloadPath(3);
        Assert.True(StateFile.MakePrivateDirectory(Path.GetDirectoryName(path)!));
        Assert.False(StateFile.MakePrivateDirectory(Path.GetDirectoryName(path)!));
        StateFile.CreateDownload(path).Dispose();
        Assert.True(File.Exists(path));

        // Made new: never one already there.
        Assert.Throws<IOException>(() => StateFile.CreateDownload(path).Dispose());
        if (OperatingSystem.IsWindows())
        {
            AllowsTheUserAlone(new DirectoryInfo(Path.GetDirectoryName(path)!).GetAccessControl(), inherited: false);
            AllowsTheUserAlone(new FileInfo(path).GetAccessControl(), inherited: true);
        }
        else
        {
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute, File.GetUnixFileMode(Path.GetDirectoryName(path)!));
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(path));
        }
    }

    /// <summary>Windows: the state file's list allows the user alone, and the directory's doesn't apply.</summary>
    [Fact]
    public void OnWindowsTheStateFileAllowsTheUserAlone()
    {
        if (!OperatingSystem.IsWindows())
        {
            Assert.Skip("Windows access-control lists");
            return;
        }

        var path = Path.Combine(this.dir, "state.bin");
        StateFile.Save(path, [1]);
        AllowsTheUserAlone(new FileInfo(path).GetAccessControl(), inherited: false);
    }

    [SupportedOSPlatform("windows")]
    private static void AllowsTheUserAlone(FileSystemSecurity security, bool inherited)
    {
        var rules = security.GetAccessRules(includeExplicit: true, includeInherited: true, typeof(SecurityIdentifier))
            .Cast<FileSystemAccessRule>().ToList();
        var rule = Assert.Single(rules);
        Assert.Equal(StateFile.CurrentUser(), rule.IdentityReference);
        Assert.Equal(AccessControlType.Allow, rule.AccessControlType);
        Assert.Equal(FileSystemRights.FullControl, rule.FileSystemRights & FileSystemRights.FullControl);
        Assert.Equal(inherited, rule.IsInherited);
        // A file inherits from its (protected) directory; the directory and the state file inherit nothing.
        Assert.Equal(!inherited, security.AreAccessRulesProtected);
    }

    [Fact]
    public void AStoreIsAnswered()
    {
        var got = new List<Input>();
        using var http = ClientTransport.Create(null, null);
        string?[] paths = [Path.Combine(this.dir, "state.bin"), Path.Combine(this.dir, "missing", "state.bin"), null];
        foreach (var p in paths)
        {
            var store = new Store { Id = (ulong)got.Count + 7, Data = ByteString.CopyFromUtf8("x") };
            var loop = new TestLoop(http, p);
            loop.Run(new Effect { Store = store });
            got.Add(loop.Next());
        }

        Assert.Equal(7UL, got[0].Stored.Id_);
        Assert.Equal(8UL, got[1].StoreFailed.Id);
        Assert.Contains("state.bin", got[1].StoreFailed.Detail);
        Assert.Equal(9UL, got[2].StoreFailed.Id);
    }

    // The client's loop, its transport and its timers all run on the thread pool. A save written
    // on a thread of the pool kept that thread for as long as the disk took: in a busy process,
    // with no other thread to spare, the heartbeats waited for the disk (CI, twice: "heartbeats
    // held up by a slow save: 0 messages"). A save writes on a thread of its own.
    // covers: RULE-SHELL-07
    [Fact]
    public async Task ASaveWritesOnAThreadOfItsOwnNotOneOfThePools()
    {
        var path = Path.Combine(this.dir, "state.bin");
        var full = Path.GetFullPath(path);
        bool? onPool = null;
        void Saving(string p)
        {
            if (Path.GetFullPath(p) == full)
            {
                onPool = Thread.CurrentThread.IsThreadPoolThread;
            }
        }

        StateFile.Saving += Saving;
        try
        {
            Exception? failed = new InvalidOperationException("not answered");
            await StateFile.SaveBeside(path, [1, 2, 3], () => true, e => failed = e).WaitAsync(TimeSpan.FromSeconds(10));
            Assert.Null(failed);
        }
        finally
        {
            StateFile.Saving -= Saving;
        }

        Assert.Equal(false, onPool);
        Assert.Equal([1, 2, 3], File.ReadAllBytes(path));
    }

    [Fact]
    public void AnUnreadableStateFileStopsTheClientStarting()
    {
        var options = new OpAmpClientOptions
        {
            Mode = OpAmpMode.DotNet,
            Endpoint = new Uri("http://127.0.0.1:1"),
            InstanceUid = new byte[16],
            StateFile = this.dir,
        };
        Assert.ThrowsAny<Exception>(() => OpAmpClient.Start(options, new OpAmpStatus()));
    }
}
