// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Reflection;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// Where the agent keeps its state: the directory named (<c>OTEL_OPAMP_STATE_DIR</c>, or the hosting
/// package's <c>OpAmp:StateDirectory</c>), none (<c>none</c>), or by default one of the user's own,
/// so an instance keeps its UID across restarts with no setup (docs/default-state-dir.md). Which,
/// and the default one's slots, are the core's Setup call's to say; this takes the lock.
/// </summary>
internal static class StateDirs
{
    /// <summary>
    /// Settle <paramref name="server"/>'s state directory and take its lock (held while the process
    /// runs). False when a directory named is another process's, or can't be locked: the SDK runs
    /// unmanaged. Without one named, the first of the default one's slots it can have, if any;
    /// never an error.
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
        if (server.StateDir is { } dir)
        {
            stateLock = SdkAgent.Lock(dir, log);
            return stateLock is not null;
        }

        if (Slot(server.DefaultSlots, server.AllSlotsTaken, log) is { } claimed)
        {
            server = server with { StateDir = claimed.Dir };
            stateLock = claimed.Lock;
        }

        return true;
    }

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
    /// line to say then). Never an error.
    /// </summary>
    public static (string Dir, FileStream Lock)? Slot(IReadOnlyList<string>? slots, string? allTaken, AgentLog log)
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
        catch (Exception)
        {
            // A base that can't be written: each start is a new instance, as before.
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
