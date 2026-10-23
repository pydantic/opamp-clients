// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import "github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"

// logLevels are send_logs_level and send_logs_level_by_scope, ready to judge a record (package
// match).
type logLevels = match.Levels
