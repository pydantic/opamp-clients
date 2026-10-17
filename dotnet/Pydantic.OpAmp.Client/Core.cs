// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Security.AccessControl;
using Google.Protobuf;
using Pydantic.OpAmp.Core;
using Config = Pydantic.OpAmp.Core.Config;

namespace Pydantic.OpAmp.Client;

/// <summary>The core refused an input (a bug in this client) or trapped (a bug in the core).</summary>
public sealed class CoreException : Exception
{
    internal CoreException(string message, Exception? inner = null)
        : base(message, inner)
    {
    }
}

/// <summary>
/// The shared OpAMP client core, run natively (the native library, <see cref="NativeCore"/>).
/// Each Core is one session, holding one client's state. Not thread-safe: the client calls it from
/// one thread.
/// </summary>
internal sealed class CoreInstance : IDisposable
{
    private readonly ICoreEngine engine;
    private readonly CoreTap? tap;

    /// <summary>
    /// A new core, set up with <paramref name="config"/>. <paramref name="getenv"/> reads
    /// OPAMP_CORE_TAP (the process's environment unless a test gives its own). <paramref name="engine"/>:
    /// tests only, to run another core behind the same bytes (the WebAssembly module, or a hostile
    /// core, the adversary) instead of the native one.
    /// </summary>
    public CoreInstance(Config config, Func<string, string?>? getenv = null, Func<ICoreEngine>? engine = null)
    {
        this.engine = (engine ?? (() => new NativeCore()))();
        try
        {
            this.tap = CoreTap.Open((getenv ?? Environment.GetEnvironmentVariable)(CoreTap.Variable));
            this.Initial = this.Call(this.engine.Init, config, CoreTap.InitIn);
        }
        catch
        {
            this.Dispose();
            throw;
        }
    }

    /// <summary>What init returned: the instance UID in use from the start, among the rest.</summary>
    public Output Initial { get; }

    /// <summary>The core tap, if OPAMP_CORE_TAP asked for one: the client's file operations go to it too.</summary>
    public CoreTap? Tap => this.tap;

    /// <summary>
    /// Feed the core one input at <paramref name="nowMs"/> (milliseconds, monotonic) and
    /// <paramref name="unixMs"/> (the wall clock, in ms since the Unix epoch: Rust mode reads
    /// Retry-After dates against it).
    /// </summary>
    public Output Step(long nowMs, long unixMs, Input input) =>
        this.Call(this.engine.Step, new global::Pydantic.OpAmp.Core.Step { NowMs = (ulong)nowMs, Input = input, UnixMs = (ulong)unixMs }, CoreTap.StepIn);

    public void Dispose()
    {
        this.tap?.Dispose();
        this.engine.Dispose();
    }

    private Output Call(Func<byte[], byte[]> fn, IMessage msg, byte tapKind)
    {
        var data = msg.ToByteArray();
        this.tap?.Record(tapKind, data);
        var raw = fn(data);
        this.tap?.Record((byte)(tapKind + 1), raw);
        var reply = Reply.Parser.ParseFrom(raw);
        if (reply.Error.Length > 0)
        {
            throw new CoreException(reply.Error);
        }

        return reply.Output ?? new Output();
    }
}

/// <summary>
/// Core tap: with OPAMP_CORE_TAP=&lt;path&gt; set, every call into the core is appended to a file for the
/// effects checker (opamp-tap). Each record is a kind byte (1 init in, 2 init out, 3 step in,
/// 4 step out), a u32 LE length, a u64 LE monotonic time in nanoseconds, then the bytes exactly as
/// they crossed (Config, Reply, Step, Reply). A process's first core writes &lt;path&gt;, each later one
/// &lt;path&gt;.&lt;pid&gt;.&lt;n&gt;. Unset, it costs nothing. The client's file operations go to it too
/// (kind 5, <see cref="FileJournal"/>), from the threads that save and download: records are
/// written one at a time, and none once the tap is closed.
/// </summary>
internal sealed class CoreTap : IDisposable
{
    public const string Variable = "OPAMP_CORE_TAP";
    public const byte InitIn = 1;
    public const byte StepIn = 3;

    private const int HeadBytes = 1 + 4 + 8;

    /// <summary>The cores tapped in this process so far.</summary>
    private static int tapped;

    /// <summary>How many cores this process has tapped.</summary>
    internal static int Count => Volatile.Read(ref tapped);

    private readonly FileStream file;
    private bool closed;

    private CoreTap(FileStream file) => this.file = file;

    /// <summary>
    /// A tap for a new core, if <paramref name="path"/> (the variable's value) names one: null
    /// when it is unset or empty, or when the file can't be opened (said once on stderr).
    /// </summary>
    public static CoreTap? Open(string? path)
    {
        if (string.IsNullOrEmpty(path))
        {
            return null;
        }

        var n = Interlocked.Increment(ref tapped) - 1;
        var name = n == 0 ? path : $"{path}.{Environment.ProcessId}.{n}";
        try
        {
            // Unbuffered: each record is one write, so a killed process leaves whole records.
            return new CoreTap(Append(name));
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            Console.Error.WriteLine($"opamp: the core tap {name} can't be opened, running without it: {e.Message}");
            return null;
        }
    }

    /// <summary>
    /// <paramref name="name"/> to append to. It holds the Config's headers and the credentials
    /// saved, so a new file is the user's only (0600, or a list of the user alone), as the state
    /// file is; one already there is narrowed to 0600 (on Windows it keeps its list).
    /// </summary>
    private static FileStream Append(string name)
    {
        if (OperatingSystem.IsWindows())
        {
            return new FileInfo(name).Create(
                FileMode.Append, FileSystemRights.Write, FileShare.ReadWrite, bufferSize: 1, FileOptions.None,
                StateFile.UserOnlyFile(StateFile.CurrentUser()));
        }

        var stream = new FileStream(name, new FileStreamOptions
        {
            Mode = FileMode.Append,
            Access = FileAccess.Write,
            Share = FileShare.ReadWrite,
            BufferSize = 0,
            UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite,
        });
        File.SetUnixFileMode(stream.SafeFileHandle, UnixFileMode.UserRead | UnixFileMode.UserWrite);
        return stream;
    }

    /// <summary>Append one record: <paramref name="kind"/>, the length, the time, <paramref name="bytes"/>.</summary>
    public void Record(byte kind, byte[] bytes)
    {
        var record = new byte[HeadBytes + bytes.Length];
        record[0] = kind;
        System.Buffers.Binary.BinaryPrimitives.WriteUInt32LittleEndian(record.AsSpan(1), (uint)bytes.Length);
        System.Buffers.Binary.BinaryPrimitives.WriteUInt64LittleEndian(record.AsSpan(5), Nanos());
        bytes.CopyTo(record, HeadBytes);
        lock (this.file)
        {
            if (this.closed)
            {
                return; // a save or download outlived its client: nothing more of it is told
            }

            try
            {
                this.file.Write(record);
            }
            catch (IOException)
            {
                // the tap is for the checker: the client goes on without this record
            }
        }
    }

    public void Dispose()
    {
        lock (this.file)
        {
            this.closed = true;
            this.file.Dispose();
        }
    }

    /// <summary>The monotonic clock, in nanoseconds.</summary>
    private static ulong Nanos() =>
        (ulong)((UInt128)(ulong)System.Diagnostics.Stopwatch.GetTimestamp() * 1_000_000_000 / (ulong)System.Diagnostics.Stopwatch.Frequency);
}
