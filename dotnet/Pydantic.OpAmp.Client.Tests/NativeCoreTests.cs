// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Globalization;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Core;
using Xunit;
using Config = Pydantic.OpAmp.Core.Config;
using Header = Pydantic.OpAmp.Core.Header;
using Reply = Pydantic.OpAmp.Core.Reply;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The native core is held to the WebAssembly module it replaced: for seeded
/// runs of configs and inputs, plausible and not, every reply is the module's, byte for byte.
/// The module is what the conformance suite, the taps and the other SDKs check, so this carries
/// their checks over to the native build.
/// </summary>
public class NativeCoreTests
{
    [Fact]
    public void EveryReplyIsTheModulesByteForByte()
    {
        var seeds = Env("OPAMP_NATIVE_SEEDS") ?? 300;
        var first = Env("OPAMP_NATIVE_SEED") ?? 1;
        var module = WasmCore.Module();
        for (var seed = first; seed < first + seeds; seed++)
        {
            var rng = new Random((int)seed);
            using var native = new NativeCore();
            using var wasm = module();
            var config = RandomConfig(rng).ToByteArray();
            Same(seed, "init", native.Init(config), wasm.Init(config));
            ulong now = 0;
            for (var i = 0; i < 80; i++)
            {
                now += (ulong)rng.Next(0, 40_000);
                var step = rng.Next(30) == 0
                    ? Bytes(rng, 40) // garbage: an error in the reply, the same one
                    : new Step { NowMs = now, UnixMs = 1_760_000_000_000 + now, Input = RandomInput(rng) }.ToByteArray();
                Same(seed, $"step {i}", native.Step(step), wasm.Step(step));
            }
        }
    }

    [Fact]
    public void SdkSettingsRepliesAreTheModulesByteForByte()
    {
        using var wasm = new WasmSdkSettings();
        var rng = new Random(7);
        var requests = new List<byte[]> { Array.Empty<byte>(), new byte[] { 0xff, 0xff } };
        for (var i = 0; i < 200; i++)
        {
            requests.Add(Bytes(rng, 60));
        }

        requests.Add(new Request { Load = new Load() }.ToByteArray());
        foreach (var r in requests)
        {
            Assert.Equal(Convert.ToHexString(wasm.Call(r)), Convert.ToHexString(Native.SdkSettingsCall(r)));
        }
    }

    [Fact]
    public void SessionsAreIndependentAndOutliveEachOther()
    {
        using var a = new NativeCore();
        var b = new NativeCore();
        Reply.Parser.ParseFrom(a.Init(new Config { InstanceUid = ByteString.CopyFrom(Enumerable.Repeat((byte)1, 16).ToArray()), Endpoint = "http://server/v1/opamp" }.ToByteArray()));
        Reply.Parser.ParseFrom(b.Init(new Config { InstanceUid = ByteString.CopyFrom(Enumerable.Repeat((byte)2, 16).ToArray()), Endpoint = "http://server/v1/opamp" }.ToByteArray()));
        b.Dispose();
        var step = new Step { Input = new Input { Start = new Empty() } }.ToByteArray();
        Assert.Equal(Enumerable.Repeat((byte)1, 16), Reply.Parser.ParseFrom(a.Step(step)).Output.InstanceUid.ToByteArray());
    }

    [Fact]
    public void AStepBeforeInitIsAnErrorNotACrash()
    {
        using var core = new NativeCore();
        Assert.Equal("step before init", Reply.Parser.ParseFrom(core.Step([])).Error);
    }

    private static void Same(ulong seed, string what, byte[] native, byte[] wasm)
    {
        if (!native.AsSpan().SequenceEqual(wasm))
        {
            Assert.Fail($"seed {seed} (OPAMP_NATIVE_SEED={seed} OPAMP_NATIVE_SEEDS=1), {what}: the native reply differs from the module's\n" +
                        $"native: {Describe(native)}\nmodule: {Describe(wasm)}");
        }
    }

    private static string Describe(byte[] reply)
    {
        try
        {
            return Reply.Parser.ParseFrom(reply).ToString();
        }
        catch (InvalidProtocolBufferException)
        {
            return Convert.ToHexString(reply);
        }
    }

    private static Config RandomConfig(Random rng)
    {
        var c = new Config
        {
            Endpoint = rng.Next(2) == 0 ? "http://server.test/v1/opamp" : "ws://server.test/v1/opamp",
            Entropy = ByteString.CopyFrom(Bytes(rng, 40, min: 32)),
            Streams = rng.Next(2) == 0,
            Persistence = rng.Next(2) == 0,
            AcceptConnectionSettings = rng.Next(2) == 0,
            RequestInstanceUid = rng.Next(4) == 0,
            HeartbeatIntervalMs = (ulong)rng.Next(0, 60_000),
            Seed = (ulong)rng.NextInt64(),
        };
        c.AcceptPackages = c.Persistence && rng.Next(2) == 0;
        c.SpliceDownloads = c.Streams && rng.Next(2) == 0;
        if (rng.Next(2) == 0)
        {
            c.InstanceUid = ByteString.CopyFrom(Bytes(rng, 16, min: 16));
        }

        if (rng.Next(3) == 0)
        {
            c.Headers.Add(new Header { Name = "Authorization", Value = "Bearer t" });
        }

        return c;
    }

    private static Input RandomInput(Random rng)
    {
        ulong N() => (ulong)rng.Next(0, 6);
        return rng.Next(17) switch
        {
            0 => new Input { Start = new Empty() },
            1 or 2 => new Input { Timer = new Empty() },
            3 => new Input { HttpResponse = new HttpResponse { Id = N(), Status = Status(rng), Body = ByteString.CopyFrom(ServerMessage(rng)), Headers = { RetryAfter(rng) } } },
            4 => new Input { HttpFailed = new Failed { Id = N(), Detail = "refused" } },
            5 => new Input { WsOpened = new Id { Id_ = N() } },
            6 => new Input { WsMessage = new WsMessage { Id = N(), Data = ByteString.CopyFrom([0, .. ServerMessage(rng)]) } },
            7 => new Input { WsClosed = new WsClosed { Id = N(), Code = 1000 } },
            8 => new Input { WsLost = new Failed { Id = N(), Detail = "reset" } },
            9 => new Input { Stored = new Id { Id_ = N() } },
            10 => new Input { SetHealth = new ComponentHealth { Healthy = rng.Next(2) == 0, Status = "s" } },
            11 => new Input { StreamOpened = new Id { Id_ = N() } },
            12 => new Input { StreamTlsReady = new Id { Id_ = N() } },
            13 => new Input { StreamData = new WsMessage { Id = N(), Data = ByteString.CopyFrom(HttpBytes(rng)) } },
            14 => new Input { StreamEof = new Id { Id_ = N() } },
            15 => new Input { ProxyAnswer = new ProxyAnswer { Id = N() } },
            _ => new Input { Stop = new Empty() },
        };
    }

    private static uint Status(Random rng) => rng.Next(5) switch
    {
        0 => 503,
        1 => 429,
        2 => (uint)rng.Next(100, 600),
        _ => 200,
    };

    private static IEnumerable<Header> RetryAfter(Random rng) => rng.Next(4) switch
    {
        0 => [new Header { Name = "Retry-After", Value = rng.Next(0, 120).ToString(CultureInfo.InvariantCulture) }],
        1 => [new Header { Name = "Retry-After", Value = "Wed, 21 Oct 2026 07:28:00 GMT" }],
        _ => [],
    };

    /// <summary>A ServerToAgent with some of what a server says, or garbage.</summary>
    private static byte[] ServerMessage(Random rng)
    {
        if (rng.Next(6) == 0)
        {
            return Bytes(rng, 30);
        }

        var m = new ServerToAgent();
        if (rng.Next(3) == 0)
        {
            m.Flags = (ulong)ServerToAgentFlags.ReportFullState;
        }

        if (rng.Next(3) == 0)
        {
            m.AgentIdentification = new AgentIdentification { NewInstanceUid = ByteString.CopyFrom(Bytes(rng, 16, min: 16)) };
        }

        if (rng.Next(3) == 0)
        {
            m.RemoteConfig = new AgentRemoteConfig
            {
                ConfigHash = ByteString.CopyFrom(Bytes(rng, 4, min: 1)),
                Config = new AgentConfigMap { ConfigMap = { ["a"] = new AgentConfigObject { Body = ByteString.CopyFrom(Bytes(rng, 8)) } } },
            };
        }

        if (rng.Next(4) == 0)
        {
            m.ErrorResponse = new ServerErrorResponse
            {
                Type = ServerErrorResponseType.Unavailable,
                RetryInfo = new RetryInfo { RetryAfterNanoseconds = (ulong)rng.Next(0, 5) * 1_000_000_000 },
            };
        }

        if (rng.Next(4) == 0)
        {
            m.ConnectionSettings = new ConnectionSettingsOffers
            {
                Hash = ByteString.CopyFrom(Bytes(rng, 4, min: 1)),
                Opamp = new OpAMPConnectionSettings { HeartbeatIntervalSeconds = (ulong)rng.Next(0, 60) },
            };
        }

        return m.ToByteArray();
    }

    /// <summary>Bytes on a stream: an HTTP response with a server's message, part of one, or garbage.</summary>
    private static byte[] HttpBytes(Random rng)
    {
        var body = ServerMessage(rng);
        var head = System.Text.Encoding.ASCII.GetBytes(
            $"HTTP/1.1 {Status(rng)} X\r\nContent-Type: application/x-protobuf\r\nContent-Length: {body.Length}\r\n\r\n");
        byte[] all = [.. head, .. body];
        return rng.Next(4) switch
        {
            0 => Bytes(rng, 50),
            1 => all[..rng.Next(all.Length + 1)],
            _ => all,
        };
    }

    private static byte[] Bytes(Random rng, int max, int min = 0)
    {
        var b = new byte[rng.Next(min, max + 1)];
        rng.NextBytes(b);
        return b;
    }

    private static ulong? Env(string name) =>
        ulong.TryParse(Environment.GetEnvironmentVariable(name), NumberStyles.None, CultureInfo.InvariantCulture, out var v) ? v : null;

    /// <summary>The SDK settings module (opamp_sdk_settings.wasm), as the oracle for the native layer.</summary>
    private sealed class WasmSdkSettings : IDisposable
    {
        private readonly Wasmtime.Store store;
        private readonly Wasmtime.Memory memory;
        private readonly Func<int, int> alloc;
        private readonly Action<int, int> free;
        private readonly Func<int, int, long> call;

        public WasmSdkSettings()
        {
            var engine = new Wasmtime.Engine();
            var module = Wasmtime.Module.FromBytes(engine, "sdk", File.ReadAllBytes(Path.Combine(AppContext.BaseDirectory, "opamp_sdk_settings.wasm")));
            this.store = new Wasmtime.Store(engine);
            var instance = new Wasmtime.Instance(this.store, module);
            this.memory = instance.GetMemory("memory")!;
            this.alloc = instance.GetFunction<int, int>("alloc")!;
            this.free = instance.GetAction<int, int>("free")!;
            this.call = instance.GetFunction<int, int, long>("call")!;
        }

        public byte[] Call(byte[] data)
        {
            var ptr = this.alloc(data.Length);
            data.CopyTo(this.memory.GetSpan(ptr, data.Length));
            var packed = this.call(ptr, data.Length);
            var outPtr = (int)((ulong)packed >> 32);
            var outLen = (int)(packed & 0xffffffff);
            var raw = this.memory.GetSpan(outPtr, outLen).ToArray();
            this.free(outPtr, outLen);
            return raw;
        }

        public void Dispose() => this.store.Dispose();
    }
}
