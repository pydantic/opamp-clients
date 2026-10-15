// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client.Fuzz;

/// <summary>
/// SharpFuzz target: the C interface's buffer handling (<see cref="Native"/>, <see cref="NativeCore"/>
/// and <see cref="Sdk.SdkSettings"/>), with any bytes on both sides of it. One input is a session of
/// the native core driven through this assembly's own wrappers: a Config (any bytes, or a valid one
/// with any saved state), then steps (any bytes, or valid Steps whose inputs carry any
/// server-to-agent bytes, up to megabytes, as a response, a WebSocket message or a stream's data),
/// and calls into the SDK settings layer (load any saved document, decide on any remote config,
/// describe, or any request bytes at all). What must hold: every call hands back bytes the wrapper
/// copied out and freed, which decode as a Reply; the core never traps, and neither does the SDK
/// settings layer (a refusal is a Reply's error, which <see cref="Sdk.SdkSettings"/> throws as a
/// <see cref="CoreException"/> that isn't a trap).
/// <para>scripts/dotnet_fuzz.sh runs it under SharpFuzz; the tests' FuzzInputsTests run
/// <see cref="Seeds"/> and every input under crashes/CoreFuzz/ with every <c>dotnet test</c>.</para>
/// </summary>
internal static class CoreFuzz
{
    public static readonly byte[][] Seeds =
    [
        [],
        [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 1, 1, 2, 1, 0, 3, (byte)'a', (byte)'b', (byte)'c', 1],
        [2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 0, 5, (byte)'h', (byte)'e', (byte)'l', (byte)'l', (byte)'o', 1, 0, 2, 0, 1, 3, 0, 3, 0, 0, 0, 1, 8, 7, 0x0a, 3, (byte)'x', (byte)'y', (byte)'z'],
        [1, 5, 0x0a, 3, (byte)'a', (byte)'b', (byte)'c', 1, 0, 0, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
    ];

    // Made on first use: instrumented code runs only once the fuzzer has started (its coverage map).
    private static readonly Lazy<Sdk.SdkSettings> Layer = new(() => new Sdk.SdkSettings());

    public static void Run(ReadOnlySpan<byte> data)
    {
        var r = new FuzzInput.Reader(data.ToArray());
        using var core = new NativeCore();
        Reply(Call(() => core.Init(Config(r)), "init"), "init");
        ulong now = 0;
        while (!r.Empty)
        {
            if (r.U8() % 5 == 0)
            {
                Settings(r);
                continue;
            }

            now += (ulong)r.U8() * 100;
            var step = Step(r, now);
            Reply(Call(() => core.Step(step), "step"), "step");
        }
    }

    private static byte[] Call(Func<byte[]> f, string what)
    {
        try
        {
            return f();
        }
        catch (CoreException e)
        {
            throw new ClientFuzz.Problem($"{what} trapped: {e.Message}");
        }
    }

    private static void Reply(byte[] raw, string what)
    {
        try
        {
            Pydantic.OpAmp.Core.Reply.Parser.ParseFrom(raw);
        }
        catch (InvalidProtocolBufferException e)
        {
            throw new ClientFuzz.Problem($"{what}: {raw.Length} bytes back that aren't a Reply: {e.Message}");
        }
    }

    private static byte[] Config(FuzzInput.Reader r)
    {
        if ((r.U8() & 1) != 0)
        {
            return r.Blob(2048);
        }

        var c = new Config
        {
            Endpoint = r.Pick(["http://s.test/v1/opamp", "ws://s.test/v1/opamp", r.Text(40)]),
            Entropy = ByteString.CopyFrom(new byte[32]),
            UnixMs = 1_790_000_000_000,
            Seed = r.U64(),
            Capabilities = r.U64(),
            Persistence = r.Bit(),
            AcceptPackages = r.Bit(),
            AcceptConnectionSettings = r.Bit(),
            Streams = r.Bit(),
            MaxReceiveBytes = r.Pick([0UL, 1UL, 1024UL, ulong.MaxValue]),
        };
        if (c.Persistence)
        {
            c.SavedState = r.Bytes(1024);
        }

        return c.ToByteArray();
    }

    /// <summary>Bytes a server may send: any, or one piece repeated up to megabytes.</summary>
    private static ByteString Payload(FuzzInput.Reader r)
    {
        var b = r.Blob(4096);
        var times = r.U8() % 4 == 0 ? 1 + (r.U16() % 512) : 1;
        if (times == 1 || b.Length == 0)
        {
            return ByteString.CopyFrom(b);
        }

        var all = new byte[b.Length * times];
        for (var i = 0; i < times; i++)
        {
            b.CopyTo(all, i * b.Length);
        }

        return ByteString.CopyFrom(all);
    }

    private static byte[] Step(FuzzInput.Reader r, ulong now)
    {
        var i = new Input();
        switch (r.U8() % 8)
        {
            case 0:
                return r.Blob(2048);
            case 1:
                i.Start = new Empty();
                break;
            case 2:
                i.Timer = new Empty();
                break;
            case 3:
                i.HttpResponse = new HttpResponse { Id = (ulong)r.U8(), Status = r.Pick([200U, 200U, 204U, 404U, 503U, (uint)r.U32()]), Body = Payload(r) };
                break;
            case 4:
                i.WsMessage = new WsMessage { Id = (ulong)r.U8(), Data = Payload(r) };
                break;
            case 5:
                i.Stop = new Empty();
                break;
            case 6:
                i.StreamData = new WsMessage { Id = (ulong)r.U8(), Data = Payload(r) };
                break;
            default:
                i.WsOpened = new Id { Id_ = (ulong)r.U8() };
                break;
        }

        return new Step { NowMs = now, UnixMs = 1_790_000_000_000 + now, Input = i }.ToByteArray();
    }

    /// <summary>
    /// The SDK settings layer through its wrapper (the remote config handler of every SDK set up by
    /// this package): any request bytes, any saved document, any remote config, any resource.
    /// </summary>
    private static void Settings(FuzzInput.Reader r)
    {
        try
        {
            switch (r.U8() % 4)
            {
                case 0:
                    {
                        var raw = Native.SdkSettingsCall(r.Blob(1024));
                        try
                        {
                            Pydantic.OpAmp.Client.Sdk.Reply.Parser.ParseFrom(raw);
                        }
                        catch (InvalidProtocolBufferException e)
                        {
                            throw new ClientFuzz.Problem($"the SDK settings layer answered {raw.Length} bytes that aren't a Reply: {e.Message}");
                        }

                        break;
                    }

                case 1:
                    Layer.Value.Load(r.Bit() ? r.Blob(1024) : null);
                    break;
                case 2:
                    {
                        var m = new AgentConfigMap();
                        for (var n = r.U8() % 3; n > 0; n--)
                        {
                            m.ConfigMap[r.Text(16)] = new AgentConfigObject
                            {
                                Body = r.Bytes(1024), ContentType = r.Pick([string.Empty, "application/json", "text/yaml", r.Text(16)]),
                            };
                        }

                        Layer.Value.Decide(new AgentRemoteConfig { ConfigHash = r.Bytes(32), Config = m }, r.Bytes(512), r.Bytes(512), save: r.Bit() ? new Action<byte[]>(_ => { }) : null);
                        break;
                    }

                default:
                    {
                        var resource = new List<KeyValue>();
                        for (var n = r.U8() % 3; n > 0; n--)
                        {
                            resource.Add(new KeyValue { Key = r.Text(16), Value = new AnyValue { StringValue = r.Text(32) } });
                        }

                        Layer.Value.Describe(resource);
                        break;
                    }
            }
        }
        catch (CoreException e) when (!e.Message.StartsWith("trap", StringComparison.Ordinal))
        {
            // Refused, with the reason.
        }
        catch (CoreException e)
        {
            throw new ClientFuzz.Problem($"the SDK settings layer trapped: {e.Message}");
        }
    }
}
