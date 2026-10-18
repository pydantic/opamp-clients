// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"strings"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// severityOff is a scope level's threshold under `off`: past every SeverityNumber.
const severityOff = 0xFFFF

// logLevels are send_logs_level and send_logs_level_by_scope, ready to judge a record.
type logLevels struct {
	min    uint32 // send_logs_level's SeverityNumber; 0: every record
	scopes []*settingspb.ScopeLevel
}

// threshold is the lowest SeverityNumber a record of the scope is exported at: the first entry
// whose pattern matches (the layer sorts them so that the first is the exact name, else the
// longest prefix), else send_logs_level; 0: every record.
func (l *logLevels) threshold(scope string) uint32 {
	for _, s := range l.scopes {
		pattern := s.GetPattern()
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(scope, prefix) {
				return s.GetMinSeverity()
			}
		} else if pattern == scope {
			return s.GetMinSeverity()
		}
	}
	return l.min
}

// keeps says whether a record of the scope and SeverityNumber passes the levels. A record with
// no severity (0) can't be judged and is kept, except where its scope is off.
func (l *logLevels) keeps(scope string, severity int) bool {
	switch t := l.threshold(scope); t {
	case 0:
		return true
	case severityOff:
		return false
	default:
		return severity == 0 || severity >= int(t)
	}
}
