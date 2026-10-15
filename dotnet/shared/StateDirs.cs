// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Reflection;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// Where the agent keeps its state: the directory named (<c>OTEL_OPAMP_STATE_DIR</c>, or the hosting
/// package's <c>OpAmp:StateDirectory</c>), none (<c>none</c>), or by default one of the user's own,
/// so an instance keeps its UID across restarts with no setup (docs/default-state-dir.md).
/// </summary>
internal static partial class StateDirs
{
    /// <summary>Processes of one program on one host that each keep their own identity by default.</summary>
    public const int Slots = 16;

    /// <summary>
    /// Settle <paramref name="server"/>'s state directory and take its lock (held while the process
    /// runs). False when a directory named is another process's, or can't be locked: the SDK runs
    /// unmanaged. Without one named, the default, if there is one to be had; never an error.
    /// </summary>
    public static bool Claim(ref Server server, Func<string, string?> getenv, AgentLog log, out FileStream? stateLock)
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

        // A service.instance.id the application chose stays its choice: no default to override it.
        // One that isn't a UUID can't be the instance UID: it is replaced, by a UID kept here.
        if (Uuid.Chosen(getenv("OTEL_RESOURCE_ATTRIBUTES")) is not null)
        {
            return true;
        }

        if (Default(getenv, log) is { } claimed)
        {
            server = server with { StateDir = claimed.Dir };
            stateLock = claimed.Lock;
        }

        return true;
    }

    /// <summary>
    /// A state directory of the user's own: one per program, service and working directory; a slot
    /// in it per process running at once, each held by a lock. Null where there's none to be had
    /// (no home, not writable, every slot taken): then each start is a new instance, as before.
    /// </summary>
    public static (string Dir, FileStream Lock)? Default(Func<string, string?> getenv, AgentLog log, string? program = null, string? cwd = null)
    {
        try
        {
            var stateBase = Base(getenv);
            if (string.IsNullOrEmpty(stateBase))
            {
                return null;
            }

            program ??= ProgramName();
            if (cwd is null)
            {
                try
                {
                    cwd = Environment.CurrentDirectory;
                }
                catch (Exception e) when (e is IOException or UnauthorizedAccessException)
                {
                    cwd = string.Empty;
                }
            }

            var attrs = Attributes(getenv);
            var service = getenv("OTEL_SERVICE_NAME")?.Trim() is { Length: > 0 } s ? s
                : attrs.GetValueOrDefault("service.name") is { Length: > 0 } a ? a : program;
            var key = string.Join('\0', service, attrs.GetValueOrDefault("service.namespace") ?? string.Empty, program, cwd);
            var hash = Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(key)))[..8].ToLowerInvariant();
            var name = NotInName().Replace(service, "_");
            name = name.Length > 64 ? name[..64] : name.Length == 0 ? "service" : name;
            var home = Path.Combine(stateBase, "opamp", $"{name}-{hash}");
            for (var slot = 1; slot <= Slots; slot++)
            {
                var dir = Path.Combine(home, slot.ToString(System.Globalization.CultureInfo.InvariantCulture));
                try
                {
                    return (dir, OpenLock(dir));
                }
                catch (IOException) when (Directory.Exists(dir) && File.Exists(Path.Combine(dir, "lock")))
                {
                    // Another process of it, running now, holds this slot.
                }
            }

            log.Info($"{Slots} processes of {service} already keep their identity here: this one is a new instance each start");
            return null;
        }
        catch (Exception)
        {
            // No home, a base that can't be written: each start is a new instance, as before.
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

        return new FileStream(Path.Combine(dir, "lock"), options);
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

    /// <summary>The user's state directory: <c>%LOCALAPPDATA%</c>, <c>~/Library/Application Support</c>, or XDG's.</summary>
    private static string? Base(Func<string, string?> getenv)
    {
        static string? Set(string? v) => string.IsNullOrWhiteSpace(v) ? null : v;
        if (OperatingSystem.IsWindows())
        {
            return Set(getenv("LOCALAPPDATA"));
        }

        var home = Set(getenv("HOME"));
        if (OperatingSystem.IsMacOS())
        {
            return home is null ? null : Path.Combine(home, "Library", "Application Support");
        }

        return Set(getenv("XDG_STATE_HOME")) ?? (home is null ? null : Path.Combine(home, ".local", "state"));
    }

    /// <summary>OTEL_RESOURCE_ATTRIBUTES: <c>k=v,...</c>, values percent-decoded.</summary>
    private static Dictionary<string, string> Attributes(Func<string, string?> getenv)
    {
        var attrs = new Dictionary<string, string>();
        foreach (var kv in (getenv("OTEL_RESOURCE_ATTRIBUTES") ?? string.Empty).Split(','))
        {
            var eq = kv.IndexOf('=');
            if (eq > 0)
            {
                attrs[kv[..eq].Trim()] = Uri.UnescapeDataString(kv[(eq + 1)..].Trim());
            }
        }

        return attrs;
    }

    [GeneratedRegex("[^A-Za-z0-9._-]+")]
    private static partial Regex NotInName();
}
