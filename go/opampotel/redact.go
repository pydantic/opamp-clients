// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
)

// mask is what a masked attribute's value becomes, whatever its type.
const mask = match.Mask

// redaction is drop_attributes and mask_attributes, then scrub_defaults and scrub_patterns
// (package match).
type redaction struct {
	match.Redaction
	scrub match.Scrubbing
}

func (r *redaction) none() bool { return r == nil || r.Redaction.None() && r.scrub.None() }

// apply is the attributes as exported: dropped ones gone, masked ones the string [REDACTED], the
// rest scrubbed (scrubValue), in their order; changed says whether any is. attrs itself is never
// changed.
func (r *redaction) apply(attrs []attribute.KeyValue) (out []attribute.KeyValue, changed bool) {
	if r.none() {
		return attrs, false
	}
	for i, kv := range attrs {
		key := string(kv.Key)
		dropped, masked := r.Key(key)
		v, scrubbed := kv.Value, false
		if !dropped && !masked {
			v, scrubbed = scrubValue(&r.scrub, &key, kv.Value)
		}
		if !dropped && !masked && !scrubbed {
			if changed {
				out = append(out, kv)
			}
			continue
		}
		if !changed {
			changed = true
			out = append(make([]attribute.KeyValue, 0, len(attrs)), attrs[:i]...)
		}
		switch {
		case masked:
			out = append(out, attribute.String(key, mask))
		case scrubbed:
			out = append(out, attribute.KeyValue{Key: kv.Key, Value: v})
		}
	}
	if !changed {
		return attrs, false
	}
	return out, true
}

// scrubValue is a value as scrubbing leaves it (an attribute's, a map member's, a slice's item, or
// a log body), and whether it changed: key is the attribute or member it is the value of (nil: a
// body or an item). A key scrubbing replaces makes the value that string, whatever its type; else
// a string is scrubbed (match.Scrubbing.Text), and so is each string in a slice and each member of
// a map, as an attribute. v itself is never changed.
func scrubValue(s *match.Scrubbing, key *string, v attribute.Value) (attribute.Value, bool) {
	if s.None() {
		return v, false
	}
	if key != nil {
		switch what, with := s.Key(*key); what {
		case match.Keep:
			return v, false
		case match.Replace:
			return attribute.StringValue(with), true
		}
	}
	switch v.Type() {
	case attribute.STRING:
		if text, ok := s.Text(v.AsString()); ok {
			return attribute.StringValue(text), true
		}
	case attribute.STRINGSLICE:
		items, changed := v.AsStringSlice(), false
		for i, item := range items {
			if text, ok := s.Text(item); ok {
				items[i], changed = text, true
			}
		}
		if changed {
			return attribute.StringSliceValue(items), true
		}
	case attribute.SLICE:
		items, changed := v.AsSlice(), false
		for i, item := range items {
			if x, ok := scrubValue(s, nil, item); ok {
				items[i], changed = x, true
			}
		}
		if changed {
			return attribute.SliceValue(items...), true
		}
	case attribute.MAP:
		members, changed := v.AsMap(), false
		for i, m := range members {
			k := string(m.Key)
			if x, ok := scrubValue(s, &k, m.Value); ok {
				members[i].Value, changed = x, true
			}
		}
		if changed {
			return attribute.MapValue(members...), true
		}
	}
	return v, false
}

// redactedSpan is a span as exported: the SDK's, with redacted attributes, events and links.
// Embedding the interface gives it the SDK's unexported method, so it is a ReadOnlySpan.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attributes []attribute.KeyValue
	events     []sdktrace.Event
	links      []sdktrace.Link
}

func (s *redactedSpan) Attributes() []attribute.KeyValue { return s.attributes }
func (s *redactedSpan) Events() []sdktrace.Event         { return s.events }
func (s *redactedSpan) Links() []sdktrace.Link           { return s.links }

// redactSpans is the spans as exported: a span redaction changes is a redactedSpan over it, and
// the rest are themselves; the slice is a new one when any is changed.
func redactSpans(r *redaction, spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	out, copied := spans, false
	for i, s := range spans {
		v, changed := redactSpan(r, s)
		if !changed {
			continue
		}
		if !copied {
			out, copied = append([]sdktrace.ReadOnlySpan(nil), spans...), true
		}
		out[i] = v
	}
	return out
}

func redactSpan(r *redaction, s sdktrace.ReadOnlySpan) (sdktrace.ReadOnlySpan, bool) {
	attrs, changed := r.apply(s.Attributes())
	events, eventsCopied := s.Events(), false
	for i, e := range events {
		if a, ok := r.apply(e.Attributes); ok {
			if !eventsCopied {
				events, eventsCopied = append([]sdktrace.Event(nil), events...), true
			}
			events[i].Attributes = a
		}
	}
	links, linksCopied := s.Links(), false
	for i, l := range links {
		if a, ok := r.apply(l.Attributes); ok {
			if !linksCopied {
				links, linksCopied = append([]sdktrace.Link(nil), links...), true
			}
			links[i].Attributes = a
		}
	}
	if !changed && !eventsCopied && !linksCopied {
		return s, false
	}
	return &redactedSpan{ReadOnlySpan: s, attributes: attrs, events: events, links: links}, true
}

// redactRecords redacts the records' attributes, and scrubs their bodies. A record redaction
// changes is replaced by a changed clone; records is copied first unless copied says it is
// already the gate's own.
//
// The SDK can't take an attribute out of a record, only set them all again, which also sets its
// count of attributes dropped by the SDK's limits to 0: a record whose limits had dropped some
// and that redaction changes is exported as if none were.
func redactRecords(r *redaction, records []sdklog.Record, copied bool) []sdklog.Record {
	for i := range records {
		var attrs []attribute.KeyValue
		records[i].WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs = append(attrs, kv)
			return true
		})
		out, changed := r.apply(attrs)
		body, bodyChanged := scrubValue(&r.scrub, nil, records[i].Body())
		if !changed && !bodyChanged {
			continue
		}
		if !copied {
			records, copied = append([]sdklog.Record(nil), records...), true
		}
		clone := records[i].Clone()
		if changed {
			clone.SetAttributes(out...)
		}
		if bodyChanged {
			clone.SetBody(body)
		}
		records[i] = clone
	}
	return records
}
