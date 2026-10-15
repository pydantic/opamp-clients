// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Runtime.Versioning;
using System.Security.AccessControl;
using System.Security.Principal;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// Where the client keeps the core's saved state: one file, replaced atomically, that only the
/// agent's user can open. It holds secrets: the credentials of connection settings the server
/// offered, and the private keys of telemetry settings' certificates. The package files downloaded
/// beside it are the user's only too.
/// </summary>
/// <remarks>
/// On Unix the file is created with mode 0600 (downloads: a 0700 directory of 0600 files). On
/// Windows it is created, unshared, with an access-control list that allows the user alone and is
/// protected from inheritance, so the directory's entries never apply to it; the downloads
/// directory gets the same list, which the files in it inherit. No entry for SYSTEM or
/// Administrators is added: the client needs none, and administrators can take ownership of a
/// file whatever its list says.
/// <para>
/// Saves run beside the client's loop, one at a time per file, so a slow disk holds up nothing and
/// stop doesn't wait for one already writing; so do a cancelled download's last writes and the
/// deletion of its file. A save is written on a thread of its own, not the pool's the loop runs
/// on. A client started on the same path in the same process waits for all of
/// that first (<see cref="Settled"/>): it starts from what the last save left, and nothing of a
/// stopped client lands on its files after it starts. One client at a time per file.
/// </para>
/// </remarks>
internal static class StateFile
{
    /// <summary>The work under way on each state file's files, by full path.</summary>
    private static readonly Dictionary<string, Busy> All = new();

    /// <summary>
    /// Work on the files of the state file at <paramref name="path"/> (a save, a download, a
    /// deletion) starting now: counted until the result is disposed, for <see cref="Settled"/>.
    /// </summary>
    public static IDisposable Begin(string path)
    {
        var busy = BusyOf(path);
        lock (busy)
        {
            busy.Running++;
        }

        return new Work(busy);
    }

    /// <summary>Wait until no work on the files of the state file at <paramref name="path"/> is under way.</summary>
    public static void Settled(string path)
    {
        var busy = BusyOf(path);
        lock (busy)
        {
            while (busy.Running > 0)
            {
                Monitor.Wait(busy);
            }
        }
    }

    /// <summary>
    /// Save <paramref name="data"/> at <paramref name="path"/> beside the loop, after every save of
    /// the same file asked for before it: saves land in the order asked for. When its turn comes,
    /// a save no longer <paramref name="wanted"/> (cancelled) is left out; otherwise it is written,
    /// whole or not at all, and <paramref name="done"/> hears how (null: saved) before the next
    /// save starts, so the answers come in order too. It is written on a thread of its own, not
    /// one of the pool's: for as long as the disk takes, a save written there kept a thread the
    /// client's loop, transport and timers need, and in a busy process, with none to spare, the
    /// heartbeats waited for the disk.
    /// </summary>
    public static Task SaveBeside(string path, byte[] data, Func<bool> wanted, Action<Exception?> done, IFileOps? files = null)
    {
        var busy = BusyOf(path);
        var work = Begin(path);
        lock (busy)
        {
            var save = busy.LastSave.ContinueWith(
                _ =>
                {
                    using (work)
                    {
                        if (!wanted())
                        {
                            return;
                        }

                        Exception? failed = null;
                        try
                        {
                            Saving?.Invoke(path);
                            Save(path, data, files);
                        }
                        catch (Exception e)
                        {
                            failed = e;
                        }

                        done(failed);
                    }
                },
                CancellationToken.None,
                TaskContinuationOptions.LongRunning,
                TaskScheduler.Default);
            busy.LastSave = save;
            return save;
        }
    }

    /// <summary>For tests: called as each save beside the loop starts, on its thread.</summary>
    internal static Action<string>? Saving { get; set; }

    /// <summary>What was saved at <paramref name="path"/>: nothing if there is no file yet.</summary>
    public static byte[] Load(string path)
    {
        try
        {
            return File.ReadAllBytes(path);
        }
        catch (FileNotFoundException)
        {
            return [];
        }
    }

    /// <summary>
    /// Replace what is saved at <paramref name="path"/> with <paramref name="data"/>, all or nothing,
    /// through <paramref name="files"/> (the real file system if none): a temporary file made new
    /// (any left by a save that failed or crashed goes first), written and flushed, renamed over
    /// the state file, and the directory flushed: saved only then (RULE-SHELL-09). The recipe is
    /// docs/state-file.md's.
    /// </summary>
    public static void Save(string path, byte[] data, IFileOps? files = null)
    {
        files ??= FileOps.Real;
        var tmp = path + ".tmp";
        try
        {
            files.Remove(tmp);
            using (var f = files.CreateNew(tmp, download: false))
            {
                f.Write(data);
                f.Sync();
            }

            files.Rename(tmp, path);

            // The rename reaches the disk only with the directory it is in: until that is
            // flushed, a crash can leave the old state, so it isn't saved yet.
            files.SyncDir(Path.GetDirectoryName(path) is { Length: > 0 } dir ? dir : ".");
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            try
            {
                files.Remove(tmp);
            }
            catch (Exception again) when (again is IOException or UnauthorizedAccessException)
            {
                // the next save removes it first
            }

            throw new IOException($"saving {path}: {e.Message}", e);
        }
    }

    /// <summary>
    /// Make directory <paramref name="dir"/> (a download's), if need be, the user's only: whether
    /// it was made.
    /// </summary>
    public static bool MakePrivateDirectory(string dir)
    {
        var made = !Directory.Exists(dir);
        if (OperatingSystem.IsWindows())
        {
            var security = UserOnlyDirectory(CurrentUser());
            var info = new DirectoryInfo(dir);
            info.Create(security);
            info.SetAccessControl(security);
            return made;
        }

        Directory.CreateDirectory(dir, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
        return made;
    }

    /// <summary>
    /// A new file for a download at <paramref name="path"/> (an error if one is there), in its
    /// directory (made already): the user's only.
    /// </summary>
    public static FileStream CreateDownload(string path)
    {
        const int bufferSize = 64 * 1024;
        if (OperatingSystem.IsWindows())
        {
            // The file inherits the directory's list.
            return new FileStream(path, FileMode.CreateNew, FileAccess.Write, FileShare.None, bufferSize);
        }

        return new FileStream(path, new FileStreamOptions
        {
            Mode = FileMode.CreateNew,
            Access = FileAccess.Write,
            Share = FileShare.None,
            BufferSize = bufferSize,
            UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite,
        });
    }

    /// <summary>A new file at <paramref name="path"/>, open for writing, that only the user can open.</summary>
    internal static FileStream CreatePrivate(string path)
    {
        if (OperatingSystem.IsWindows())
        {
            return new FileInfo(path).Create(
                FileMode.CreateNew, FileSystemRights.Write, FileShare.None, 4096, FileOptions.None, UserOnlyFile(CurrentUser()));
        }

        return new FileStream(path, new FileStreamOptions
        {
            Mode = FileMode.CreateNew,
            Access = FileAccess.Write,
            UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite,
        });
    }

    /// <summary>The user the process runs as.</summary>
    [SupportedOSPlatform("windows")]
    internal static SecurityIdentifier CurrentUser()
    {
        using var identity = WindowsIdentity.GetCurrent();
        return identity.User ?? throw new IOException("the process has no user");
    }

    /// <summary>A file's list: <paramref name="user"/> may do anything, nobody else anything, nothing inherited.</summary>
    [SupportedOSPlatform("windows")]
    internal static FileSecurity UserOnlyFile(SecurityIdentifier user)
    {
        var security = new FileSecurity();
        security.SetAccessRuleProtection(isProtected: true, preserveInheritance: false);
        security.AddAccessRule(new FileSystemAccessRule(user, FileSystemRights.FullControl, AccessControlType.Allow));
        return security;
    }

    /// <summary>A directory's list: as for a file, and inherited by what is created in it.</summary>
    [SupportedOSPlatform("windows")]
    internal static DirectorySecurity UserOnlyDirectory(SecurityIdentifier user)
    {
        var security = new DirectorySecurity();
        security.SetAccessRuleProtection(isProtected: true, preserveInheritance: false);
        security.AddAccessRule(new FileSystemAccessRule(
            user,
            FileSystemRights.FullControl,
            InheritanceFlags.ContainerInherit | InheritanceFlags.ObjectInherit,
            PropagationFlags.None,
            AccessControlType.Allow));
        return security;
    }

    private static Busy BusyOf(string path)
    {
        var key = Path.GetFullPath(path);
        lock (All)
        {
            if (!All.TryGetValue(key, out var busy))
            {
                All[key] = busy = new Busy();
            }

            return busy;
        }
    }

    /// <summary>The work under way on one state file's files.</summary>
    private sealed class Busy
    {
        public int Running;
        public Task LastSave = Task.CompletedTask;
    }

    private sealed class Work(Busy busy) : IDisposable
    {
        private int done;

        public void Dispose()
        {
            if (Interlocked.Exchange(ref this.done, 1) == 1)
            {
                return;
            }

            lock (busy)
            {
                busy.Running--;
                Monitor.PulseAll(busy);
            }
        }
    }
}
