// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net.Http.Headers;
using System.Reflection;
using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;
using static Pydantic.OpAmp.AutoInstrumentation.Tests.Otlp;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>scrub_defaults and scrub_patterns, at the traces and logs gates: attributes and log bodies.</summary>
public class ScrubbingTests
{
    private static Sink Send(Controls controls, string path, byte[] body)
    {
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = sink });
        client.Send(new HttpRequestMessage(HttpMethod.Post, "http://collector" + path)
        {
            Content = new ByteArrayContent(body) { Headers = { ContentType = new MediaTypeHeaderValue("application/x-protobuf") } },
        });
        return sink;
    }

    /// <summary>A LogRecord with this body and these attributes.</summary>
    private static byte[] Record(AnyValue body, params KeyValue[] attributes) => Message(o =>
    {
        o.WriteTag(2, WireFormat.WireType.Varint);
        o.WriteEnum(9);
        Nested(o, 5, body.ToByteArray());
        foreach (var a in attributes)
        {
            Nested(o, 6, a.ToByteArray());
        }
    });

    /// <summary>A logs export's records' bodies, whole.</summary>
    private static List<AnyValue> Bodies(byte[] message) =>
        [.. from resource in Fields(message).Where(f => f.Field == 1)
            from scopeLogs in Fields(resource.Bytes).Where(f => f.Field == 2)
            from record in Fields(scopeLogs.Bytes).Where(f => f.Field == 2)
            select AnyValue.Parser.ParseFrom(Fields(record.Bytes).Single(f => f.Field == 5).Bytes)];

    /// <summary>A vector's log body: a string (or another typed value), or a map of members, nested.</summary>
    private static AnyValue Body(JsonElement b)
    {
        if (b.TryGetProperty("map", out var map))
        {
            var list = new KeyValueList();
            list.Values.AddRange(map.EnumerateArray().Select(m => new KeyValue { Key = m.GetProperty("key").GetString(), Value = Body(m) }));
            return new AnyValue { KvlistValue = list };
        }

        return b.TryGetProperty("string", out var text) ? new AnyValue { StringValue = text.GetString() }
            : b.TryGetProperty("int", out var i) ? new AnyValue { IntValue = i.GetInt64() }
            : b.TryGetProperty("bool", out var x) ? new AnyValue { BoolValue = x.GetBoolean() }
            : b.TryGetProperty("double", out var d) ? new AnyValue { DoubleValue = d.GetDouble() }
            : new AnyValue { ArrayValue = new ArrayValue { Values = { b.GetProperty("array").EnumerateArray().Select(e => new AnyValue { StringValue = e.GetString() }) } } };
    }

    // Every case of testdata/scrub.json, through the gates: a span's attributes, its event's and its
    // link's, and a log record's, each scrubbed as the case wants; and the record's body.
    [Fact]
    public void EveryScrubVectorHoldsForSpansEventsLinksLogsAndBodies()
    {
        var failed = new List<string>();
        var cases = Vectors.Cases("scrub.json");
        foreach (var c in cases)
        {
            var name = c.GetProperty("name").GetString();
            var controls = new Controls();
            controls.Apply(Vectors.Settings(c));
            var attributes = c.GetProperty("attributes").EnumerateArray().Select(Vectors.Attribute).ToArray();
            var want = c.GetProperty("want").EnumerateArray().Select(Vectors.Attribute).ToList();
            var hasBody = c.TryGetProperty("body", out var b) && b.ValueKind == JsonValueKind.Object;
            var body = hasBody ? Body(b) : new AnyValue { StringValue = "b" };
            var wantBody = hasBody ? Body(c.GetProperty("want_body")) : body;

            var span = Assert.Single(ReadSpans(Assert.Single(Send(controls, "/v1/traces", Traces(Span("s", attributes, [attributes], [attributes]))).Exports).Message));
            var logs = Assert.Single(Send(controls, "/v1/logs", Logs(ScopeLogs("app", [Record(body, attributes)]))).Exports).Message;
            var record = Assert.Single(ReadLogs(logs));
            foreach (var (where, got) in new[] { ("span", span.Attributes), ("event", span.Events.Single()), ("link", span.Links.Single()), ("log", record.Attributes) })
            {
                if (!got.SequenceEqual(want))
                {
                    failed.Add($"{name} ({where}): {string.Join(", ", got)}");
                }
            }

            if (!Assert.Single(Bodies(logs)).Equals(wantBody))
            {
                failed.Add($"{name} (body): {Bodies(logs)[0]}");
            }
        }

        Assert.Empty(failed);
        if (Environment.GetEnvironmentVariable("OPAMP_SETTINGS_VECTORS") is null)
        {
            Assert.True(cases.Count >= 30, $"{cases.Count} cases: scrub.json was cut short");
        }
    }

    [Fact]
    public void ScrubbingChangesLiveAndLeftOutEveryValueGoesAsItCame()
    {
        var controls = new Controls();
        var export = Traces(Span("s", [Str("api_key", "k1"), Str("note", "my secret is x"), Str("iban", "DE89")]));
        List<KeyValue> Exported() => Assert.Single(ReadSpans(Assert.Single(Send(controls, "/v1/traces", export).Exports).Message)).Attributes;

        Assert.Equal([Str("api_key", "k1"), Str("note", "my secret is x"), Str("iban", "DE89")], Exported());
        controls.Apply(Vectors.Settings(Policy.OfEvery(("scrubbing", """{"defaults":true}"""))));
        Assert.Equal([Str("api_key", "[Scrubbed due to 'api_key']"), Str("note", "[Scrubbed due to 'secret']"), Str("iban", "DE89")], Exported());
        controls.Apply(Vectors.Settings(Policy.OfEvery(("scrubbing", """{"patterns":["IBAN"]}"""))));
        Assert.Equal([Str("api_key", "k1"), Str("note", "my secret is x"), Str("iban", "[Scrubbed due to 'iban']")], Exported());
        controls.Apply(new Settings());
        Assert.Equal(export, Assert.Single(Send(controls, "/v1/traces", export).Exports).Message);

        // A map body: each member as an attribute; a list's strings one by one, its other items kept.
        controls.Apply(Vectors.Settings(Policy.OfEvery(("scrubbing", """{"defaults":true}"""))));
        AnyValue Map(params KeyValue[] members)
        {
            var list = new KeyValueList();
            list.Values.AddRange(members);
            return new AnyValue { KvlistValue = list };
        }

        AnyValue Lines(params AnyValue[] items)
        {
            var list = new ArrayValue();
            list.Values.AddRange(items);
            return new AnyValue { ArrayValue = list };
        }

        var body = Map(Str("token", "t"), Str("cookie", "c"), new KeyValue { Key = "lines", Value = Lines(new AnyValue { StringValue = "wrong password" }, new AnyValue { IntValue = 3 }) });
        var logs = Assert.Single(Send(controls, "/v1/logs", Logs(ScopeLogs("app", [Record(body)]))).Exports).Message;
        Assert.Equal(
            Map(Str("token", "t"), Str("cookie", "[Scrubbed due to 'cookie']"), new KeyValue { Key = "lines", Value = Lines(new AnyValue { StringValue = "[Scrubbed due to 'password']" }, new AnyValue { IntValue = 3 }) }),
            Assert.Single(Bodies(logs)));
        // Nothing to scrub: byte for byte as it came.
        var plain = Logs(ScopeLogs("app", [Record(new AnyValue { StringValue = "fine" }, Str("n", "1"))]));
        Assert.Equal(plain, Assert.Single(Send(controls, "/v1/logs", plain).Exports).Message);
    }

    // A body nested past protobuf's own limit isn't well formed: the gate can't read it, so it
    // isn't sent (as any export it can't read while it changes what it sends), and nothing throws.
    [Fact]
    public void ABodyNestedTooDeepIsntReadAndIsntSent()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings(Policy.OfEvery(("scrubbing", """{"defaults":true}"""))));
        var deep = new AnyValue { StringValue = "my password" };
        for (var i = 0; i < 150; i++)
        {
            var list = new ArrayValue();
            list.Values.Add(deep);
            deep = new AnyValue { ArrayValue = list };
        }

        Assert.Empty(Send(controls, "/v1/logs", Logs(ScopeLogs("app", [Record(deep)]))).Exports);
    }

    [Fact]
    public void TheDefaultPatternsAtTheirEdges()
    {
        var s = Scrubber.Of(true, [])!;
        (string Text, string? Match)[] cases =
        [
            (string.Empty, null), ("password", null), ("a password", "password"), ("PassWord!", "PassWord"),
            ("author", null), ("authors", null), ("authorx", "auth"), ("auth", null), ("oauth x", "auth"),
            ("authors_x", "auth"), ("api_key=1", "api_key"), ("api  key", null), ("apikey!", "apikey"),
            ("my_jwt_", "_jwt_"), ("xjwt", null), ("jwt_x", "jwt_"), ("pylf_v1_x", "pylf_v1_"), ("pylf_v_", null),
            ("ſecret!", "ſecret"), ("apiKey!", "apiKey"), ("İ éssn", "ssn"),
            ("kéjwté", "jwt"), ("x \U0001F600secret", "secret"), ("session cookie", "session"),
        ];
        foreach (var (text, match) in cases)
        {
            Assert.Equal(match is null ? null : Scrubber.Scrubbed(match), s.Text(text));
        }

        Assert.Equal(Scrubber.KeyScrub.Values, s.Key("Exception.Message", out _));
        Assert.Equal(Scrubber.KeyScrub.Keep, s.Key("exception.message", out _));
        Assert.Equal(Scrubber.KeyScrub.Keep, Scrubber.Of(false, ["*"])!.Key("url.full", out _));
        Assert.Null(Scrubber.Of(false, []));
        // A key a pattern matches: a value of any type becomes the string.
        var controls = new Controls();
        controls.Apply(Vectors.Settings(Policy.OfEvery(("scrubbing", """{"patterns":["n*"]}"""))));
        var span = Assert.Single(ReadSpans(Assert.Single(Send(controls, "/v1/traces", Traces(Span("s", [Int("n", 1), Int("m", 1)]))).Exports).Message));
        Assert.Equal([Str("n", "[Scrubbed due to 'n*']"), Int("m", 1)], span.Attributes);
    }

    private static readonly string[] Pieces =
    [
        "pass", "word", "auth", "or", "s", "api", "key", "_", "-", ".", " ", "jwt", "ssn", "pylf_v", "1", "x", "K",
        "K", "ſ", "İ", "é", "session", "safe", "\U0001F600",
    ];

    private static string Text(Random r)
    {
        var b = new System.Text.StringBuilder();
        for (var i = r.Next(6); i > 0; i--)
        {
            b.Append(Pieces[r.Next(Pieces.Length)]);
        }

        return b.ToString();
    }

    /// <summary>The layer's test-only Evaluate call, which the wrapper doesn't expose: through its one entry point.</summary>
    private static Evaluated Evaluate(Evaluate e)
    {
        var call = typeof(SdkSettings).GetMethod("Do", BindingFlags.NonPublic | BindingFlags.Static)!;
        try
        {
            return ((Reply)call.Invoke(null, [new Request { Evaluate = e }])!).Evaluated;
        }
        catch (TargetInvocationException x)
        {
            throw x.InnerException!;
        }
    }

    // The gates' scrubbing against the layer's (Evaluate), on generated keys, values and bodies.
    [Fact]
    public void ScrubbingLeavesWhatTheLayerLeaves()
    {
        var r = new Random(13);
        for (var run = 0; run < 400; run++)
        {
            var patterns = new List<string>();
            for (var i = r.Next(3); i > 0; i--)
            {
                var p = Text(r).Trim(' ') + "*";
                if (p != "*" && !p.Contains(','))
                {
                    patterns.Add(JsonSerializer.Serialize(p));
                }
            }

            var fields = new List<string>();
            if (r.Next(4) != 0)
            {
                fields.Add("\"defaults\":true");
            }

            if (patterns.Count > 0)
            {
                fields.Add($"\"patterns\":[{string.Join(",", patterns)}]");
            }

            var doc = fields.Count == 0 ? Policy.OfEvery() : Policy.OfEvery(("scrubbing", "{" + string.Join(",", fields) + "}"));
            var attributes = new List<KeyValue>();
            var keys = new HashSet<string>();
            for (var i = r.Next(5); i > 0; i--)
            {
                var key = Text(r);
                if (key.Length > 0 && keys.Add(key))
                {
                    attributes.Add(Str(key, Text(r)));
                }
            }

            var body = new AnyValue { StringValue = Text(r) };
            var input = new Pydantic.OpAmp.Client.Sdk.Attributes { Body = body };
            input.Attributes_.AddRange(attributes);
            var want = Evaluate(new Evaluate
            {
                Settings = ByteString.CopyFromUtf8(doc), Host = Vectors.Declaring(), NowUnixMs = Vectors.Now, Attributes = input,
            });
            Assert.Equal(string.Empty, want.Refused);

            var controls = new Controls();
            controls.Apply(Vectors.Settings(doc));
            var logs = Assert.Single(Send(controls, "/v1/logs", Logs(ScopeLogs("app", [Record(body, [.. attributes])]))).Exports).Message;
            Assert.Equal(want.Redacted.ToList(), Assert.Single(ReadLogs(logs)).Attributes);
            Assert.Equal(want.Body, Assert.Single(Bodies(logs)));
        }
    }

    [Fact]
    public void ThePluginDeclaresItWhereItDeclaresRedaction()
    {
        foreach (var env in new[] { Array.Empty<(string, string)>(), [("OTEL_LOGS_EXPORTER", "console")], [("OTEL_TRACES_EXPORTER", "zipkin")] })
        {
            var live = Agent.Prepare(n => n == "OTEL_OPAMP_ENDPOINT" ? "http://127.0.0.1:1/v1/opamp" : env.FirstOrDefault(e => e.Item1 == n).Item2, (_, _) => { })!
                .HostNow(providersBuilt: false).Live;
            Assert.Equal(live.Contains("drop_attributes"), live.Contains("scrub_defaults"));
            Assert.Equal(live.Contains("drop_attributes"), live.Contains("scrub_patterns"));
        }
    }
}
