// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package match

import (
	"slices"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// SafeKeys are the keys whose values scrubbing never touches, at any depth: the Logfire SDK's
// SAFE_KEYS (logfire 5.2.0), sorted. Each is matched exactly, case and all.
var SafeKeys = []string{
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
}

// The kinds of the Logfire SDK's default patterns.
const (
	word    = iota // the word, as it is
	auth           // `auth`, unless `or` or `ors` and a word boundary follow (author, authors)
	pair           // two words, with at most one of `.`, `_`, ` `, `-` between (api key, api_key)
	token          // `pylf_v`, digits, `_`: a Logfire token's prefix
	acronym        // the word on its own: a word boundary or a `_` (part of the match) on each side
)

type pattern struct {
	kind int
	a, b string
}

// defaults are the Logfire SDK's default patterns (logfire 5.2.0's DEFAULT_PATTERNS), in its
// order: at a position, the first that matches there is the match.
var defaults = []pattern{
	{word, "password", ""}, {word, "passwd", ""}, {word, "mysql_pwd", ""}, {word, "secret", ""},
	{auth, "", ""}, {word, "credential", ""}, {pair, "private", "key"}, {pair, "api", "key"},
	{word, "session", ""}, {word, "cookie", ""}, {pair, "social", "security"}, {pair, "credit", "card"},
	{pair, "logfire", "token"}, {token, "", ""}, {acronym, "csrf", ""}, {acronym, "xsrf", ""},
	{acronym, "jwt", ""}, {acronym, "ssn", ""},
}

// sameLetter says whether the text's character c is the pattern's character p, as Python's
// re.IGNORECASE has it: a lowercase ASCII letter also matches its uppercase, and `i` also `İ` and
// `ı`, `s` also `ſ`, `k` also the Kelvin sign; any other character only itself.
func sameLetter(p, c rune) bool {
	if p < 'a' || p > 'z' {
		return p == c
	}
	if c == p || c == p-('a'-'A') {
		return true
	}
	switch p {
	case 'i':
		return c == 'İ' || c == 'ı'
	case 's':
		return c == 'ſ'
	case 'k':
		return c == 'K'
	}
	return false
}

// isWord says whether t[i] is a word character (an ASCII letter, digit or `_`); outside t, none.
func isWord(t []rune, i int) bool {
	if i < 0 || i >= len(t) {
		return false
	}
	c := t[i]
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// boundary says whether there is a word boundary before t[at].
func boundary(t []rune, at int) bool { return isWord(t, at-1) != isWord(t, at) }

// wordAt is where w ends if it is at t[at:], else -1.
func wordAt(t []rune, at int, w string) int {
	i := at
	for _, p := range w {
		if i >= len(t) || !sameLetter(p, t[i]) {
			return -1
		}
		i++
	}
	return i
}

func at(t []rune, i int) rune {
	if i < 0 || i >= len(t) {
		return -1
	}
	return t[i]
}

// defaultAt is where d ends if it matches at t[at:], else -1. Where a pattern could match two
// ways, it takes the way Python's regex tries first (a separator, then none).
func defaultAt(t []rune, i int, d pattern) int {
	switch d.kind {
	case word:
		return wordAt(t, i, d.a)
	case auth:
		end := wordAt(t, i, "auth")
		if end < 0 {
			return -1
		}
		if or := wordAt(t, end, "or"); or >= 0 {
			if sameLetter('s', at(t, or)) && boundary(t, or+1) || boundary(t, or) {
				return -1
			}
		}
		return end
	case pair:
		mid := wordAt(t, i, d.a)
		if mid < 0 {
			return -1
		}
		if c := at(t, mid); c == '.' || c == '_' || c == ' ' || c == '-' {
			if end := wordAt(t, mid+1, d.b); end >= 0 {
				return end
			}
		}
		return wordAt(t, mid, d.b)
	case token:
		j := wordAt(t, i, "pylf_v")
		if j < 0 {
			return -1
		}
		digits := j
		for '0' <= at(t, j) && at(t, j) <= '9' {
			j++
		}
		if j > digits && at(t, j) == '_' {
			return j + 1
		}
		return -1
	default: // acronym
		after := func(end int) int {
			switch {
			case end < 0:
				return -1
			case boundary(t, end):
				return end
			case at(t, end) == '_':
				return end + 1
			}
			return -1
		}
		if boundary(t, i) {
			if end := after(wordAt(t, i, d.a)); end >= 0 {
				return end
			}
		}
		if at(t, i) == '_' {
			return after(wordAt(t, i+1, d.a))
		}
		return -1
	}
}

// findDefault is the first match of a default pattern in t, as Python's re.search finds it: the
// leftmost start, and there the first pattern in their order. Its start and end; ok false: none.
func findDefault(t []rune) (start, end int, ok bool) {
	for i := range t {
		for _, d := range defaults {
			if e := defaultAt(t, i, d); e >= 0 {
				return i, e, true
			}
		}
	}
	return 0, 0, false
}

// Scrubbed is what a scrubbed value becomes, naming why: the text a default pattern matched, or
// the pattern.
func Scrubbed(why string) string { return "[Scrubbed due to '" + why + "']" }

// Scrubbing is scrub_defaults and scrub_patterns: the Logfire SDK's default patterns, and globs
// (lowercase, sorted) matched ignoring ASCII case against a whole key or string value.
type Scrubbing struct {
	Defaults bool
	Patterns []string
}

// ScrubbingOf is the settings' scrubbing.
func ScrubbingOf(s *settingspb.Settings) *Scrubbing {
	return &Scrubbing{Defaults: s.GetScrubDefaults(), Patterns: s.GetScrubPatterns()}
}

// None says whether scrubbing leaves every value as it is.
func (s *Scrubbing) None() bool { return s == nil || !s.Defaults && len(s.Patterns) == 0 }

// KeyScrub is what scrubbing does with an attribute, by its key.
type KeyScrub int

const (
	// Keep leaves its value alone, nested values too.
	Keep KeyScrub = iota
	// Replace replaces its value, whatever its type, with the string Key returns.
	Replace
	// Values scrubs its string values (Text): the value if a string, each string in it if a
	// slice, each member as an attribute if a map.
	Values
)

// Key is what becomes of the attribute (or a map's member) key: kept (scrubbing off, or one of
// SafeKeys), its value replaced with the string returned (a default pattern matches anywhere in
// the key, or a pattern matches it), or its values scrubbed.
func (s *Scrubbing) Key(key string) (KeyScrub, string) {
	if s.None() {
		return Keep, ""
	}
	if _, safe := slices.BinarySearch(SafeKeys, key); safe {
		return Keep, ""
	}
	if s.Defaults {
		t := []rune(key)
		if start, end, ok := findDefault(t); ok {
			return Replace, Scrubbed(string(t[start:end]))
		}
	}
	if p, ok := s.pattern(key); ok {
		return Replace, Scrubbed(p)
	}
	return Values, ""
}

// Text is what a string value or a log body becomes, and whether it is scrubbed. A default pattern
// scrubs it when it matches part of it (a value that is all the match, `password` alone, says
// nothing secret and is kept, as the Logfire SDK keeps it); a pattern when it matches it whole.
func (s *Scrubbing) Text(text string) (string, bool) {
	if s.None() {
		return text, false
	}
	if s.Defaults {
		t := []rune(text)
		if start, end, ok := findDefault(t); ok && (start != 0 || end != len(t)) {
			return Scrubbed(string(t[start:end])), true
		}
	}
	if p, ok := s.pattern(text); ok {
		return Scrubbed(p), true
	}
	return text, false
}

// pattern is the first pattern (sorted, as the layer keeps them) that matches text.
func (s *Scrubbing) pattern(text string) (string, bool) {
	for _, p := range s.Patterns {
		if Glob(p, text, true) {
			return p, true
		}
	}
	return "", false
}
