// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections;
using System.Text;
using System.Text.RegularExpressions;
using Google.Protobuf;
using Google.Protobuf.Reflection;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// What the options records and the notifications print instead of their secrets: a record's
/// ToString is what <c>$"{options}"</c> and structured loggers write, so it must not hold a key, a
/// header's value (Authorization, say) or a URL's password. As Python's <c>Config.__repr__</c>.
/// </summary>
internal static partial class Redacted
{
    private const string Hidden = "<redacted>";

    // What a private key is set to in the copy printed; and how protobuf's JSON prints that, and a
    // header's value set to Hidden (its '<' and '>' escaped), both printed as Hidden instead.
    private static readonly ByteString HiddenKey = ByteString.CopyFromUtf8(Hidden);
    private static readonly string HiddenKeyJson = $"\"{HiddenKey.ToBase64()}\"";
    private static readonly string HiddenJson = "\"\\u003credacted\\u003e\"";

    /// <summary>Print <paramref name="members"/> as a record does: <c>Name = value</c>, comma-separated.</summary>
    public static bool Print(StringBuilder builder, params (string Name, object? Value)[] members)
    {
        for (var i = 0; i < members.Length; i++)
        {
            builder.Append(i == 0 ? string.Empty : ", ").Append(members[i].Name).Append(" = ").Append(members[i].Value);
        }

        return true;
    }

    /// <summary>Whether there is a secret, not what it is.</summary>
    public static string? Secret(string? value) => value is null ? null : Hidden;

    /// <summary><paramref name="uri"/> as written, without the password in it.</summary>
    public static string? Url(Uri? uri) => uri is null ? null : Password().Replace(uri.OriginalString, "$1:***@");

    /// <summary><paramref name="url"/> without the password in it.</summary>
    public static string Url(string url) => Password().Replace(url, "$1:***@");

    /// <summary>
    /// <paramref name="message"/> as protobuf prints it (JSON), but with what the server offers as
    /// credentials hidden: every header's value, every private key, and the password in every URL
    /// (an endpoint, a proxy, a download).
    /// </summary>
    public static string Message(IMessage message)
    {
        var copy = message.Descriptor.Parser.ParseFrom(message.ToByteString());
        Hide(copy);
        return copy.ToString()!
            .Replace(HiddenKeyJson, $"\"{Hidden}\"", StringComparison.Ordinal)
            .Replace(HiddenJson, $"\"{Hidden}\"", StringComparison.Ordinal);
    }

    /// <summary>The headers' names, each value left out: any may hold a credential.</summary>
    public static string? Headers(IReadOnlyDictionary<string, string>? headers) =>
        headers is null ? null : $"{{ {string.Join(", ", headers.Keys.Select(k => $"{k} = {Hidden}"))} }}";

    private static void Hide(IMessage message)
    {
        switch (message)
        {
            case global::OpAmp.Proto.V1.Header h:
                h.Value = Hidden;
                break;
            case TLSCertificate { PrivateKey.IsEmpty: false } c:
                c.PrivateKey = HiddenKey;
                break;
        }

        foreach (var field in message.Descriptor.Fields.InFieldNumberOrder())
        {
            var value = field.Accessor.GetValue(message);
            switch (field.FieldType)
            {
                case FieldType.String when !field.IsRepeated && field.Name is "destination_endpoint" or "url" or "download_url":
                    field.Accessor.SetValue(message, Url((string)value));
                    break;
                case FieldType.Message when field.IsMap:
                    foreach (DictionaryEntry e in (IDictionary)value)
                    {
                        if (e.Value is IMessage m)
                        {
                            Hide(m);
                        }
                    }

                    break;
                case FieldType.Message when field.IsRepeated:
                    foreach (IMessage m in (IList)value)
                    {
                        Hide(m);
                    }

                    break;
                case FieldType.Message when value is IMessage m:
                    Hide(m);
                    break;
            }
        }
    }

    [GeneratedRegex("(//[^/@:]*):[^/@]*@")]
    private static partial Regex Password();
}
