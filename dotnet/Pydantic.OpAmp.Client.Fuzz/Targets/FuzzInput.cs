// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Client.Fuzz;

/// <summary>
/// What the SharpFuzz targets (<see cref="ClientFuzz"/>, <see cref="CoreFuzz"/>) share, as
/// python/tests/fuzz/_fuzzlib.py does for Atheris and FuzzInput.java for Jazzer: a reader of the
/// fuzzer's bytes (0 past their end), the server that answers from them (any bytes, or a
/// ServerToAgent built from them and perhaps damaged, an error status, a reset), and the
/// application's calls a callback makes. The reader is shared by the client's loop (the callback)
/// and the server's tasks: each read is one at a time.
/// </summary>
internal static class FuzzInput
{
    public const string Capability = "io.example.fuzz";
    public const string Files = "http://files.test";
    public static readonly int[] Statuses = [204, 301, 302, 400, 401, 403, 404, 429, 500, 503];

    public static readonly ulong Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig
        | AgentCapabilities.AcceptsPackages | AgentCapabilities.ReportsPackageStatuses | AgentCapabilities.AcceptsOpAmpconnectionSettings
        | AgentCapabilities.AcceptsRestartCommand | AgentCapabilities.ReportsHealth | AgentCapabilities.ReportsEffectiveConfig
        | AgentCapabilities.ReportsAvailableComponents | AgentCapabilities.AcceptsOtherConnectionSettings
        | AgentCapabilities.ReportsConnectionSettingsStatus);

    public static readonly string[] Calls =
    [
        "SetHealth", "SetDescription", "SetEffectiveConfig", "SetRemoteConfigStatus", "PackageInstalled", "PackageRemoved",
        "SetConnectionSettingsStatus", "SendCustomMessage", "SetCustomCapabilities", "SetAvailableComponents", "Stop", "Nothing",
    ];

    public static byte[] Sha256(byte[] b) => SHA256.HashData(b);

    /// <summary>Where a body of <paramref name="size"/> bytes is cut as it arrives (up to three cuts).</summary>
    public static List<int> Cuts(Reader r, int size)
    {
        var at = new List<int>();
        if (size > 0)
        {
            for (var n = r.U8() % 4; n > 0; n--)
            {
                at.Add(r.U16() % (size + 1));
            }
        }

        return at;
    }

    /// <summary><paramref name="data"/> in pieces, cut at <paramref name="at"/>.</summary>
    public static List<byte[]> Pieces(byte[] data, IEnumerable<int> at)
    {
        var output = new List<byte[]>();
        if (data.Length == 0)
        {
            return output;
        }

        var from = 0;
        foreach (var m in at.Where(a => a > 0 && a < data.Length).Distinct().Order())
        {
            output.Add(data[from..m]);
            from = m;
        }

        output.Add(data[from..]);
        return output;
    }

    /// <summary><paramref name="b"/> as a header name: no control characters, spaces or colons.</summary>
    public static string Token(byte[] b) => new(b.Where(x => x > 32 && x < 127 && x != ':').Select(x => (char)x).ToArray());

    /// <summary>
    /// <paramref name="b"/> as a header value HTTP/1.1 parsers take, read as Latin-1: no control
    /// characters but tab; bytes 128 to 255 (obs-text) are allowed.
    /// </summary>
    public static string Text(byte[] b) => new string(b.Where(c => c == 9 || (c >= 32 && c != 127)).Select(c => (char)c).ToArray()).Trim();

    public static List<KeyValue> Attributes(Reader r)
    {
        var output = new List<KeyValue>();
        for (var n = r.U8() % 3; n > 0; n--)
        {
            var v = (r.U8() % 4) switch
            {
                0 => new AnyValue { StringValue = r.Text(32) },
                1 => new AnyValue { IntValue = (long)r.U64() },
                2 => new AnyValue { BytesValue = r.Bytes(32) },
                _ => new AnyValue { KvlistValue = new KeyValueList { Values = { new KeyValue { Key = r.Text(8), Value = new AnyValue { BoolValue = r.Bit() } } } } },
            };
            output.Add(new KeyValue { Key = r.Text(16), Value = v });
        }

        return output;
    }

    /// <summary>
    /// The application's call <paramref name="what"/>, its arguments from the fuzzer's bytes and
    /// the last notification of each kind the callback was told (<paramref name="seen"/>). True:
    /// stop was asked for (the caller carries it out).
    /// </summary>
    public static bool Call(Reader r, OpAmpClient c, string what, Dictionary<Type, OpAmpNotification> seen)
    {
        switch (what)
        {
            case "SetHealth":
                c.SetHealth(new ComponentHealth
                {
                    Healthy = r.Bit(), Status = r.Text(32), LastError = r.Text(32), StartTimeUnixNano = r.U64(), StatusTimeUnixNano = r.U64(),
                });
                break;
            case "SetDescription":
                c.SetDescription(new AgentDescription { IdentifyingAttributes = { Attributes(r) }, NonIdentifyingAttributes = { Attributes(r) } });
                break;
            case "SetEffectiveConfig":
                {
                    var cm = new AgentConfigMap();
                    cm.ConfigMap[r.Text(8)] = new AgentConfigObject { Body = r.Bytes(256) };
                    c.SetEffectiveConfig(new EffectiveConfig { ConfigMap = cm });
                    break;
                }

            case "SetRemoteConfigStatus":
                {
                    var h = seen.GetValueOrDefault(typeof(OpAmpNotification.RemoteConfig)) is OpAmpNotification.RemoteConfig rc && r.Bit()
                        ? rc.Config.ConfigHash : r.Bytes(32);
                    c.SetRemoteConfigStatus(new RemoteConfigStatus { LastRemoteConfigHash = h, Status = (RemoteConfigStatuses)(r.U8() % 5), ErrorMessage = r.Text(32) });
                    break;
                }

            case "PackageInstalled":
                {
                    string name;
                    byte[] hash;
                    if (seen.GetValueOrDefault(typeof(OpAmpNotification.InstallPackage)) is OpAmpNotification.InstallPackage ip && r.U8() % 4 != 0)
                    {
                        name = ip.Name;
                        hash = r.U8() % 4 != 0 ? ip.Package.Hash.ToByteArray() : r.Blob(16);
                    }
                    else
                    {
                        name = r.Pick(["pkg", "other", string.Empty, r.Text(16)]);
                        hash = r.Blob(16);
                    }

                    c.PackageInstalled(name, hash, r.Bit() ? null : r.Text(32));
                    break;
                }

            case "PackageRemoved":
                c.PackageRemoved(seen.GetValueOrDefault(typeof(OpAmpNotification.RemovePackage)) is OpAmpNotification.RemovePackage rp && r.Bit() ? rp.Name : r.Text(16));
                break;
            case "SetConnectionSettingsStatus":
                {
                    var h = seen.GetValueOrDefault(typeof(OpAmpNotification.ConnectionSettings)) is OpAmpNotification.ConnectionSettings cs && r.Bit()
                        ? cs.Settings.Hash : r.Bytes(16);
                    c.SetConnectionSettingsStatus(new ConnectionSettingsStatus
                    {
                        LastConnectionSettingsHash = h, Status = (ConnectionSettingsStatuses)(r.U8() % 4), ErrorMessage = r.Text(32),
                    });
                    break;
                }

            case "SendCustomMessage":
                c.SendCustomMessage(new CustomMessage { Capability = r.Pick([Capability, r.Text(24)]), Type = r.Text(16), Data = r.Bytes(512) });
                break;
            case "SetCustomCapabilities":
                {
                    var cc = new CustomCapabilities();
                    for (var n = r.U8() % 3; n > 0; n--)
                    {
                        cc.Capabilities.Add(r.Pick([Capability, r.Text(24)]));
                    }

                    c.SetCustomCapabilities(cc);
                    break;
                }

            case "SetAvailableComponents":
                {
                    var ac = new AvailableComponents();
                    for (var n = r.U8() % 3; n > 0; n--)
                    {
                        ac.Components[r.Text(16)] = new ComponentDetails { Metadata = { Attributes(r) } };
                    }

                    c.SetAvailableComponents(ac);
                    break;
                }

            case "Stop":
                return true;
        }

        return false;
    }

    /// <summary>The fuzzer's bytes, read front to back; 0 past the end.</summary>
    internal sealed class Reader(byte[] data)
    {
        private readonly object gate = new();
        private int i;

        public bool Empty
        {
            get
            {
                lock (this.gate)
                {
                    return this.i >= data.Length;
                }
            }
        }

        public int U8()
        {
            lock (this.gate)
            {
                return this.i < data.Length ? data[this.i++] : 0;
            }
        }

        public int U16()
        {
            lock (this.gate)
            {
                return this.U8() | (this.U8() << 8);
            }
        }

        public ulong U32()
        {
            lock (this.gate)
            {
                return (uint)(this.U16() | (this.U16() << 16));
            }
        }

        public ulong U64()
        {
            lock (this.gate)
            {
                return this.U32() | (this.U32() << 32);
            }
        }

        public byte[] Take(int n)
        {
            lock (this.gate)
            {
                var k = Math.Max(0, Math.Min(n, data.Length - this.i));
                var output = data[this.i..(this.i + k)];
                this.i += k;
                return output;
            }
        }

        /// <summary>A length (one byte, or two when <paramref name="most"/> is over 255), then that many bytes.</summary>
        public byte[] Blob(int most)
        {
            lock (this.gate)
            {
                var n = most <= 255 ? this.U8() : this.U16();
                return this.Take(Math.Min(n, most));
            }
        }

        public ByteString Bytes(int most) => ByteString.CopyFrom(this.Blob(most));

        /// <summary>protobuf strings are UTF-8: what isn't becomes U+FFFD.</summary>
        public string Text(int most) => Encoding.UTF8.GetString(this.Blob(most));

        public T Pick<T>(IReadOnlyList<T> choices)
        {
            lock (this.gate)
            {
                return choices[this.U8() % choices.Count];
            }
        }

        public bool Bit() => (this.U8() & 1) != 0;
    }

    /// <summary>An answer: how ("ok", "reset", "garbage": all the server writes, status line included).</summary>
    internal sealed record Answer(string How, int Status, List<(string Name, string Value)> Headers, byte[] Data, List<int> Cuts);

    /// <summary>
    /// What the server does with a WebSocket upgrade ("ok", "status": refused, "reset"), or with a
    /// message on one ("message": a frame's payload; "raw": any bytes, as frames or not, where the
    /// client reads frames itself, elsewhere a message's payload; "close", with its code; "drop").
    /// </summary>
    internal sealed record Ws(string How, int Status, List<(string Name, string Value)> Headers, byte[] Data, int Code);

    /// <summary>An OpAMP server whose every answer is the fuzzer's; package files it offers kept by URL.</summary>
    internal sealed class Server(Reader r)
    {
        private readonly object gate = new();
        private int answers;

        public ConcurrentDictionary<string, byte[]> Offered { get; } = new();

        public int Answers => Volatile.Read(ref this.answers);

        /// <summary>Headers of the server's own, perhaps none (a Latin-1 Server or Set-Cookie, say).</summary>
        public List<(string Name, string Value)> Headers()
        {
            lock (this.gate)
            {
                var output = new List<(string, string)>();
                for (var n = (r.U8() & 1) != 0 ? r.U8() % 3 : 0; n > 0; n--)
                {
                    var name = Token(r.Pick([Encoding.ASCII.GetBytes("Server"), Encoding.ASCII.GetBytes("Set-Cookie"), Encoding.ASCII.GetBytes("X-Fuzz"), r.Blob(8)]));
                    var value = Text(r.Blob(32));
                    if (name.Length > 0)
                    {
                        output.Add((name, value));
                    }
                }

                return output;
            }
        }

        public Ws Upgrade()
        {
            lock (this.gate)
            {
                return (r.U8() % 6) switch
                {
                    0 => new Ws("reset", 0, [], [], 0),
                    1 => new Ws("status", r.Pick([200, 301, 400, 401, 403, 404, 426, 429, 500, 503]), this.Headers(), [], 0),
                    _ => new Ws("ok", 101, [], [], 0),
                };
            }
        }

        public Ws WsAnswer(byte[] body)
        {
            lock (this.gate)
            {
                var msg = body.Length > 0 && body[0] == 0 ? body[1..] : body;
                var a = this.Respond(msg);
                if (a.How == "reset")
                {
                    return new Ws("drop", 0, [], [], 0);
                }

                if (a.How == "garbage")
                {
                    return new Ws("raw", 0, [], a.Data, 0);
                }

                if (a.Status != 200)
                {
                    return new Ws("close", 0, [], [], r.Pick([1000, 1001, 1002, 1008, 1011, 3000, 4999, 0]));
                }

                return new Ws("message", 0, [], [0, .. a.Data], 0);
            }
        }

        public Answer Respond(byte[] body)
        {
            lock (this.gate)
            {
                this.answers++;
                ByteString uid;
                try
                {
                    uid = AgentToServer.Parser.ParseFrom(body).InstanceUid;
                }
                catch (InvalidProtocolBufferException)
                {
                    uid = ByteString.Empty;
                }

                var kind = r.U8() % 10;
                var status = 200;
                var headers = this.Headers();
                byte[] data;
                switch (kind)
                {
                    case 0:
                        data = new ServerToAgent { InstanceUid = uid }.ToByteArray();
                        break;
                    case 1:
                        data = r.Blob(4096);
                        break;
                    case 2:
                        return new Answer("reset", 0, headers, [], []);
                    case 3:
                        status = Statuses[r.U8() % Statuses.Length];
                        if (r.Bit())
                        {
                            headers.Add(("Retry-After", r.Pick(["0", "1", "120", "Wed, 21 Oct 2037 07:28:00 GMT", "soon", r.U32().ToString(System.Globalization.CultureInfo.InvariantCulture)])));
                        }

                        if (status is 301 or 302)
                        {
                            headers.Add(("Location", r.Pick(["http://opamp.test/v2", "/elsewhere", "https://x.test/", string.Empty])));
                        }

                        data = r.Blob(255);
                        break;
                    case 4:
                        if (!r.Empty)
                        {
                            return new Answer("garbage", 0, headers, r.Blob(1024), []);
                        }

                        data = this.Message(uid).ToByteArray();
                        break;
                    default:
                        data = this.Message(uid).ToByteArray();
                        if (kind == 9 && data.Length > 0)
                        {
                            for (var n = 1 + (r.U8() % 3); n > 0; n--)
                            {
                                var at = r.U16() % data.Length;
                                var x = r.U8();
                                data[at] ^= (byte)(x == 0 ? 1 : x);
                            }

                            if (r.Bit())
                            {
                                data = data[..(r.U16() % data.Length)];
                            }
                        }

                        break;
                }

                return new Answer("ok", status, headers, data, Cuts(r, data.Length));
            }
        }

        public Answer Download(string url)
        {
            lock (this.gate)
            {
                var found = this.Offered.TryGetValue(url, out var body);
                var how = r.U8() % 8;
                var headers = this.Headers();
                if (!found || how == 1)
                {
                    return new Answer("ok", 404, headers, Encoding.UTF8.GetBytes("missing"), []);
                }

                if (how == 2)
                {
                    return new Answer("reset", 0, [], [], []);
                }

                if (how == 3)
                {
                    // A redirect, to anywhere (a Location of any bytes but control characters).
                    headers.Add(("Location", r.Pick([Files + "/moved", "/p1", "//other.test/p", "http://[bad", Text(r.Blob(40))])));
                    return new Answer("ok", Statuses[r.U8() % Statuses.Length], headers, [], []);
                }

                if (how == 4)
                {
                    body = body!.Length == 0 ? [(byte)'x'] : (byte[])body.Clone();
                    body[^1] ^= 1; // not the file offered
                }

                return new Answer("ok", 200, headers, body!, Cuts(r, body!.Length));
            }
        }

        public ServerToAgent Message(ByteString uid)
        {
            var parts = r.U16();
            var m = new ServerToAgent { InstanceUid = (parts & 1) == 0 ? uid : r.Bytes(20) };
            if ((parts & 2) != 0)
            {
                var cm = new AgentConfigMap();
                for (var n = r.U8() % 3; n > 0; n--)
                {
                    cm.ConfigMap[r.Text(16)] = new AgentConfigObject { Body = r.Bytes(512), ContentType = r.Text(16) };
                }

                m.RemoteConfig = new AgentRemoteConfig { ConfigHash = r.Bytes(32), Config = cm };
            }

            if ((parts & 4) != 0)
            {
                m.PackagesAvailable = this.Packages();
            }

            if ((parts & 8) != 0)
            {
                var cs = new ConnectionSettingsOffers { Hash = r.Bytes(16) };
                if (r.Bit())
                {
                    var o = new OpAMPConnectionSettings
                    {
                        DestinationEndpoint = r.Pick(["http://opamp.test/v1/opamp", "http://other.test/opamp", "ws://opamp.test/v1/opamp", r.Text(40)]),
                        HeartbeatIntervalSeconds = r.Pick([0UL, 1UL, 30UL, 3600UL, 1UL << 63, ulong.MaxValue, r.U32()]),
                        Headers = new global::OpAmp.Proto.V1.Headers(),
                    };
                    for (var n = r.U8() % 3; n > 0; n--)
                    {
                        o.Headers.Headers_.Add(new Header { Key = r.Text(16), Value = r.Text(32) });
                    }

                    cs.Opamp = o;
                }

                if (r.Bit())
                {
                    cs.OwnMetrics = new TelemetryConnectionSettings
                    {
                        DestinationEndpoint = r.Text(40),
                        Headers = new global::OpAmp.Proto.V1.Headers { Headers_ = { new Header { Key = r.Text(16), Value = r.Text(32) } } },
                    };
                }

                for (var n = r.U8() % 3; n > 0; n--)
                {
                    cs.OtherConnections[r.Text(16)] = new OtherConnectionSettings { DestinationEndpoint = r.Text(40) };
                }

                m.ConnectionSettings = cs;
            }

            if ((parts & 16) != 0)
            {
                m.Flags = r.Pick([1UL, 2UL, 3UL, r.U64()]);
            }

            if ((parts & 32) != 0)
            {
                m.Capabilities = r.U64();
            }

            if ((parts & 64) != 0)
            {
                m.AgentIdentification = new AgentIdentification
                {
                    NewInstanceUid = r.Pick([ByteString.CopyFrom(new byte[16]), ByteString.CopyFrom(Enumerable.Range(0, 16).Select(x => (byte)x).ToArray()), r.Bytes(20)]),
                };
            }

            if ((parts & 128) != 0)
            {
                m.Command = new ServerToAgentCommand { Type = (CommandType)(r.U8() % 3) };
            }

            if ((parts & 256) != 0)
            {
                var cc = new CustomCapabilities();
                for (var n = r.U8() % 4; n > 0; n--)
                {
                    cc.Capabilities.Add(r.Text(24));
                }

                m.CustomCapabilities = cc;
            }

            if ((parts & 512) != 0)
            {
                m.CustomMessage = new CustomMessage { Capability = r.Pick([Capability, r.Text(24)]), Type = r.Text(16), Data = r.Bytes(256) };
            }

            if ((parts & 1024) != 0)
            {
                var e = new ServerErrorResponse { Type = (ServerErrorResponseType)(r.U8() % 5), ErrorMessage = r.Text(64) };
                if (r.Bit())
                {
                    e.RetryInfo = new RetryInfo { RetryAfterNanoseconds = r.U64() };
                }

                m.ErrorResponse = e;
            }

            return m;
        }

        private PackagesAvailable Packages()
        {
            var offer = new PackagesAvailable { AllPackagesHash = r.Bytes(16) };
            for (var n = r.U8() % 3; n > 0; n--)
            {
                var name = r.Pick(["pkg", "other", string.Empty, "../up", "a/b", r.Text(16)]);
                var p = new PackageAvailable { Type = (PackageType)(r.U8() % 3), Version = r.Text(8), Hash = r.Bytes(16) };
                if (r.U8() % 4 != 0)
                {
                    var k = this.Offered.Count + 1;
                    var url = r.Pick([$"{Files}/p{k}", $"{Files}/p{k}?x=1", string.Empty, "ftp://files.test/p", r.Text(40)]);
                    var body = r.Blob(2048);
                    if (url.StartsWith(Files, StringComparison.Ordinal))
                    {
                        this.Offered[url] = body;
                    }

                    var sum = ByteString.CopyFrom(Sha256(body));
                    var f = new DownloadableFile
                    {
                        DownloadUrl = url, ContentHash = r.Pick([sum, sum, ByteString.Empty, r.Bytes(32)]), Signature = r.Bytes(16),
                    };
                    if (r.Bit())
                    {
                        f.Headers = new global::OpAmp.Proto.V1.Headers { Headers_ = { new Header { Key = r.Text(16), Value = r.Text(32) } } };
                    }

                    p.File = f;
                }

                offer.Packages[name] = p;
            }

            return offer;
        }
    }
}
