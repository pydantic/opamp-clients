// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package sdksettings

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

var (
	fuzzLayerOnce sync.Once
	fuzzLayer     *Layer
	fuzzLayerErr  error
)

// The settings layer's agent flow through wazero, fed any saved client state and any remote
// config: neither start nor step ever fails (an unreadable state is a warning, a bad config is
// FAILED), an APPLIED config puts settings in force and reports what it applied, and a FAILED one
// changes nothing.
func FuzzSettingsLayer(f *testing.F) {
	f.Add([]byte{}, []byte(v1("head_sampling", override(`"ratio":0.25`))), "application/json", byte(1))
	f.Add([]byte(`not a state`), []byte(v1("head_sampling", override(`"ratio":"x"`))), "application/json", byte(0))
	f.Add([]byte{0x0a, 0x00}, []byte(every("instrumentations", override(`"disabled":["otelhttp","grpc"]`))), "", byte(3))
	f.Add([]byte{0xff}, []byte(`{}`), "text/yaml", byte(7))
	f.Add([]byte{}, []byte(every("signal_exports", `{"traces":{"mode":"override","enabled":true},"metrics":{"mode":"override","enabled":"maybe"},"logs":{"mode":"local"}}`)),
		"application/json", byte(2))
	f.Fuzz(func(t *testing.T, state, body []byte, contentType string, flags byte) {
		fuzzLayerOnce.Do(func() { fuzzLayer, fuzzLayerErr = New(context.Background()) })
		if fuzzLayerErr != nil {
			t.Fatal(fuzzLayerErr)
		}
		l, ctx := fuzzLayer, context.Background()
		host := &settingspb.Host{Live: declaring.Live, NextStart: declaring.NextStart}
		if flags&2 != 0 {
			host.Live = append([]string{"deactivate_instrumentations"}, declaring.Live...)
		}
		if flags&4 != 0 {
			// Neither deactivate_all_instrumentations nor send_metrics declared.
			host.Live = slices.DeleteFunc(slices.Clone(host.Live), func(k string) bool { return k == "send_metrics" })
			host.NextStart = []string{"deactivate_instrumentations"}
		}
		started, err := l.Start(ctx, &settingspb.AgentStart{Host: host, StateDir: flags&1 != 0, CoreState: state,
			Endpoint: "http://127.0.0.1:1", InstanceUid: make([]byte, 16), NowUnixMs: now, NowMonoMs: now})
		if err != nil {
			t.Fatalf("start %q: %v", state, err)
		}
		// A remote config comes decoded, from the core: its strings are UTF-8.
		contentType = strings.ToValidUTF8(contentType, "\uFFFD")
		config := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{policyFile: {Body: body, ContentType: contentType}}}}
		out, err := l.Step(ctx, &settingspb.AgentStep{Agent: started.GetAgent(), NowUnixMs: now + 1, NowMonoMs: now + 1,
			Input: &settingspb.AgentStep_RemoteConfig{RemoteConfig: config}})
		if err != nil {
			t.Fatalf("decide %q: %v", body, err)
		}
		var status *protobufs.RemoteConfigStatus
		var apply *settingspb.Settings
		for _, e := range out.GetEffects() {
			if e.GetStatus() != nil {
				status = e.GetStatus()
			}
			if e.GetApply() != nil {
				apply = e.GetApply()
			}
		}
		switch status.GetStatus() {
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
			if apply == nil {
				t.Fatalf("decide %q: applied nothing (flags %b)", body, flags)
			}
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
			if apply != nil || status.GetErrorMessage() == "" {
				t.Fatalf("decide %q: failed, yet %v", body, out)
			}
			if !proto.Equal(out.GetEffective(), started.GetEffective()) {
				t.Fatalf("decide %q: failed, but the effective config changed: %v, was %v", body, out.GetEffective(), started.GetEffective())
			}
		default:
			t.Fatalf("decide %q: status %v", body, status)
		}
		if string(status.GetLastRemoteConfigHash()) != "h" {
			t.Fatalf("decide %q: status for hash %q", body, status.GetLastRemoteConfigHash())
		}
	})
}
