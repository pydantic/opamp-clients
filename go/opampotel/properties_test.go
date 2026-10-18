// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/binary"
	"math/rand"
	"testing"
	"testing/quick"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// Properties over generated sequences, each against a model written from the settings contract
// (docs/sdk-settings.md) and the specification's ratio sampler, not from this package's code.

var scopes = []string{otelhttp, "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc", "checkout", "github.com/XSAM/otelsql"}
var instNames = []string{"otelhttp", "otelgrpc", "checkout", "otelsql"}

// Switches set and spans started, in any order: a span is recorded exactly when its scope was
// on at the moment it started, whatever was switched before or after.
func TestPropertyASpanIsRecordedExactlyWhenItsInstrumentationWasOn(t *testing.T) {
	prop := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		c := NewControls()
		rec := tracetest.NewSpanRecorder()
		tp := c.TracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
		off, all, want := map[string]bool{}, false, 0
		for n := r.Intn(40); n > 0; n-- {
			if r.Intn(3) == 0 {
				s := &settingspb.Settings{}
				off, all = map[string]bool{}, r.Intn(6) == 0
				if all {
					s.DeactivateAllInstrumentations = proto.Bool(true)
				}
				var list []string
				for _, name := range instNames {
					if r.Intn(2) == 0 {
						off[name] = true
						list = append(list, name)
					}
				}
				if len(list) > 0 {
					s.DeactivateInstrumentations = &settingspb.Names{Names: list}
				}
				c.Apply(s)
				continue
			}
			i := r.Intn(len(scopes))
			_, span := tp.Tracer(scopes[i]).Start(context.Background(), "s")
			span.End()
			if !all && !off[instNames[i]] {
				want++
			}
		}
		return len(rec.Ended()) == want
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// ratioSamples is the specification's TraceIdRatioBased for the Go SDK: the low 8 bytes, shifted
// right once, under the rate's share of 2^63.
func ratioSamples(rate float64, low uint64) bool {
	return low>>1 < uint64(rate*(1<<63))
}

// A root span is decided by the last rate set, or else by the configured sampler, whatever the
// order the rates were set in.
func TestPropertyARootIsSampledByTheLastRateOrElseTheConfiguredSampler(t *testing.T) {
	prop := func(seed int64, low uint64, configuredOn bool) bool {
		r := rand.New(rand.NewSource(seed))
		configured := sdktrace.NeverSample()
		if configuredOn {
			configured = sdktrace.AlwaysSample()
		}
		s := NewLiveSampler(nil).Wrap(configured)
		var rate *float64
		for n := r.Intn(8); n > 0; n-- {
			if r.Intn(3) == 0 {
				rate = nil
			} else {
				rate = proto.Float64(r.Float64())
			}
			s.Rate(rate)
		}
		var id [16]byte
		id[0] = 1
		binary.BigEndian.PutUint64(id[8:], low)
		want := configuredOn
		if rate != nil {
			want = ratioSamples(*rate, low)
		}
		return (sample(s, trace.TraceID(id)) == sdktrace.RecordAndSample) == want
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
