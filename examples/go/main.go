// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/pydantic/opamp-clients/go/opamp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// liveSampler is a sampler the server can change while the program runs.
type liveSampler struct {
	current atomic.Pointer[sdktrace.Sampler]
}

func (s *liveSampler) set(to sdktrace.Sampler) { s.current.Store(&to) }

func (s *liveSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return (*s.current.Load()).ShouldSample(p)
}

func (s *liveSampler) Description() string { return (*s.current.Load()).Description() }

// liveExporter is an exporter the server can point elsewhere while the program runs. Until it
// says where, spans go nowhere.
type liveExporter struct {
	current atomic.Pointer[sdktrace.SpanExporter]
}

// set points it at `to`, and shuts down the exporter it replaces.
func (e *liveExporter) set(ctx context.Context, to sdktrace.SpanExporter) {
	if old := e.current.Swap(&to); old != nil {
		_ = (*old).Shutdown(ctx)
	}
}

func (e *liveExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if cur := e.current.Load(); cur != nil {
		return (*cur).ExportSpans(ctx, spans)
	}
	return nil
}

func (e *liveExporter) Shutdown(ctx context.Context) error {
	if cur := e.current.Load(); cur != nil {
		return (*cur).Shutdown(ctx)
	}
	return nil
}

// exporter is an OTLP/HTTP exporter for the traces part of telemetry settings: the endpoint is
// the full URL (.../v1/traces), with the headers offered.
func exporter(ctx context.Context, t *protobufs.TelemetryConnectionSettings) (sdktrace.SpanExporter, error) {
	headers := map[string]string{}
	for _, h := range t.GetHeaders().GetHeaders() {
		headers[h.Key] = h.Value
	}
	return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(t.DestinationEndpoint), otlptracehttp.WithHeaders(headers))
}

// settings is the remote config this program understands: {"sampling_ratio": 0.25}.
type settings struct {
	SamplingRatio float64 `json:"sampling_ratio"`
}

// description is the agent description, from the SDK's resource.
func description(res *resource.Resource) *protobufs.AgentDescription {
	identifying := map[attribute.Key]bool{
		semconv.ServiceNameKey: true, semconv.ServiceNamespaceKey: true, semconv.ServiceInstanceIDKey: true,
	}
	d := &protobufs.AgentDescription{}
	for _, kv := range res.Attributes() {
		pkv := &protobufs.KeyValue{Key: string(kv.Key), Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: kv.Value.Emit()}}}
		if identifying[kv.Key] {
			d.IdentifyingAttributes = append(d.IdentifyingAttributes, pkv)
		} else {
			d.NonIdentifyingAttributes = append(d.NonIdentifyingAttributes, pkv)
		}
	}
	return d
}

func effective(s settings) *protobufs.EffectiveConfig {
	body, _ := json.Marshal(s)
	return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: body, ContentType: "application/json"}}}}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// One UUID is both the OpAMP instance UID and service.instance.id, so the fleet view and
	// the telemetry name the same instance.
	uid := uuid.Must(uuid.NewV7())
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName("checkout"), semconv.ServiceInstanceID(uid.String())))
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		panic(err)
	}

	initial := settings{SamplingRatio: 1}
	sampler := &liveSampler{}
	sampler.set(sdktrace.TraceIDRatioBased(initial.SamplingRatio))
	// Spans go wherever the server says (telemetry settings), through a live exporter.
	live := &liveExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
		sdktrace.WithBatcher(live, sdktrace.WithBatchTimeout(500*time.Millisecond)))
	otel.SetTracerProvider(tp)

	// Notifications arrive on the client's goroutine: hand them to one of ours.
	endpoint := os.Getenv("OPAMP_ENDPOINT") // https://.../v1/opamp, or wss://... for WebSocket
	notes := make(chan opamp.Notification, 16)
	client, err := opamp.Start(ctx, opamp.Config{
		Endpoint:    endpoint,
		InstanceUID: [16]byte(uid),
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnTraces),
		// Header: http.Header{"Authorization": {"Bearer " + token}},
	}, opamp.Status{
		Description:     description(res),
		Health:          &protobufs.ComponentHealth{Healthy: true},
		EffectiveConfig: effective(initial),
	}, func(_ *opamp.Client, n opamp.Notification) { notes <- n })
	if err != nil {
		panic(err)
	}
	go func() {
		for n := range notes {
			if n.Kind == opamp.Stopped && n.Err != nil {
				// The client stopped on its own (a bug): don't carry on unmanaged.
				fmt.Fprintln(os.Stderr, "OpAMP client stopped:", n.Err)
				os.Exit(1)
			}
			if n.Kind == opamp.ConnectionSettings {
				// Where to send our own telemetry: all the settings, not only what changed.
				status := &protobufs.ConnectionSettingsStatus{LastConnectionSettingsHash: n.ConnectionSettings.Hash,
					Status: protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED}
				if t := n.ConnectionSettings.OwnTraces; t != nil {
					if e, err := exporter(ctx, t); err != nil {
						status.Status = protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED
						status.ErrorMessage = err.Error()
					} else {
						live.set(ctx, e)
					}
				}
				client.SetConnectionSettingsStatus(status)
				continue
			}
			if n.Kind != opamp.RemoteConfig {
				continue
			}
			status := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.ConfigHash}
			var next struct {
				SamplingRatio *float64 `json:"sampling_ratio"`
			}
			file := n.RemoteConfig.GetConfig().GetConfigMap()[""]
			err := json.Unmarshal(file.GetBody(), &next)
			if r := next.SamplingRatio; err != nil || r == nil || *r < 0 || *r > 1 {
				status.Status = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
				status.ErrorMessage = `want {"sampling_ratio": 0..1}`
			} else {
				sampler.set(sdktrace.TraceIDRatioBased(*r))
				status.Status = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
				client.SetEffectiveConfig(effective(settings{SamplingRatio: *r}))
			}
			client.SetRemoteConfigStatus(status)
		}
	}()

	// The application's work: a span every 100 ms.
	go func() {
		tracer := otel.Tracer("checkout")
		for ctx.Err() == nil {
			_, span := tracer.Start(ctx, "checkout")
			span.End()
			time.Sleep(100 * time.Millisecond)
		}
	}()

	<-ctx.Done() // the program runs until interrupted

	// Stop: the server hears the agent go (agent_disconnect), then the SDK flushes.
	_ = client.Stop(context.Background())
	_ = tp.Shutdown(context.Background())
}
