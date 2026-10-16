// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

type capCase struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Now      uint64          `json:"now_unix_ms"`
	Signal   string          `json:"signal"`
	Events   []struct {
		AtNs    int64  `json:"at_ns"`
		Parent  string `json:"parent"`
		Sampled bool   `json:"sampled"`
		Records int    `json:"records"`
	} `json:"events"`
	Want []uint64 `json:"want"`
}

// fakeClock is a monotonic clock a test sets.
type fakeClock struct{ ns atomic.Int64 }

func (f *fakeClock) now() int64 { return f.ns.Load() }

// withClock gives c a clock the test sets.
func withClock(c *Controls) *fakeClock {
	f := &fakeClock{}
	c.clock, c.Sampler.clock = f.now, f.now
	return f
}

// batchOf is a batch of n records of the scope "app", with no severity, each record's body its
// index. One batch is kept and grown, as the caps.json cases go up to two million records.
func batchOf(n int) []sdklog.Record {
	for len(batch) < n {
		var r sdklog.Record
		r.SetBody(attribute.IntValue(len(batch)))
		batch = append(batch, r)
	}
	return batch[:n:n]
}

var batch []sdklog.Record

// hugeBatch is the most records a test batch is made of.
const hugeBatch = 100_000

// countSink counts what the gate exported, and checks it is the start of the batch.
type countSink struct {
	recordSink
	n   uint64
	err error
}

func (s *countSink) Export(_ context.Context, records []sdklog.Record) error {
	for i := range records {
		if records[i].Body().AsInt64() != int64(i) {
			s.err = fmt.Errorf("record %d of the batch went on in place of record %d: the cap keeps the first",
				records[i].Body().AsInt64(), i)
		}
	}
	s.n += uint64(len(records))
	return nil
}

// exportBatch sends a batch of n through c's log gate and says how many records reached the
// sink, checking they are the batch's first ones.
func exportBatch(t testing.TB, c *Controls, n int) uint64 {
	t.Helper()
	sink := &countSink{}
	if err := c.Logs(sink).Export(context.Background(), batchOf(n)); err != nil {
		t.Fatal(err)
	}
	if sink.err != nil {
		t.Fatal(sink.err)
	}
	return sink.n
}

// A log cap's run, batch by batch at the vectors' times (caps.json, the logs cases).
func TestTheLogCapVectors(t *testing.T) {
	v := readVectors[struct{ Cases []capCase }](t, "caps.json")
	ran := 0
	for _, c := range v.Cases {
		if c.Signal != "logs" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			clock := withClock(controls)
			controls.Apply(settingsOf(t, c.Settings, c.Now))
			var got []uint64
			for _, e := range c.Events {
				clock.ns.Store(e.AtNs)
				if e.Records > hugeBatch {
					// Two million records would be over a gigabyte of sdklog.Record: the gate's
					// own bucket, at the gate's clock, takes the batch's size instead.
					got = append(got, controls.gates.Load().logsCap.take(controls.clock(), uint64(e.Records)))
					continue
				}
				got = append(got, exportBatch(t, controls, e.Records))
			}
			if fmt.Sprint(got) != fmt.Sprint(c.Want) {
				t.Fatalf("passed %v, want %v", got, c.Want)
			}
		})
	}
	if ran < 10 {
		t.Fatalf("%d log cases: caps.json was cut short", ran)
	}
}

// The spec's edge cases for the log cap, on a live gate.
func TestTheLogCapLive(t *testing.T) {
	c := NewControls()
	clock := withClock(c)
	apply := func(doc string) { c.Apply(settingsOf(t, []byte(doc), 0)) }

	if n := exportBatch(t, c, 50); n != 50 {
		t.Fatalf("no cap: %d of 50", n)
	}
	apply(logCap("10"))
	if n := exportBatch(t, c, 15); n != 10 {
		t.Fatalf("a full bucket of 10: %d of 15", n)
	}
	apply(logCap("10")) // the same cap again: the same bucket, still empty
	if n := exportBatch(t, c, 5); n != 0 {
		t.Fatalf("the same cap re-applied refilled the bucket: %d of 5", n)
	}
	clock.ns.Add(500_000_000)
	if n := exportBatch(t, c, 15); n != 5 {
		t.Fatalf("half a second at 10 a second: %d of 15, want 5", n)
	}
	apply(logCap("20")) // a change of n: a full bucket
	if n := exportBatch(t, c, 30); n != 20 {
		t.Fatalf("a new cap of 20: %d of 30", n)
	}
	apply(every()) // local: no cap, at once
	if n := exportBatch(t, c, 30); n != 30 {
		t.Fatalf("cap removed: %d of 30", n)
	}
}

// Records the level drops take no token; the cap counts only what passes it.
func TestRecordsDroppedByLevelTakeNoToken(t *testing.T) {
	c := NewControls()
	withClock(c)
	c.Apply(settingsOf(t, []byte(every("log_levels", override(`"level":"warn"`), "log_rate_limit", override(`"records_per_second":2`))), 0))
	sink := &recordSink{}
	batch := make([]sdklog.Record, 6)
	for i := range batch {
		batch[i].SetSeverity(log.SeverityDebug)
		if i >= 4 {
			batch[i].SetSeverity(log.SeverityError)
		}
	}
	if err := c.Logs(sink).Export(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 2 || got[0].Severity() != log.SeverityError {
		t.Fatalf("exported %d, want the 2 errors: the 4 debug records took tokens", len(got))
	}
}

// logCap is a policy whose log_rate_limit control overrides the records a second.
func logCap(perSecond string) string {
	return every("log_rate_limit", override(`"records_per_second":`+perSecond))
}

// The gate's bucket against the layer's (Evaluate), on generated runs.
func TestTheLogCapMatchesTheLayers(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 200; i++ {
		perSecond := []string{"0.25", "0.5", "1", "1.5", "3", "7.5", "100"}[r.IntN(7)]
		doc := []byte(logCap(perSecond))
		run := &settingspb.CapRun{Signal: settingspb.CapRun_LOGS}
		at := uint64(r.IntN(1_000_000_000))
		for range 1 + r.IntN(8) {
			at += uint64(r.IntN(1_500_000_000))
			if r.IntN(8) == 0 && at > 300_000_000 {
				at -= 300_000_000 // a clock that goes back
			}
			run.Events = append(run.Events, &settingspb.CapEvent{AtNs: at, Records: uint64(r.IntN(6))})
		}
		want, err := testLayer(t).Evaluate(context.Background(), &settingspb.Evaluate{Settings: doc, Host: goHost(),
			NowUnixMs: vectorNow, Input: &settingspb.Evaluate_Cap{Cap: run}})
		if err != nil || want.GetRefused() != "" {
			t.Fatalf("%s: %v %v", doc, want, err)
		}
		c := NewControls()
		clock := withClock(c)
		c.Apply(settingsOf(t, doc, 0))
		var got []uint64
		for _, e := range run.Events {
			clock.ns.Store(int64(e.AtNs))
			got = append(got, exportBatch(t, c, int(e.Records)))
		}
		if fmt.Sprint(got) != fmt.Sprint(want.GetPassed()) {
			t.Fatalf("%s %v: the gate passed %v, the layer %v", doc, run.Events, got, want.GetPassed())
		}
	}
}
