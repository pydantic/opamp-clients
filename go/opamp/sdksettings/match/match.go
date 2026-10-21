// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package match holds the matchers of the settings that match telemetry (docs/sdk-settings.md,
// "Settings that match telemetry"): globs, log levels by scope and attribute redaction by key.
// The settings layer reads and checks the settings; these only match, the layer's way, and run
// once a span, log record or attribute, so they are native. The Go integration (opampotel) and
// the Collector's logfirepolicy processor both match with them, and the shared vectors in
// testdata/sdk-settings hold them to the layer.
package match

import (
	"strings"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// Glob says whether text matches pattern: `*` matches any run of characters, none included, and
// every other character is itself. By Unicode characters (runes), case-sensitively, or ignoring
// ASCII case with fold. On a mismatch the last `*` takes one more character and the pattern after
// it starts again, so at most pattern × text steps.
func Glob(pattern, text string, fold bool) bool {
	p, t := []rune(pattern), []rune(text)
	pi, ti := 0, 0
	starAfter, starTaken := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && p[pi] == '*':
			pi++
			starAfter, starTaken = pi, ti
		case pi < len(p) && same(p[pi], t[ti], fold):
			pi++
			ti++
		case starAfter >= 0:
			starTaken++
			pi, ti = starAfter, starTaken
		default:
			return false
		}
	}
	for ; pi < len(p); pi++ {
		if p[pi] != '*' {
			return false
		}
	}
	return true
}

// same compares two characters, ignoring ASCII case (only) with fold: `A` is `a`, `É` isn't `é`.
func same(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	if !fold {
		return false
	}
	lower := func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	return lower(a) == lower(b)
}

// SeverityOff is a scope level's threshold under `off`: past every SeverityNumber.
const SeverityOff = 0xFFFF

// Levels are send_logs_level and send_logs_level_by_scope, ready to judge a record.
type Levels struct {
	Min    uint32 // send_logs_level's SeverityNumber; 0: every record
	Scopes []*settingspb.ScopeLevel
}

// LevelsOf are the settings' levels.
func LevelsOf(s *settingspb.Settings) *Levels {
	return &Levels{Min: s.GetSendLogsMinSeverity(), Scopes: s.GetSendLogsScopeLevels()}
}

// Threshold is the lowest SeverityNumber a record of the scope is exported at: the first entry
// whose pattern matches (the layer sorts them so that the first is the exact name, else the
// longest prefix), else send_logs_level; 0: every record.
func (l *Levels) Threshold(scope string) uint32 {
	for _, s := range l.Scopes {
		pattern := s.GetPattern()
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(scope, prefix) {
				return s.GetMinSeverity()
			}
		} else if pattern == scope {
			return s.GetMinSeverity()
		}
	}
	return l.Min
}

// Keeps says whether a record of the scope and SeverityNumber passes the levels. A record with
// no severity (0) can't be judged and is kept, except where its scope is off.
func (l *Levels) Keeps(scope string, severity int) bool {
	switch t := l.Threshold(scope); t {
	case 0:
		return true
	case SeverityOff:
		return false
	default:
		return severity == 0 || severity >= int(t)
	}
}

// Mask is what a masked attribute's value becomes, whatever its type.
const Mask = "[REDACTED]"

// Redaction is drop_attributes and mask_attributes: globs on attribute keys, lowercase, matched
// ignoring ASCII case. A key both match is dropped.
type Redaction struct {
	Drop, Mask []string
}

// RedactionOf is the settings' redaction.
func RedactionOf(s *settingspb.Settings) *Redaction {
	return &Redaction{Drop: s.GetDropAttributes(), Mask: s.GetMaskAttributes()}
}

// None says whether the redaction leaves every attribute as it is.
func (r *Redaction) None() bool { return r == nil || len(r.Drop) == 0 && len(r.Mask) == 0 }

// Key says what becomes of the attribute with this key: dropped, masked (its value Mask), or
// neither.
func (r *Redaction) Key(key string) (drop, mask bool) {
	if r.None() {
		return false, false
	}
	if anyGlob(r.Drop, key) {
		return true, false
	}
	return false, anyGlob(r.Mask, key)
}

func anyGlob(patterns []string, key string) bool {
	for _, p := range patterns {
		if Glob(p, key, true) {
			return true
		}
	}
	return false
}
