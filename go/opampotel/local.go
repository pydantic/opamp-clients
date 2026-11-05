// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"strconv"
	"strings"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// localSettings is what the SDK Setup built does without the server, for the local report
// (contracts' logfire-sdk-local/v1), as far as it can be told: its sampler's rate (the one
// OTEL_TRACES_SAMPLER names, which the package's sampler wraps), and whether it exports each signal
// at all (an exporter Setup built for it, found). Setup builds every provider itself, so each
// signal is told. Left out: the log level, as the SDK has none of its own for the application's
// log records.
func localSettings(getenv func(string) string, found []*settingspb.FoundExporter) *settingspb.Local {
	exports := map[string]bool{}
	for _, f := range found {
		exports[f.GetSignal()] = true
	}
	return &settingspb.Local{
		SamplingRate: ptr(decimal(headFromEnv(getenv).rate())),
		SendTraces:   ptr(exports["traces"]),
		SendMetrics:  ptr(exports["metrics"]),
		SendLogs:     ptr(exports["logs"]),
	}
}

// decimal is a ratio as decimal text, as the local report takes it: "1", "0.25", "0.00001"
// (the shortest digits that read back as x, never an exponent).
func decimal(x float64) string {
	text := strconv.FormatFloat(x, 'f', -1, 64)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

func ptr[T any](v T) *T { return &v }

// withLocal puts what the SDK does without the server in the host, once Setup has built it, and
// the settings loaded at start in force again under that host: the effective config the client
// starts with reports it beside the policy in force. Before start.
func (a *agent) withLocal(local *settingspb.Local) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.server.local = local
	now := a.now()
	a.decideAgain(now, unixMs(now))
	a.arm()
}
