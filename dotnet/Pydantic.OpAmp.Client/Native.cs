// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Runtime.InteropServices;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// One session of the shared core: an encoded <c>Config</c> in, then encoded <c>Step</c>s, each
/// answered with an encoded <c>Reply</c>, exactly as the WebAssembly module's <c>init</c> and
/// <c>step</c> answer (docs/how-it-works.md, "The interface"). The client runs <see cref="NativeCore"/>; tests run the
/// module itself, or a hostile one, behind the same interface. Not thread-safe: one caller at a time.
/// </summary>
internal interface ICoreEngine : IDisposable
{
    /// <summary>Start the session. Throws <see cref="CoreException"/> if the core trapped (panicked).</summary>
    byte[] Init(byte[] config);

    /// <summary>Run one step; as <see cref="Init"/>.</summary>
    byte[] Step(byte[] step);
}

/// <summary>
/// The shared core and the SDK settings layer as a native library, built from
/// the same source as the WebAssembly modules and answering with the same bytes. Shipped beside
/// the assembly (runtimes/&lt;rid&gt;/native in a package).
/// </summary>
internal static partial class Native
{
    /// <summary>The version of the library's C interface this assembly speaks (the library's ABI_VERSION).</summary>
    internal const uint AbiVersion = 1;

    private const string Lib = "opamp_native";

    private static readonly Lazy<bool> Checked = new(() =>
    {
        var version = opamp_native_abi_version();
        return version == AbiVersion
            ? true
            : throw new InvalidOperationException($"the native OpAMP core speaks interface {version}, this assembly {AbiVersion}: a stale {Lib} library is installed beside it");
    });

    /// <summary>Make sure the library is there and speaks this assembly's interface.</summary>
    internal static void Check() => _ = Checked.Value;

    /// <summary>Run one encoded SDK settings request; throws <see cref="CoreException"/> if the layer panicked.</summary>
    internal static unsafe byte[] SdkSettingsCall(byte[] request)
    {
        Check();
        fixed (byte* p = request)
        {
            return Take(opamp_sdk_settings_call(p, (nuint)request.Length, out var outPtr, out var outLen), outPtr, outLen);
        }
    }

    /// <summary>What a call handed back, copied out and freed: the reply, or a panic as <see cref="CoreException"/>.</summary>
    internal static unsafe byte[] Take(int code, byte* ptr, nuint len)
    {
        var bytes = new ReadOnlySpan<byte>(ptr, checked((int)len)).ToArray();
        opamp_bytes_free(ptr, len);
        return code == 0 ? bytes : throw new CoreException($"trap: {System.Text.Encoding.UTF8.GetString(bytes)}");
    }

    [LibraryImport(Lib)]
    internal static partial uint opamp_native_abi_version();

    [LibraryImport(Lib)]
    internal static partial CoreHandle opamp_core_new();

    [LibraryImport(Lib)]
    internal static partial void opamp_core_free(IntPtr handle);

    [LibraryImport(Lib)]
    internal static unsafe partial int opamp_core_init(CoreHandle handle, byte* ptr, nuint len, out byte* outPtr, out nuint outLen);

    [LibraryImport(Lib)]
    internal static unsafe partial int opamp_core_step(CoreHandle handle, byte* ptr, nuint len, out byte* outPtr, out nuint outLen);

    [LibraryImport(Lib)]
    internal static unsafe partial int opamp_sdk_settings_call(byte* ptr, nuint len, out byte* outPtr, out nuint outLen);

    [LibraryImport(Lib)]
    internal static unsafe partial void opamp_bytes_free(byte* ptr, nuint len);
}

/// <summary>A session in the native library, freed once.</summary>
internal sealed class CoreHandle : SafeHandle
{
    public CoreHandle()
        : base(IntPtr.Zero, ownsHandle: true)
    {
    }

    public override bool IsInvalid => this.handle == IntPtr.Zero;

    protected override bool ReleaseHandle()
    {
        Native.opamp_core_free(this.handle);
        return true;
    }
}

/// <summary>The shared core, run natively: what every client runs.</summary>
internal sealed class NativeCore : ICoreEngine
{
    private readonly CoreHandle handle;

    public NativeCore()
    {
        Native.Check();
        this.handle = Native.opamp_core_new();
    }

    public unsafe byte[] Init(byte[] config)
    {
        fixed (byte* p = config)
        {
            return Native.Take(Native.opamp_core_init(this.handle, p, (nuint)config.Length, out var o, out var n), o, n);
        }
    }

    public unsafe byte[] Step(byte[] step)
    {
        fixed (byte* p = step)
        {
            return Native.Take(Native.opamp_core_step(this.handle, p, (nuint)step.Length, out var o, out var n), o, n);
        }
    }

    public void Dispose() => this.handle.Dispose();
}
