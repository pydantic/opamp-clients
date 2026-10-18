// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The off switch (tests/shell-tables/off.json): every case through Setup, the variables in the
// process environment and the code's endpoint as WithEndpoint. Off: no client starts and the
// reason is said once, at WARN; on: the client starts and polls.
func TestOffSwitchTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "shell-tables", "off.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []struct {
			ID           string            `json:"id"`
			Env          map[string]string `json:"env"`
			CodeEndpoint *string           `json:"code_endpoint"`
			Off          bool              `json:"off"`
			Reason       string            `json:"reason"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) != 16 {
		t.Fatalf("%d cases, want 16", len(table.Cases))
	}
	for _, c := range table.Cases {
		t.Run(c.ID, func(t *testing.T) {
			var polls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				polls.Add(1)
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer srv.Close()
			// Every endpoint of the case is this server's, so an "on" case is seen to poll.
			local := func(v string) string {
				if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
					return srv.URL + "/v1/opamp"
				}
				return v
			}
			for _, name := range setupVariables {
				if strings.HasPrefix(name, "OTEL_") {
					t.Setenv(name, "")
				}
			}
			t.Setenv("OTEL_OPAMP_STATE_DIR", "none")
			t.Setenv("OTEL_TRACES_EXPORTER", "none")
			t.Setenv("OTEL_METRICS_EXPORTER", "none")
			t.Setenv("OTEL_LOGS_EXPORTER", "none")
			for k, v := range c.Env {
				t.Setenv(k, local(v))
			}
			var opts []Option
			if c.CodeEndpoint != nil {
				opts = append(opts, WithEndpoint(local(*c.CodeEndpoint)))
			}
			said := records(t)
			sdk, err := Setup(context.Background(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			if c.Off {
				if sdk.agent != nil || sdk.Controls != nil {
					t.Fatal("off, but a client started")
				}
			} else {
				if sdk.agent == nil {
					t.Fatal("on, but no client started")
				}
				for deadline := time.Now().Add(10 * time.Second); polls.Load() == 0; {
					if time.Now().After(deadline) {
						t.Fatal("on, but the server never heard from the client")
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			if err := sdk.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c.Off && polls.Load() != 0 {
				t.Fatalf("off, but the server was polled %d times", polls.Load())
			}
			var off []string
			for _, s := range *said {
				if strings.Contains(s, "opamp: off") {
					off = append(off, s)
				}
			}
			var want []string
			if c.Reason != "" {
				want = []string{"WARN opamp: " + c.Reason}
			}
			if strings.Join(off, "\n") != strings.Join(want, "\n") {
				t.Fatalf("said %q, want %q", off, want)
			}
		})
	}
}
