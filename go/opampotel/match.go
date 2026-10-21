// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// The sampling rules' matchers, native because they run once a span. The layer reads and checks
// the settings; these only match, the layer's way, with the globs of package match: the shared
// vectors in testdata/sdk-settings and the layer's Evaluate call hold them to it.

// rule is a sampling rule, ready to match: the first rule a root span matches decides its rate.
type rule struct {
	sampler    sdktrace.Sampler // the SDK's trace-ID ratio sampler at the rule's rate
	name, path *string
	kind       trace.SpanKind // 0: any
	attributes []attributeMatch
}

type attributeMatch struct {
	key     attribute.Key
	pattern string
}

func newRule(r *settingspb.SamplingRule) rule {
	out := rule{sampler: sdktrace.TraceIDRatioBased(r.GetRate()), name: r.Name, path: r.Path}
	if r.Kind != nil {
		// The layer's SpanKind numbers are OTLP's, and so are the Go API's.
		out.kind = trace.SpanKind(r.GetKind())
	}
	for k, p := range r.GetAttributes() {
		out.attributes = append(out.attributes, attributeMatch{attribute.Key(k), p})
	}
	return out
}

// matches says whether the span, as the sampler sees it at its start, matches every matcher the
// rule has (a rule with none matches every span).
func (r *rule) matches(p sdktrace.SamplingParameters) bool {
	if r.name != nil && !match.Glob(*r.name, p.Name, false) {
		return false
	}
	if r.kind != 0 {
		kind := p.Kind
		if kind == trace.SpanKindUnspecified {
			kind = trace.SpanKindInternal // as the SDK starts it
		}
		if kind != r.kind {
			return false
		}
	}
	if r.path != nil {
		path, ok := spanPath(p.Attributes)
		if !ok || !match.Glob(*r.path, path, false) {
			return false
		}
	}
	for _, a := range r.attributes {
		v, ok := first(p.Attributes, a.key)
		if !ok {
			return false
		}
		text, ok := attributeText(v)
		if !ok || !match.Glob(a.pattern, text, false) {
			return false
		}
	}
	return true
}

// first is the value of the first attribute named key: the one a rule sees.
func first(attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// attributeText is the text an attribute's value matches by: a string's own, an integer's
// decimal, true or false. A double or an array has none (languages print floats differently).
func attributeText(v attribute.Value) (string, bool) {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString(), true
	case attribute.INT64:
		return strconv.FormatInt(v.AsInt64(), 10), true
	case attribute.BOOL:
		return strconv.FormatBool(v.AsBool()), true
	}
	return "", false
}

// spanPath is what a rule's `path` matches: url.path, or else http.target up to its first `?`.
// An attribute that isn't a string is passed over.
func spanPath(attrs []attribute.KeyValue) (string, bool) {
	if v, ok := first(attrs, "url.path"); ok && v.Type() == attribute.STRING {
		return v.AsString(), true
	}
	if v, ok := first(attrs, "http.target"); ok && v.Type() == attribute.STRING {
		path, _, _ := strings.Cut(v.AsString(), "?")
		return path, true
	}
	return "", false
}
