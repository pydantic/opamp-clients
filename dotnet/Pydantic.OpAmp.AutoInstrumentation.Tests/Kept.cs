// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Text;
using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Tests;

/// <summary>
/// The client's saved state (opamp-state) holding a config applied, as the client leaves it once
/// the server has heard APPLIED: what the next start's settings come from. A test without a client
/// writes it here.
/// </summary>
/// <remarks>
/// The bytes are a state the client saved before it kept the config applied (the shared vectors'
/// testdata/sdk-settings/start.json), with the config applied added: its field 16, a message, which
/// protobuf takes from the end of the encoding.
/// </remarks>
internal static class Kept
{
    private const string Before =
        "a core state saved before the core kept the config applied: the document kept before carries over";

    /// <summary>A client state whose config applied has <paramref name="document"/> as its settings entry.</summary>
    public static byte[] CoreState(string document)
    {
        var dir = AppContext.BaseDirectory;
        while (!Directory.Exists(Path.Combine(dir, "testdata", "sdk-settings")))
        {
            dir = Path.GetDirectoryName(dir) ?? throw new DirectoryNotFoundException("testdata/sdk-settings");
        }

        using var doc = JsonDocument.Parse(File.ReadAllText(Path.Combine(dir, "testdata", "sdk-settings", "start.json")));
        var hex = doc.RootElement.GetProperty("cases").EnumerateArray()
            .First(c => c.GetProperty("name").GetString() == Before).GetProperty("core_state").GetString()!;
        var applied = new AgentRemoteConfig
        {
            ConfigHash = ByteString.CopyFromUtf8("applied"),
            Config = new AgentConfigMap
            {
                ConfigMap =
                {
                    ["elastic"] = new AgentConfigObject
                    {
                        Body = ByteString.CopyFrom(Encoding.UTF8.GetBytes(document)),
                        ContentType = "application/json",
                    },
                },
            },
        };
        using var out_ = new MemoryStream();
        out_.Write(Convert.FromHexString(hex));
        using (var coded = new CodedOutputStream(out_, leaveOpen: true))
        {
            coded.WriteTag(16, WireFormat.WireType.LengthDelimited);
            coded.WriteBytes(applied.ToByteString());
        }

        return out_.ToArray();
    }

    /// <summary>Leave in <paramref name="dir"/> what the client would after a config with <paramref name="document"/> was applied.</summary>
    public static void KeepApplied(string dir, string document) =>
        File.WriteAllBytes(Path.Combine(dir, "opamp-state"), CoreState(document));
}
