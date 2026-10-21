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

// redaction is drop_attributes and mask_attributes (package match, which the Collector's
// logfirepolicy processor redacts with too).
type redaction struct{ match.Redaction }

func (r *redaction) none() bool { return r == nil || r.Redaction.None() }

// apply is the attributes as exported: dropped ones gone, masked ones the string [REDACTED], the
// rest as they were, in their order; changed says whether any is. attrs itself is never changed.
func (r *redaction) apply(attrs []attribute.KeyValue) (out []attribute.KeyValue, changed bool) {
	if r.none() {
		return attrs, false
	}
	for i, kv := range attrs {
		key := string(kv.Key)
		dropped, masked := r.Key(key)
		if !dropped && !masked {
			if changed {
				out = append(out, kv)
			}
			continue
		}
		if !changed {
			changed = true
			out = append(make([]attribute.KeyValue, 0, len(attrs)), attrs[:i]...)
		}
		if masked {
			out = append(out, attribute.String(key, mask))
		}
	}
	if !changed {
		return attrs, false
	}
	return out, true
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

// redactRecords redacts the records' attributes. A record redaction changes is replaced by a
// changed clone; records is copied first unless copied says it is already the gate's own.
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
		if !changed {
			continue
		}
		if !copied {
			records, copied = append([]sdklog.Record(nil), records...), true
		}
		clone := records[i].Clone()
		clone.SetAttributes(out...)
		records[i] = clone
	}
	return records
}
