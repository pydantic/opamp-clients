// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Wasmtime;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// A core run as a WebAssembly module in Wasmtime, behind the interface the client runs the native
/// core through: the shipped module (opamp_core.wasm, the oracle the native core is held to), or a
/// hostile one (the adversary, testdata/core/opamp_core_adversary.wasm). One instance per session.
/// </summary>
internal sealed class WasmCore : ICoreEngine
{
    private static readonly Lazy<Module> Shipped = new(() => Compile(File.ReadAllBytes(Path.Combine(AppContext.BaseDirectory, "opamp_core.wasm"))));
    private static readonly Engine WasmEngine = new();

    private readonly Store store;
    private readonly Memory memory;
    private readonly Func<int, int> alloc;
    private readonly Action<int, int> free;
    private readonly Func<int, int, long> init;
    private readonly Func<int, int, long> step;

    private WasmCore(Module module)
    {
        this.store = new Store(WasmEngine);
        var instance = new Instance(this.store, module);
        this.memory = instance.GetMemory("memory") ?? throw new InvalidOperationException("no memory export");
        this.alloc = instance.GetFunction<int, int>("alloc") ?? throw new InvalidOperationException("no alloc");
        this.free = instance.GetAction<int, int>("free") ?? throw new InvalidOperationException("no free");
        this.init = instance.GetFunction<int, int, long>("init") ?? throw new InvalidOperationException("no init");
        this.step = instance.GetFunction<int, int, long>("step") ?? throw new InvalidOperationException("no step");
    }

    /// <summary>Sessions of the shipped module.</summary>
    public static Func<ICoreEngine> Module() => () => new WasmCore(Shipped.Value);

    /// <summary>Sessions of the module <paramref name="wasm"/>, compiled once.</summary>
    public static Func<ICoreEngine> Of(byte[] wasm)
    {
        var module = Compile(wasm);
        return () => new WasmCore(module);
    }

    public byte[] Init(byte[] config) => this.Call(this.init, config);

    public byte[] Step(byte[] step) => this.Call(this.step, step);

    public void Dispose() => this.store.Dispose();

    private static Module Compile(byte[] wasm) => Wasmtime.Module.FromBytes(WasmEngine, "core", wasm);

    private byte[] Call(Func<int, int, long> fn, byte[] data)
    {
        long packed;
        try
        {
            var ptr = this.alloc(data.Length);
            data.CopyTo(this.memory.GetSpan(ptr, data.Length));
            packed = fn(ptr, data.Length);
        }
        catch (WasmtimeException e)
        {
            throw new CoreException($"trap: {e.Message}", e);
        }

        var outPtr = (int)((ulong)packed >> 32);
        var outLen = (int)(packed & 0xffffffff);
        var raw = this.memory.GetSpan(outPtr, outLen).ToArray();
        this.free(outPtr, outLen);
        return raw;
    }
}
