// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.KeyValue;
import io.opentelemetry.api.common.Value;
import io.opentelemetry.api.common.ValueType;
import java.util.ArrayList;
import java.util.List;
import java.util.Set;

/**
 * {@code scrub_defaults} and {@code scrub_patterns}: the Logfire SDK's scrubbing, of the
 * attributes redaction keeps and of log bodies (docs/sdk-settings.md). The matchers are the
 * layer's, character for character: the shared vectors (scrub.json) and its reference
 * (Evaluate) hold them to it.
 */
final class Scrubbing {
  static final Scrubbing NONE = new Scrubbing(false, List.of());

  /**
   * The keys whose values scrubbing never touches, at any depth: the Logfire SDK's SAFE_KEYS
   * (logfire 5.2.0). Each is matched exactly, case and all.
   */
  static final Set<String> SAFE_KEYS = Set.of(
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
      "pydantic_ai.all_messages", "rpc.method", "url.full", "url.path", "url.query");

  /**
   * The kinds of the Logfire SDK's default patterns. WORD: the word, as it is. AUTH: {@code auth},
   * unless {@code or} or {@code ors} and a word boundary follow (author, authors). PAIR: two words
   * with at most one of {@code .}, {@code _}, space, {@code -} between (api key, api_key). TOKEN:
   * {@code pylf_v}, digits, {@code _} (a Logfire token's prefix). ACRONYM: the word on its own, a
   * word boundary or a {@code _} (part of the match) on each side.
   */
  private enum Kind { WORD, AUTH, PAIR, TOKEN, ACRONYM }

  private record Pattern(Kind kind, String a, String b) {}

  /**
   * The Logfire SDK's default patterns (logfire 5.2.0's DEFAULT_PATTERNS), in its order: at a
   * position, the first that matches there is the match.
   */
  private static final List<Pattern> DEFAULTS = List.of(
      word("password"), word("passwd"), word("mysql_pwd"), word("secret"), new Pattern(Kind.AUTH, "", ""),
      word("credential"), pair("private", "key"), pair("api", "key"), word("session"), word("cookie"),
      pair("social", "security"), pair("credit", "card"), pair("logfire", "token"),
      new Pattern(Kind.TOKEN, "", ""), acronym("csrf"), acronym("xsrf"), acronym("jwt"), acronym("ssn"));

  private static Pattern word(String w) {
    return new Pattern(Kind.WORD, w, "");
  }

  private static Pattern pair(String a, String b) {
    return new Pattern(Kind.PAIR, a, b);
  }

  private static Pattern acronym(String w) {
    return new Pattern(Kind.ACRONYM, w, "");
  }

  private final boolean defaults;
  private final List<String> patterns;
  private final List<Glob> globs;

  private Scrubbing(boolean defaults, List<String> patterns) {
    this.defaults = defaults;
    this.patterns = patterns;
    this.globs = patterns.stream().map(Glob::ignoringAsciiCase).toList();
  }

  /** scrub_defaults and scrub_patterns (globs, lowercase, sorted, as the layer hands them). */
  static Scrubbing of(boolean defaults, List<String> patterns) {
    return !defaults && patterns.isEmpty() ? NONE : new Scrubbing(defaults, List.copyOf(patterns));
  }

  /** The settings' scrubbing; {@link #NONE} without any. */
  static Scrubbing of(SettingsProto.Settings s) {
    return of(s.getScrubDefaults(), s.getScrubPatternsList());
  }

  /** Whether scrubbing changes anything at all. */
  boolean any() {
    return defaults || !patterns.isEmpty();
  }

  /** What a scrubbed value becomes, naming why: the text a default pattern matched, or the pattern. */
  static String scrubbed(String why) {
    return "[Scrubbed due to '" + why + "']";
  }

  /** {@link #key}'s answer that leaves the value, nested values too, as it is. */
  static final String KEEP = null;
  /** {@link #key}'s answer that scrubs the value's strings: {@link #value} with no key. */
  static final String VALUES = "";

  /**
   * What becomes of the attribute (or a map's member) {@code key}: {@link #KEEP} (scrubbing off,
   * or one of SAFE_KEYS), the string that replaces its value whatever its type (a default pattern
   * matches anywhere in the key, or a pattern matches it), or {@link #VALUES}.
   */
  String key(String key) {
    if (!any() || SAFE_KEYS.contains(key)) {
      return KEEP;
    }
    if (defaults) {
      int[] t = key.codePoints().toArray();
      int[] m = findDefault(t);
      if (m != null) {
        return scrubbed(new String(t, m[0], m[1] - m[0]));
      }
    }
    String p = pattern(key);
    return p == null ? VALUES : scrubbed(p);
  }

  /**
   * What a string value or a log body becomes; null: kept. A default pattern scrubs it when it
   * matches part of it (a value that is all the match, {@code password} alone, says nothing secret
   * and is kept, as the Logfire SDK keeps it); a pattern when it matches it whole.
   */
  String text(String text) {
    if (!any()) {
      return null;
    }
    if (defaults) {
      int[] t = text.codePoints().toArray();
      int[] m = findDefault(t);
      if (m != null && (m[0] != 0 || m[1] != t.length)) {
        return scrubbed(new String(t, m[0], m[1] - m[0]));
      }
    }
    String p = pattern(text);
    return p == null ? null : scrubbed(p);
  }

  /** The first pattern that matches {@code text} whole, ignoring ASCII case; null: none. */
  private String pattern(String text) {
    for (int i = 0; i < globs.size(); i++) {
      if (globs.get(i).matches(text)) {
        return patterns.get(i);
      }
    }
    return null;
  }

  /**
   * A body or a map member's or list item's value as scrubbing leaves it: the same object when
   * nothing changes. {@code key}: the member's key (null: a body or an item). A string is
   * scrubbed, a list's items each, a map's members each as an attribute; any other value kept.
   */
  Value<?> value(String key, Value<?> value) {
    if (key != null) {
      String what = key(key);
      if (what == KEEP) {
        return value;
      }
      if (!what.isEmpty()) {
        return Value.of(what);
      }
    }
    if (value == null) {
      return null;
    }
    ValueType type = value.getType();
    if (type == ValueType.STRING) {
      String s = text((String) value.getValue());
      return s == null ? value : Value.of(s);
    }
    if (type == ValueType.ARRAY) {
      @SuppressWarnings("unchecked")
      List<Value<?>> items = (List<Value<?>>) value.getValue();
      List<Value<?>> out = null;
      for (int i = 0; i < items.size(); i++) {
        Value<?> s = value(null, items.get(i));
        if (s == items.get(i)) {
          continue;
        }
        if (out == null) {
          out = new ArrayList<>(items);
        }
        out.set(i, s);
      }
      return out == null ? value : Value.of(out);
    }
    if (type == ValueType.KEY_VALUE_LIST) {
      @SuppressWarnings("unchecked")
      List<KeyValue> members = (List<KeyValue>) value.getValue();
      KeyValue[] out = null;
      for (int i = 0; i < members.size(); i++) {
        KeyValue m = members.get(i);
        Value<?> s = value(m.getKey(), m.getValue());
        if (s == m.getValue()) {
          continue;
        }
        if (out == null) {
          out = members.toArray(KeyValue[]::new);
        }
        out[i] = KeyValue.of(m.getKey(), s);
      }
      return out == null ? value : Value.of(out);
    }
    return value;
  }

  // --- the default patterns, as Python's re finds them ---

  /**
   * Whether the text's character c is the pattern's character p, as Python's re.IGNORECASE has
   * it: a lowercase ASCII letter also matches its uppercase, and {@code i} also {@code İ} and
   * {@code ı}, {@code s} also {@code ſ}, {@code k} also the Kelvin sign; any other character only
   * itself.
   */
  private static boolean sameLetter(int p, int c) {
    if (p < 'a' || p > 'z') {
      return p == c;
    }
    if (c == p || c == p - ('a' - 'A')) {
      return true;
    }
    return switch (p) {
      case 'i' -> c == 0x130 || c == 0x131;
      case 's' -> c == 0x17F;
      case 'k' -> c == 0x212A;
      default -> false;
    };
  }

  /** t[i], or -1 outside t. */
  private static int at(int[] t, int i) {
    return i < 0 || i >= t.length ? -1 : t[i];
  }

  /** Whether t[i] is a word character (an ASCII letter, digit or {@code _}); outside t, none. */
  private static boolean isWord(int[] t, int i) {
    int c = at(t, i);
    return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_';
  }

  /** Whether there is a word boundary before t[i]. */
  private static boolean boundary(int[] t, int i) {
    return isWord(t, i - 1) != isWord(t, i);
  }

  /** Where {@code w} ends if it is at t[i:], else -1. */
  private static int wordAt(int[] t, int i, String w) {
    for (int k = 0; k < w.length(); k++, i++) {
      if (i >= t.length || !sameLetter(w.charAt(k), t[i])) {
        return -1;
      }
    }
    return i;
  }

  /**
   * Where {@code d} ends if it matches at t[i:], else -1. Where a pattern could match two ways,
   * it takes the way Python's regex tries first (a separator, then none).
   */
  private static int defaultAt(int[] t, int i, Pattern d) {
    switch (d.kind()) {
      case WORD:
        return wordAt(t, i, d.a());
      case AUTH: {
        int end = wordAt(t, i, "auth");
        if (end < 0) {
          return -1;
        }
        int or = wordAt(t, end, "or");
        if (or >= 0 && (sameLetter('s', at(t, or)) && boundary(t, or + 1) || boundary(t, or))) {
          return -1;
        }
        return end;
      }
      case PAIR: {
        int mid = wordAt(t, i, d.a());
        if (mid < 0) {
          return -1;
        }
        int c = at(t, mid);
        if (c == '.' || c == '_' || c == ' ' || c == '-') {
          int end = wordAt(t, mid + 1, d.b());
          if (end >= 0) {
            return end;
          }
        }
        return wordAt(t, mid, d.b());
      }
      case TOKEN: {
        int j = wordAt(t, i, "pylf_v");
        if (j < 0) {
          return -1;
        }
        int digits = j;
        while (at(t, j) >= '0' && at(t, j) <= '9') {
          j++;
        }
        return j > digits && at(t, j) == '_' ? j + 1 : -1;
      }
      default: { // ACRONYM
        if (boundary(t, i)) {
          int end = after(t, wordAt(t, i, d.a()));
          if (end >= 0) {
            return end;
          }
        }
        return at(t, i) == '_' ? after(t, wordAt(t, i + 1, d.a())) : -1;
      }
    }
  }

  /** An acronym's end: at a word boundary, or past a {@code _}; -1: neither (or no acronym). */
  private static int after(int[] t, int end) {
    if (end < 0) {
      return -1;
    }
    if (boundary(t, end)) {
      return end;
    }
    return at(t, end) == '_' ? end + 1 : -1;
  }

  /**
   * The first match of a default pattern in t, as Python's re.search finds it: the leftmost
   * start, and there the first pattern in their order. {start, end}; null: none.
   */
  private static int[] findDefault(int[] t) {
    for (int i = 0; i < t.length; i++) {
      for (Pattern d : DEFAULTS) {
        int end = defaultAt(t, i, d);
        if (end >= 0) {
          return new int[] {i, end};
        }
      }
    }
    return null;
  }
}
