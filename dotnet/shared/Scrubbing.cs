// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// scrub_defaults and scrub_patterns: the Logfire SDK's scrubbing, of the attributes redaction
/// keeps and of log bodies (docs/sdk-settings.md). The matchers are the layer's, character for
/// character: the shared vectors (scrub.json) and its reference (Evaluate) hold them to it.
/// <paramref name="defaults"/>: the Logfire SDK's default patterns; <paramref name="patterns"/>:
/// globs (lowercase, sorted, as the layer hands them) matched ignoring ASCII case against a whole
/// key or string value.
/// </summary>
internal sealed class Scrubber(bool defaults, IReadOnlyList<string> patterns)
{
    /// <summary>
    /// The keys whose values scrubbing never touches, at any depth: the Logfire SDK's SAFE_KEYS
    /// (logfire 5.2.0). Each is matched exactly, case and all.
    /// </summary>
    public static readonly HashSet<string> SafeKeys = new(StringComparer.Ordinal)
    {
        "agent_session_id", "binary_content", "code.filepath", "code.function", "code.lineno", "db.plan",
        "db.query.text", "db.statement", "do_not_scrub", "error.type", "event.name", "exception.message",
        "exception.stacktrace", "exception.type", "fastapi.route.name", "fastapi.route.operation_id",
        "gen_ai.conversation.id", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.provider.name",
        "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.system_instructions",
        "gen_ai.tool.call.id", "gen_ai.tool.definitions", "gen_ai.tool.name",
        "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_read.input_tokens",
        "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.raw", "http.method",
        "http.route", "http.scheme", "http.status_code", "http.target", "http.url",
        "langsmith.metadata.session_id", "langsmith.trace.session_name", "logfire.config",
        "logfire.json_schema", "logfire.level_name", "logfire.level_num", "logfire.logger_name", "logfire.msg",
        "logfire.msg_template", "logfire.package_versions", "logfire.pending_parent_id", "logfire.sample_rate",
        "logfire.scrubbed", "logfire.span_type", "logfire.tags", "logfire.version", "model_request_parameters",
        "pydantic_ai.all_messages", "rpc.method", "url.full", "url.path", "url.query",
    };

    /// <summary>
    /// The kinds of the Logfire SDK's default patterns. Word: the word, as it is. Auth: <c>auth</c>,
    /// unless <c>or</c> or <c>ors</c> and a word boundary follow (author, authors). Pair: two words
    /// with at most one of <c>.</c>, <c>_</c>, space, <c>-</c> between (api key, api_key). Token:
    /// <c>pylf_v</c>, digits, <c>_</c> (a Logfire token's prefix). Acronym: the word on its own, a
    /// word boundary or a <c>_</c> (part of the match) on each side.
    /// </summary>
    private enum Kind
    {
        Word,
        Auth,
        Pair,
        Token,
        Acronym,
    }

    private readonly record struct Default(Kind Kind, string A = "", string B = "");

    /// <summary>
    /// The Logfire SDK's default patterns (logfire 5.2.0's DEFAULT_PATTERNS), in its order: at a
    /// position, the first that matches there is the match.
    /// </summary>
    private static readonly Default[] Defaults =
    [
        new(Kind.Word, "password"), new(Kind.Word, "passwd"), new(Kind.Word, "mysql_pwd"), new(Kind.Word, "secret"),
        new(Kind.Auth), new(Kind.Word, "credential"), new(Kind.Pair, "private", "key"), new(Kind.Pair, "api", "key"),
        new(Kind.Word, "session"), new(Kind.Word, "cookie"), new(Kind.Pair, "social", "security"),
        new(Kind.Pair, "credit", "card"), new(Kind.Pair, "logfire", "token"), new(Kind.Token),
        new(Kind.Acronym, "csrf"), new(Kind.Acronym, "xsrf"), new(Kind.Acronym, "jwt"), new(Kind.Acronym, "ssn"),
    ];

    /// <summary>What becomes of an attribute by its key.</summary>
    public enum KeyScrub
    {
        /// <summary>Its value as it is, nested values too.</summary>
        Keep,

        /// <summary>Its value replaced, whatever its type, by the string <see cref="Key"/> gives.</summary>
        Replace,

        /// <summary>Its string values scrubbed: a string, a list's items, a map's members as attributes.</summary>
        Values,
    }

    /// <summary>The settings' scrubbing; null when it changes nothing.</summary>
    public static Scrubber? Of(bool defaults, IReadOnlyList<string> patterns) =>
        !defaults && patterns.Count == 0 ? null : new Scrubber(defaults, patterns);

    /// <summary>What a scrubbed value becomes, naming why: the text a default pattern matched, or the pattern.</summary>
    public static string Scrubbed(string why) => $"[Scrubbed due to '{why}']";

    /// <summary>
    /// What becomes of the attribute (or a map's member) <paramref name="key"/>: kept (one of
    /// SafeKeys), its value replaced with <paramref name="replacement"/> (a default pattern matches
    /// anywhere in the key, or a pattern matches it), or its values scrubbed.
    /// </summary>
    public KeyScrub Key(string key, out string replacement)
    {
        replacement = string.Empty;
        if (SafeKeys.Contains(key))
        {
            return KeyScrub.Keep;
        }

        if (defaults)
        {
            var t = CodePoints(key);
            if (FindDefault(t) is var (start, end))
            {
                replacement = Scrubbed(Text(t, start, end));
                return KeyScrub.Replace;
            }
        }

        if (this.Pattern(key) is { } p)
        {
            replacement = Scrubbed(p);
            return KeyScrub.Replace;
        }

        return KeyScrub.Values;
    }

    /// <summary>
    /// What a string value or a log body becomes; null: kept. A default pattern scrubs it when it
    /// matches part of it (a value that is all the match, <c>password</c> alone, says nothing secret
    /// and is kept, as the Logfire SDK keeps it); a pattern when it matches it whole.
    /// </summary>
    public string? Text(string text)
    {
        if (defaults)
        {
            var t = CodePoints(text);
            if (FindDefault(t) is var (start, end) && (start != 0 || end != t.Length))
            {
                return Scrubbed(Text(t, start, end));
            }
        }

        return this.Pattern(text) is { } p ? Scrubbed(p) : null;
    }

    /// <summary>The first pattern that matches <paramref name="text"/> whole, ignoring ASCII case; null: none.</summary>
    private string? Pattern(string text)
    {
        foreach (var p in patterns)
        {
            if (Glob.MatchesIgnoringAsciiCase(p, text))
            {
                return p;
            }
        }

        return null;
    }

    private static int[] CodePoints(string s)
    {
        var list = new List<int>(s.Length);
        foreach (var r in s.EnumerateRunes())
        {
            list.Add(r.Value);
        }

        return [.. list];
    }

    private static string Text(int[] t, int start, int end)
    {
        var b = new StringBuilder();
        for (var i = start; i < end; i++)
        {
            b.Append(char.ConvertFromUtf32(t[i]));
        }

        return b.ToString();
    }

    /// <summary>
    /// Whether the text's character <paramref name="c"/> is the pattern's <paramref name="p"/>, as
    /// Python's re.IGNORECASE has it: a lowercase ASCII letter also matches its uppercase, and
    /// <c>i</c> also <c>İ</c> and <c>ı</c>, <c>s</c> also <c>ſ</c>, <c>k</c> also the Kelvin sign;
    /// any other character only itself.
    /// </summary>
    private static bool SameLetter(int p, int c)
    {
        if (p is < 'a' or > 'z')
        {
            return p == c;
        }

        if (c == p || c == p - ('a' - 'A'))
        {
            return true;
        }

        return p switch
        {
            'i' => c is 0x130 or 0x131,
            's' => c == 0x17F,
            'k' => c == 0x212A,
            _ => false,
        };
    }

    /// <summary>t[i], or -1 outside t.</summary>
    private static int At(int[] t, int i) => i < 0 || i >= t.Length ? -1 : t[i];

    /// <summary>Whether t[i] is a word character (an ASCII letter, digit or <c>_</c>); outside t, none.</summary>
    private static bool IsWord(int[] t, int i) => At(t, i) is (>= 'a' and <= 'z') or (>= 'A' and <= 'Z') or (>= '0' and <= '9') or '_';

    /// <summary>Whether there is a word boundary before t[i].</summary>
    private static bool Boundary(int[] t, int i) => IsWord(t, i - 1) != IsWord(t, i);

    /// <summary>Where <paramref name="w"/> ends if it is at t[i..], else -1.</summary>
    private static int WordAt(int[] t, int i, string w)
    {
        foreach (var p in w)
        {
            if (i >= t.Length || !SameLetter(p, t[i]))
            {
                return -1;
            }

            i++;
        }

        return i;
    }

    /// <summary>
    /// Where <paramref name="d"/> ends if it matches at t[i..], else -1. Where a pattern could
    /// match two ways, it takes the way Python's regex tries first (a separator, then none).
    /// </summary>
    private static int DefaultAt(int[] t, int i, Default d)
    {
        switch (d.Kind)
        {
            case Kind.Word:
                return WordAt(t, i, d.A);
            case Kind.Auth:
            {
                var end = WordAt(t, i, "auth");
                if (end < 0)
                {
                    return -1;
                }

                var or = WordAt(t, end, "or");
                return or >= 0 && ((SameLetter('s', At(t, or)) && Boundary(t, or + 1)) || Boundary(t, or)) ? -1 : end;
            }

            case Kind.Pair:
            {
                var mid = WordAt(t, i, d.A);
                if (mid < 0)
                {
                    return -1;
                }

                if (At(t, mid) is '.' or '_' or ' ' or '-' && WordAt(t, mid + 1, d.B) is var end and >= 0)
                {
                    return end;
                }

                return WordAt(t, mid, d.B);
            }

            case Kind.Token:
            {
                var j = WordAt(t, i, "pylf_v");
                if (j < 0)
                {
                    return -1;
                }

                var digits = j;
                while (At(t, j) is >= '0' and <= '9')
                {
                    j++;
                }

                return j > digits && At(t, j) == '_' ? j + 1 : -1;
            }

            default: // Acronym
                if (Boundary(t, i) && After(t, WordAt(t, i, d.A)) is var e and >= 0)
                {
                    return e;
                }

                return At(t, i) == '_' ? After(t, WordAt(t, i + 1, d.A)) : -1;
        }
    }

    /// <summary>An acronym's end: at a word boundary, or past a <c>_</c>; -1: neither (or no acronym).</summary>
    private static int After(int[] t, int end) =>
        end < 0 ? -1 : Boundary(t, end) ? end : At(t, end) == '_' ? end + 1 : -1;

    /// <summary>
    /// The first match of a default pattern in t, as Python's re.search finds it: the leftmost
    /// start, and there the first pattern in their order. Null: none.
    /// </summary>
    private static (int Start, int End)? FindDefault(int[] t)
    {
        for (var i = 0; i < t.Length; i++)
        {
            foreach (var d in Defaults)
            {
                var end = DefaultAt(t, i, d);
                if (end >= 0)
                {
                    return (i, end);
                }
            }
        }

        return null;
    }
}
