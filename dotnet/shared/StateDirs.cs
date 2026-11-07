// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Reflection;
using Logfire.Fleet.Client;
using Logfire.Fleet.Core;

namespace Logfire.Fleet.Otel;

/// <summary>
/// Where the agent keeps its state: the directory named (<c>OTEL_OPAMP_STATE_DIR</c>, or the hosting
/// package's <c>OpAmp:StateDirectory</c>), none (<c>none</c>), or by default one of the user's own,
/// so an instance keeps its UID across restarts with no setup (docs/default-state-dir.md). Which,
/// and the default one's slots, are the core's Setup call's to say; this takes the lock, on a named
/// one too, so processes sharing it each keep their own identity.
/// </summary>
internal static class StateDirs
{
    /// <summary>How many processes running at once keep their own identity in one state directory.</summary>
    public const int Slots = 16;

    /// <summary>
    /// Settle <paramref name="server"/>'s state directory and take its lock (held while the process
    /// runs): the first of its slots this process can have, the named one's (<see cref="NamedSlots"/>)
    /// or the default one's. None to be had (not writable, every slot taken): no state directory, so
    /// each start is a new instance; a directory named that can't be used is said as a warning.
    /// Always true: the SDK runs managed either way.
    /// </summary>
    public static bool Claim(ref Server server, AgentLog log, out FileStream? stateLock)
    {
        stateLock = null;
        var named = server.StateDir?.Trim();
        if (string.Equals(named, "none", StringComparison.OrdinalIgnoreCase))
        {
            server = server with { StateDir = null };
            return true;
        }

        // The directory locked is the one the state is kept in: trimmed, and blank is none named.
        server = server with { StateDir = string.IsNullOrEmpty(named) ? null : named };
        // A directory named is locked as the default one is, so processes sharing it never join as one instance.
        var claimed = server.StateDir is { } dir
            ? Slot(NamedSlots(dir), $"{Slots} processes already keep their identity in {dir}: this one is a new instance each start", log, named: dir)
            : Slot(server.DefaultSlots, server.AllSlotsTaken, log);
        server = server with { StateDir = claimed?.Dir };
        stateLock = claimed?.Lock;
        return true;
    }

    /// <summary>
    /// A named state directory's slots (docs/default-state-dir.md): itself, then its folders 2 to
    /// 16. The first process keeps the directory and its UID, as before slots; one sharing it (a
    /// child process inherits <c>OTEL_OPAMP_STATE_DIR</c>) takes a folder of its own instead of
    /// running unmanaged.
    /// </summary>
    public static IReadOnlyList<string> NamedSlots(string dir) =>
        [dir, .. Enumerable.Range(2, Slots - 1).Select(n => Path.Combine(dir, n.ToString(System.Globalization.CultureInfo.InvariantCulture)))];

    /// <summary>
    /// The default state directory, for <paramref name="program"/> in <paramref name="cwd"/> (null:
    /// this process's), as the core's Setup names it, and the first of its slots this process can
    /// have. Null where there's none to be had (no home, not writable, every slot taken): then each
    /// start is a new instance, as before.
    /// </summary>
    public static (string Dir, FileStream Lock)? Default(Func<string, string?> getenv, AgentLog log, string? program = null, string? cwd = null)
    {
        var start = Server.Start(getenv, program, cwd);
        start.Code = new SetupInCode { Endpoint = "http://localhost" }; // any: the slots don't depend on it
        try
        {
            var s = OpAmpSetup.Start(start);
            return Slot(s.DefaultSlots, s.AllSlotsTaken, log);
        }
        catch (CoreException)
        {
            return null;
        }
    }

    /// <summary>
    /// The first of <paramref name="slots"/> this process can make and lock, and its lock; null where
    /// there's none to be had (not writable, every slot taken: <paramref name="allTaken"/> is the
    /// line to say then). Never an error; when the slots are those of the directory
    /// <paramref name="named"/>, one that can't be used is said as a warning.
    /// </summary>
    public static (string Dir, FileStream Lock)? Slot(IReadOnlyList<string>? slots, string? allTaken, AgentLog log, string? named = null)
    {
        if (slots is not { Count: > 0 })
        {
            return null;
        }

        try
        {
            foreach (var dir in slots)
            {
                try
                {
                    return (dir, OpenLock(dir));
                }
                catch (IOException) when (Directory.Exists(dir) && File.Exists(Path.Combine(dir, OpAmpSetup.LockFile)))
                {
                    // Another process of it, running now, holds this slot.
                }
            }

            log.Info(allTaken ?? string.Empty);
            return null;
        }
        catch (Exception e)
        {
            // A base that can't be written: each start is a new instance, as before.
            if (named is not null)
            {
                log.Warn($"the state directory {named} can't be used ({e.Message}): each start is a new instance");
            }

            return null;
        }
    }

    /// <summary>
    /// The lock on a state directory, made readable by the user only if it is new: the operating
    /// system's, on its <c>lock</c> file, taken without waiting (it throws if another process has
    /// it), held while the stream is open, never inherited by a process this one starts.
    /// </summary>
    public static FileStream OpenLock(string dir)
    {
        var options = new FileStreamOptions { Mode = FileMode.OpenOrCreate, Access = FileAccess.ReadWrite, Share = FileShare.None };
        if (OperatingSystem.IsWindows())
        {
            Directory.CreateDirectory(dir);
        }
        else
        {
            Directory.CreateDirectory(dir, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
            options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
        }

        return new FileStream(Path.Combine(dir, OpAmpSetup.LockFile), options);
    }

    /// <summary>The program, as a name: the entry assembly's, else the process's.</summary>
    public static string ProgramName()
    {
        try
        {
            if (Assembly.GetEntryAssembly()?.GetName().Name is { Length: > 0 } entry)
            {
                return entry;
            }

            using var p = System.Diagnostics.Process.GetCurrentProcess();
            return p.ProcessName;
        }
        catch (Exception)
        {
            return "dotnet";
        }
    }
}
