// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package sdksettings

import (
	"bytes"
	"context"
	"errors"
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

// The settings layer through wazero, fed any saved document and any remote config: neither call
// ever fails (an unreadable saved document is a warning, a bad config is FAILED), a document the
// layer keeps reads back the same with no warning, an APPLIED config's saved document loads as
// exactly what it applied, saved when the host can keep state, a config whose save fails is
// FAILED, and a FAILED one changes nothing.
func FuzzSettingsLayer(f *testing.F) {
	f.Add([]byte(v1("signal_exports", logsOff)), []byte(v1("head_sampling", override(`"ratio":0.25`))), "application/json", byte(1))
	f.Add([]byte(`not json`), []byte(v1("head_sampling", override(`"ratio":"x"`))), "application/json", byte(0))
	f.Add([]byte{}, []byte(every("instrumentations", override(`"disabled":["otelhttp","grpc"]`))), "", byte(3))
	f.Add([]byte(every("instrumentations", override(`"all_disabled":true`))), []byte(`{}`), "text/yaml", byte(7))
	f.Add([]byte(`{"schema":"logfire-sdk-policy/v1","head_sampling":{"mode":"override","ratio":1e309}}`),
		[]byte(every("signal_exports", `{"traces":{"mode":"override","enabled":true},"metrics":{"mode":"override","enabled":"maybe"},"logs":{"mode":"local"}}`)),
		"application/json", byte(2))
	f.Fuzz(func(t *testing.T, saved, body []byte, contentType string, flags byte) {
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
		// flags&1: the host keeps state; flags&8: its save fails.
		var asked []byte
		var save func([]byte) error
		if flags&1 != 0 {
			save = func(doc []byte) error {
				asked = doc
				if flags&8 != 0 {
					return errors.New("the disk is full")
				}
				return nil
			}
		}

		loaded, err := l.Load(ctx, saved, host)
		if err != nil {
			t.Fatalf("load %q: %v", saved, err)
		}
		if loaded.GetWarning() != "" && !proto.Equal(loaded.GetSettings(), &settingspb.Settings{}) {
			t.Fatalf("load %q: warned, yet settings %v", saved, loaded.GetSettings())
		}
		again, err := l.Load(ctx, loaded.GetCurrent(), host)
		if err != nil || again.GetWarning() != "" || !proto.Equal(again.GetSettings(), loaded.GetSettings()) {
			t.Fatalf("load %q: kept %q, which reads back as %v (%v), not %v", saved, loaded.GetCurrent(), again, err, loaded.GetSettings())
		}

		// A remote config comes decoded, from the core: its strings are UTF-8.
		contentType = strings.ToValidUTF8(contentType, "\uFFFD")
		config := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{policyFile: {Body: body, ContentType: contentType}}}}
		d, err := l.Decide(ctx, config, loaded.GetCurrent(), loaded.GetCurrent(), host, save)
		if err != nil {
			t.Fatalf("decide %q: %v", body, err)
		}
		switch d.GetStatus().GetStatus() {
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
			if d.GetApply() == nil || flags&9 == 9 || (save != nil) != bytes.Equal(asked, d.GetCurrent()) {
				t.Fatalf("decide %q: applied %v, saved %q (flags %b)", body, d.GetApply(), asked, flags)
			}
			kept, err := l.Load(ctx, d.GetCurrent(), host)
			if err != nil || kept.GetWarning() != "" || !proto.Equal(kept.GetSettings(), d.GetApply()) {
				t.Fatalf("decide %q: applied %v, but what it keeps (%q) loads as %v (%v)", body, d.GetApply(), d.GetCurrent(), kept, err)
			}
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
			if d.GetApply() != nil || d.GetStatus().GetErrorMessage() == "" {
				t.Fatalf("decide %q: failed, yet %v", body, d)
			}
			if asked != nil && flags&8 != 0 && d.GetStatus().GetErrorMessage() != "can't save the settings: the disk is full" {
				t.Fatalf("decide %q: its save failed, yet %q", body, d.GetStatus().GetErrorMessage())
			}
			kept, err := l.Load(ctx, d.GetCurrent(), host)
			if err != nil || !proto.Equal(kept.GetSettings(), loaded.GetSettings()) {
				t.Fatalf("decide %q: failed, but what is in force changed: %v, was %v", body, kept.GetSettings(), loaded.GetSettings())
			}
		default:
			t.Fatalf("decide %q: status %v", body, d.GetStatus())
		}
		if string(d.GetStatus().GetLastRemoteConfigHash()) != "h" {
			t.Fatalf("decide %q: status for hash %q", body, d.GetStatus().GetLastRemoteConfigHash())
		}
	})
}
