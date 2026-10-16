// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Tests;

/// <summary>
/// Logfire SDK policies for the tests: the remote config's only file, <c>logfire-sdk-policy.json</c>
/// (docs/sdk-settings.md). A test writes only the overrides. Every other control is local.
/// </summary>
internal static class Policy
{
    public const string File = "logfire-sdk-policy.json";
    public const string V1 = "logfire-sdk-policy/v1";
    public const string V2 = "logfire-sdk-policy/v2-draft";

    private static readonly string[] Signals = ["traces", "metrics", "logs"];

    private static readonly string[] V1Controls = ["head_sampling", "signal_exports"];

    private static readonly string[] V2Controls =
    [
        "head_sampling", "signal_exports", "sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit",
        "attribute_redaction", "debug", "app_settings", "instrumentations", "polling_interval",
    ];

    /// <summary>The version 1 policy with every control local: the SDK's own settings.</summary>
    public static string Local => Of();

    /// <summary>
    /// A policy with these overrides and every other control local. Each override names a control
    /// (a signal's export as <c>signal_exports.logs</c>) and the fields of its override, as a JSON
    /// object. The policy is version 1 when every override is a version 1 control, else version 2.
    /// </summary>
    public static string Of(params (string Control, string Fields)[] overrides) =>
        Write(overrides.All(o => o.Control == "head_sampling" || o.Control.StartsWith("signal_exports.", StringComparison.Ordinal)) ? 1 : 2, overrides);

    /// <summary>As <see cref="Of"/>, but always version 2.</summary>
    public static string OfV2(params (string Control, string Fields)[] overrides) => Write(2, overrides);

    /// <summary>A version 1 policy that samples <paramref name="ratio"/> of new traces.</summary>
    public static string HeadSampling(double ratio) => Of(("head_sampling", $"{{\"ratio\":{Number(ratio)}}}"));

    /// <summary>A version 1 policy that stops the export of each signal named.</summary>
    public static string Stop(params string[] signals) =>
        Of([.. signals.Select(s => ($"signal_exports.{s}", "{\"enabled\":false}"))]);

    /// <summary>A number as JSON writes it.</summary>
    public static string Number(double n) => n.ToString("R", CultureInfo.InvariantCulture);

    /// <summary>A remote config whose only file is <paramref name="document"/>, hashed by it unless <paramref name="hash"/> is given.</summary>
    public static AgentRemoteConfig Config(string document, string? hash = null) => new()
    {
        ConfigHash = ByteString.CopyFromUtf8(hash ?? document),
        Config = new AgentConfigMap
        {
            ConfigMap = { [File] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(document), ContentType = "application/json" } },
        },
    };

    /// <summary>The policy an effective config reports. It is the only file, with the content type application/json.</summary>
    public static string Text(EffectiveConfig e)
    {
        var file = Assert.Single(e.ConfigMap.ConfigMap);
        Assert.Equal(File, file.Key);
        Assert.Equal("application/json", file.Value.ContentType);
        return file.Value.Body.ToStringUtf8();
    }

    /// <summary>
    /// The overrides of the policy an effective config reports, as <see cref="Overrides(string)"/> writes them.
    /// </summary>
    public static string Overrides(EffectiveConfig e) => Overrides(Text(e));

    /// <summary>
    /// The overrides of <paramref name="policy"/>, as a compact JSON object: each overridden control
    /// (a signal's export as <c>signal_exports.logs</c>) to the fields of its override, without
    /// <c>mode</c>, keys sorted. A policy with every control local is <c>{}</c>.
    /// </summary>
    public static string Overrides(string policy)
    {
        var root = JsonNode.Parse(policy)!.AsObject();
        var found = new SortedDictionary<string, JsonNode>(StringComparer.Ordinal);
        foreach (var (name, value) in root)
        {
            if (name == "schema")
            {
                continue;
            }

            if (name == "signal_exports")
            {
                foreach (var (signal, control) in value!.AsObject())
                {
                    Add($"signal_exports.{signal}", control!);
                }
            }
            else
            {
                Add(name, value!);
            }
        }

        var o = new JsonObject();
        foreach (var (k, v) in found)
        {
            o[k] = v;
        }

        return o.ToJsonString();

        void Add(string control, JsonNode node)
        {
            var fields = Sorted(node)!.AsObject();
            if (fields["mode"]?.GetValue<string>() != "override")
            {
                return;
            }

            fields.Remove("mode");
            found[control] = fields;
        }
    }

    /// <summary>The schema of <paramref name="policy"/>.</summary>
    public static string Schema(string policy) => JsonNode.Parse(policy)!["schema"]!.GetValue<string>();

    private static JsonNode? Sorted(JsonNode? node) => node switch
    {
        JsonObject o => new JsonObject(o.OrderBy(kv => kv.Key, StringComparer.Ordinal)
            .Select(kv => KeyValuePair.Create(kv.Key, Sorted(kv.Value)))),
        JsonArray a => new JsonArray([.. a.Select(Sorted)]),
        null => null,
        _ => JsonNode.Parse(node.ToJsonString()),
    };

    private static string Write(int version, (string Control, string Fields)[] overrides)
    {
        var given = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var (control, fields) in overrides)
        {
            var known = control.StartsWith("signal_exports.", StringComparison.Ordinal)
                ? Signals.Contains(control["signal_exports.".Length..])
                : V2Controls.Contains(control) && control != "signal_exports";
            Assert.True(known, $"{control} is no control of the policy");
            Assert.True(version == 2 || control == "head_sampling" || control.StartsWith("signal_exports.", StringComparison.Ordinal), $"{control} is no version 1 control");
            using var parsed = JsonDocument.Parse(fields); // the fields are one JSON object
            Assert.Equal(JsonValueKind.Object, parsed.RootElement.ValueKind);
            var inner = fields.Trim()[1..^1].Trim();
            given.Add(control, inner.Length == 0 ? "{\"mode\":\"override\"}" : $"{{\"mode\":\"override\",{inner}}}");
        }

        const string local = "{\"mode\":\"local\"}";
        string Control(string name) => given.GetValueOrDefault(name, local);
        var b = new StringBuilder("{\"schema\":\"").Append(version == 1 ? V1 : V2).Append('"');
        foreach (var name in version == 1 ? V1Controls : V2Controls)
        {
            b.Append(",\"").Append(name).Append("\":");
            if (name == "signal_exports")
            {
                b.Append('{').AppendJoin(',', Signals.Select(s => $"\"{s}\":{Control($"signal_exports.{s}")}")).Append('}');
            }
            else
            {
                b.Append(Control(name));
            }
        }

        return b.Append('}').ToString();
    }
}
