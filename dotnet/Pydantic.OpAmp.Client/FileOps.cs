// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Runtime.InteropServices;
using System.Text.Json;

namespace Pydantic.OpAmp.Client;

/// <summary>A file being written by a save (its temporary file) or a download.</summary>
internal interface IWritingFile : IDisposable
{
    /// <summary>Append <paramref name="data"/>.</summary>
    void Write(ReadOnlySpan<byte> data);

    /// <summary>Append <paramref name="data"/>.</summary>
    ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct);

    /// <summary>Flush what was written to the disk (fsync).</summary>
    void Sync();
}

/// <summary>
/// The file operations of the client's saves and downloads: the real file system
/// (<see cref="FileOps.Real"/>), recorded in the core tap when it is on (<see cref="FileJournal"/>),
/// or a test's, to inject faults. Each throws an <see cref="IOException"/> (or
/// <see cref="UnauthorizedAccessException"/>) when it fails.
/// </summary>
internal interface IFileOps
{
    /// <summary>
    /// A new file at <paramref name="path"/>, made exclusively (an error if one is there), that only
    /// the user can open; <paramref name="download"/>: a download's, in the downloads directory
    /// (whose access-control list it inherits on Windows).
    /// </summary>
    IWritingFile CreateNew(string path, bool download);

    /// <summary>Rename <paramref name="from"/> over <paramref name="to"/>.</summary>
    void Rename(string from, string to);

    /// <summary>Flush directory <paramref name="dir"/>'s entries to the disk: false where that isn't done (Windows).</summary>
    bool SyncDir(string dir);

    /// <summary>Delete file <paramref name="path"/>: whether there was one.</summary>
    bool Remove(string path);

    /// <summary>Make directory <paramref name="dir"/>, the user's only, if need be: whether it was made.</summary>
    bool MakeDir(string dir);

    /// <summary>Delete directory <paramref name="dir"/> and everything in it: whether there was one.</summary>
    bool Clear(string dir);
}

/// <summary>The real file system's operations.</summary>
internal static class FileOps
{
    /// <summary>The real file system.</summary>
    public static readonly IFileOps Real = new RealFileOps();

    /// <summary>
    /// Blocking file work (a download's file made, flushed to the disk, deleted) on a thread of its
    /// own, as a save is written: on one of the pool's, for as long as the disk took, it kept a
    /// thread the client's loop, transport and timers need, and in a busy process the heartbeats
    /// waited for the disk.
    /// </summary>
    public static Task<T> OffPool<T>(Func<T> blocking) =>
        Task.Factory.StartNew(blocking, CancellationToken.None, TaskCreationOptions.LongRunning, TaskScheduler.Default);

    /// <inheritdoc cref="OffPool{T}(Func{T})"/>
    public static Task OffPool(Action blocking) =>
        Task.Factory.StartNew(blocking, CancellationToken.None, TaskCreationOptions.LongRunning, TaskScheduler.Default);

    private sealed class RealFileOps : IFileOps
    {
        public IWritingFile CreateNew(string path, bool download) =>
            new RealFile(download ? StateFile.CreateDownload(path) : StateFile.CreatePrivate(path));

        public void Rename(string from, string to) => File.Move(from, to, overwrite: true);

        public bool SyncDir(string dir) => Posix.SyncDir(dir);

        public bool Remove(string path)
        {
            if (!File.Exists(path))
            {
                return false;
            }

            File.Delete(path);
            return true;
        }

        public bool MakeDir(string dir) => StateFile.MakePrivateDirectory(dir);

        public bool Clear(string dir)
        {
            try
            {
                Directory.Delete(dir, recursive: true);
                return true;
            }
            catch (DirectoryNotFoundException)
            {
                return false;
            }
        }
    }

    private sealed class RealFile(FileStream file) : IWritingFile
    {
        public void Write(ReadOnlySpan<byte> data) => file.Write(data);

        public ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct) => file.WriteAsync(data, ct);

        public void Sync() => file.Flush(flushToDisk: true);

        public void Dispose() => file.Dispose();
    }

    /// <summary>
    /// fsync of a directory, which .NET can't open: through the C library, on Unix. A rename
    /// reaches the disk only once the directory it is in is flushed. On macOS fsync(2) only hands
    /// the entries to the drive, which may keep them in its cache: fcntl(F_FULLFSYNC) asks it to
    /// write them out (as <see cref="FileStream.Flush(bool)"/> does for a file), and fsync is the
    /// fallback where the file system doesn't support that, as libuv and OpenJDK do.
    /// </summary>
    internal static class Posix
    {
        /// <summary>F_FULLFSYNC, in macOS's &lt;sys/fcntl.h&gt;.</summary>
        internal const int FFullFsync = 51;

        private static readonly Lazy<Fns?> Lib = new(Load);

        [UnmanagedFunctionPointer(CallingConvention.Cdecl, SetLastError = true)]
        internal delegate int OpenFn([MarshalAs(UnmanagedType.LPUTF8Str)] string path, int flags);

        [UnmanagedFunctionPointer(CallingConvention.Cdecl, SetLastError = true)]
        internal delegate int FdFn(int fd);

        /// <summary>
        /// fcntl(2) with a command that takes no argument, as F_FULLFSYNC: fcntl is variadic, and
        /// only its fixed arguments are passed (in registers, on every ABI), none it doesn't read.
        /// </summary>
        [UnmanagedFunctionPointer(CallingConvention.Cdecl, SetLastError = true)]
        internal delegate int FcntlFn(int fd, int cmd);

        public static bool SyncDir(string dir)
        {
            if (OperatingSystem.IsWindows())
            {
                return false; // NTFS journals its directories; there is nothing to open
            }

            if (Lib.Value is not { } lib)
            {
                return false; // no C library found: not flushed, and not recorded as flushed
            }

            return SyncDir(dir, lib);
        }

        /// <summary>Flush directory <paramref name="dir"/> through <paramref name="lib"/>'s functions.</summary>
        internal static bool SyncDir(string dir, Fns lib)
        {
            var fd = lib.Open(dir, 0); // O_RDONLY
            if (fd < 0)
            {
                throw new IOException($"syncing the directory {dir}: open: {Marshal.GetPInvokeErrorMessage(Marshal.GetLastPInvokeError())}");
            }

            try
            {
                if (Flush(lib, fd) != 0)
                {
                    throw new IOException($"syncing the directory {dir}: {Marshal.GetPInvokeErrorMessage(Marshal.GetLastPInvokeError())}");
                }
            }
            finally
            {
                lib.Close(fd);
            }

            return true;
        }

        /// <summary>fcntl(F_FULLFSYNC) where there is one (macOS), else or if it fails fsync: 0, or -1 with errno set.</summary>
        private static int Flush(Fns lib, int fd) =>
            lib.FullFsync is { } fcntl && fcntl(fd, FFullFsync) != -1 ? 0 : lib.Fsync(fd);

        private static Fns? Load()
        {
            var apple = OperatingSystem.IsMacOS() || OperatingSystem.IsIOS();
            string[] names = apple
                ? ["/usr/lib/libSystem.B.dylib", "libSystem.dylib"]
                : ["libc.so.6", "libc.musl-x86_64.so.1", "libc.musl-aarch64.so.1", "libc.so", "libc"];
            foreach (var name in names)
            {
                if (NativeLibrary.TryLoad(name, out var lib)
                    && NativeLibrary.TryGetExport(lib, "open", out var open)
                    && NativeLibrary.TryGetExport(lib, "fsync", out var fsync)
                    && NativeLibrary.TryGetExport(lib, "close", out var close))
                {
                    // F_FULLFSYNC is Apple's only; without fcntl, fsync alone.
                    var fullFsync = apple && NativeLibrary.TryGetExport(lib, "fcntl", out var fcntl)
                        ? Marshal.GetDelegateForFunctionPointer<FcntlFn>(fcntl)
                        : null;
                    return new Fns(
                        Marshal.GetDelegateForFunctionPointer<OpenFn>(open),
                        Marshal.GetDelegateForFunctionPointer<FdFn>(fsync),
                        Marshal.GetDelegateForFunctionPointer<FdFn>(close),
                        fullFsync);
                }
            }

            return null;
        }

        /// <summary>The C library's functions; <paramref name="FullFsync"/>: fcntl, where F_FULLFSYNC is the flush (macOS).</summary>
        internal sealed record Fns(OpenFn Open, FdFn Fsync, FdFn Close, FcntlFn? FullFsync);
    }
}

/// <summary>
/// The file operations of a client with a core tap, recorded in it (records of kind 5, one JSON
/// object each, the tap checker's file records) after each succeeded, before anything that depends
/// on it reaches the core. Paths are relative to the state file's directory, joined with
/// <c>/</c>: <c>.</c> is that directory. A save's writes carry their bytes, a download's how many.
/// </summary>
internal sealed class FileJournal : IFileOps
{
    public const byte Kind = 5;

    private readonly IFileOps inner;
    private readonly CoreTap tap;
    private readonly string dir;

    /// <summary>
    /// Record <paramref name="inner"/>'s operations on the files of the state file at
    /// <paramref name="stateFile"/> in <paramref name="tap"/>, which names the state file first.
    /// </summary>
    public FileJournal(IFileOps inner, CoreTap tap, string stateFile)
    {
        this.inner = inner;
        this.tap = tap;
        var full = Path.GetFullPath(stateFile);
        this.dir = Path.GetDirectoryName(full) ?? full;
        this.Record("state", ("path", this.Relative(full)));
    }

    public IWritingFile CreateNew(string path, bool download)
    {
        var file = this.inner.CreateNew(path, download);
        this.Record("create", ("path", this.Relative(path)));
        return new Journaled(this, file, this.Relative(path), download);
    }

    public void Rename(string from, string to)
    {
        this.inner.Rename(from, to);
        this.Record("rename", ("from", this.Relative(from)), ("to", this.Relative(to)));
    }

    public bool SyncDir(string dir)
    {
        var synced = this.inner.SyncDir(dir);
        if (synced)
        {
            this.Record("syncdir", ("path", this.Relative(dir)));
        }

        return synced;
    }

    public bool Remove(string path)
    {
        var removed = this.inner.Remove(path);
        if (removed)
        {
            this.Record("remove", ("path", this.Relative(path)));
        }

        return removed;
    }

    public bool MakeDir(string dir)
    {
        var made = this.inner.MakeDir(dir);
        if (made)
        {
            this.Record("mkdir", ("path", this.Relative(dir)));
        }

        return made;
    }

    public bool Clear(string dir)
    {
        var cleared = this.inner.Clear(dir);
        if (cleared)
        {
            this.Record("clear", ("path", this.Relative(dir)));
        }

        return cleared;
    }

    /// <summary><paramref name="path"/> as the tap names it.</summary>
    private string Relative(string path)
    {
        var rel = Path.GetRelativePath(this.dir, Path.GetFullPath(path));
        return Path.DirectorySeparatorChar == '/' ? rel : rel.Replace(Path.DirectorySeparatorChar, '/');
    }

    private void Record(string op, params (string Key, object Value)[] fields)
    {
        using var buf = new MemoryStream();
        using (var json = new Utf8JsonWriter(buf))
        {
            json.WriteStartObject();
            json.WriteString("op", op);
            foreach (var (key, value) in fields)
            {
                if (value is long n)
                {
                    json.WriteNumber(key, n);
                }
                else
                {
                    json.WriteString(key, (string)value);
                }
            }

            json.WriteEndObject();
        }

        this.tap.Record(Kind, buf.ToArray());
    }

    private sealed class Journaled(FileJournal journal, IWritingFile file, string path, bool download) : IWritingFile
    {
        public void Write(ReadOnlySpan<byte> data)
        {
            file.Write(data);
            this.Wrote(data);
        }

        public async ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
        {
            await file.WriteAsync(data, ct).ConfigureAwait(false);
            this.Wrote(data.Span);
        }

        public void Sync()
        {
            file.Sync();
            journal.Record("sync", ("path", path));
        }

        public void Dispose() => file.Dispose();

        private void Wrote(ReadOnlySpan<byte> data)
        {
            if (download)
            {
                journal.Record("write", ("path", path), ("len", (long)data.Length));
            }
            else
            {
                journal.Record("write", ("path", path), ("data", Convert.ToHexString(data).ToLowerInvariant()));
            }
        }
    }
}
