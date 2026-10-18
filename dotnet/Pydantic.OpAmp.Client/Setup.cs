// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Google.Protobuf;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// The core's Setup call: an OpenTelemetry integration's start-up decisions (where the server is,
/// the headers, the heartbeat interval, the state directory, the instance UID, what is safe to log),
/// made by the core, the same in every language. The integration keeps only the I/O: reading its
/// settings, making and locking a state directory's slot, reading and writing its instance-uid
/// file, and logging what it is told to. It needs no client and keeps no state: the same call
/// always gives the same answer. Thread-safe.
/// </summary>
public static class OpAmpSetup
{
    /// <summary>The file in a state directory's slot that a process holds a lock on while it runs.</summary>
    public const string LockFile = "lock";

    /// <summary>The file in a state directory that holds the instance UID.</summary>
    public const string UidFile = "instance-uid";

    /// <summary>
    /// Where the server is and how to reach it, from the environment and what the code gives;
    /// <c>Off</c> without an endpoint. Throws <see cref="CoreException"/> on a failure of the core.
    /// </summary>
    public static SetupStarted Start(SetupStart start) => Call(new SetupCall { Start = start }).Started;

    /// <summary>
    /// The instance UID, once the state directory is known (and its slot locked). Throws
    /// <see cref="CoreException"/> on a failure of the core, or a call it can't use.
    /// </summary>
    public static SetupIdentified Identify(SetupIdentity identity) => Call(new SetupCall { Identity = identity }).Identified;

    /// <summary>
    /// Why the core can't be loaded here (no native library for this platform, a file that isn't
    /// one, or a library of another interface), or null when it can. An integration asks before
    /// <see cref="Start"/>, and runs unmanaged when the core can't be loaded: the calls above throw then.
    /// </summary>
    public static string? Unavailable() => Unavailable(Native.Check);

    /// <summary>Why <paramref name="load"/> fails (as <see cref="Unavailable()"/> says it), or null when it doesn't.</summary>
    internal static string? Unavailable(Action load)
    {
        try
        {
            load();
            return null;
        }
        catch (Exception e)
        {
            // DllNotFoundException, BadImageFormatException, EntryPointNotFoundException, a stale library's interface.
            return $"{e.GetType().Name}: {e.Message}";
        }
    }

    private static SetupReply Call(SetupCall call)
    {
        var reply = SetupReply.Parser.ParseFrom(Native.SetupCall(call.ToByteArray()));
        return reply.ResultCase == SetupReply.ResultOneofCase.Error ? throw new CoreException(reply.Error) : reply;
    }
}
