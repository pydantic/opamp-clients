// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.api.common.Value;
import io.opentelemetry.api.logs.Severity;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.sdk.common.InstrumentationScopeInfo;
import io.opentelemetry.sdk.logs.data.Body;
import io.opentelemetry.sdk.logs.data.LogRecordData;
import io.opentelemetry.sdk.resources.Resource;
import io.opentelemetry.sdk.trace.data.DelegatingSpanData;
import io.opentelemetry.sdk.trace.data.EventData;
import io.opentelemetry.sdk.trace.data.ExceptionEventData;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.data.SpanData;
import java.util.ArrayList;
import java.util.List;

/**
 * {@code drop_attributes} and {@code mask_attributes}, at export: an attribute whose key a drop
 * pattern matches (ignoring ASCII case) is removed, one a mask pattern matches has the string
 * {@code [REDACTED]} for its value, whatever its type; a key both match is dropped. Span, span
 * event, span link and log record attributes; never resource or scope attributes, names or bodies.
 *
 * <p>The application's span or record is never changed: other processors and exporters share it.
 * A redacted one is a new object over it, made only when an attribute changes. A drop leaves
 * {@code dropped_attributes_count} as it was: the total attribute count goes down with it.
 */
final class Redaction {
  static final String MASK = "[REDACTED]";
  static final Redaction NONE = new Redaction(List.of(), List.of());

  private final List<Glob> drop;
  private final List<Glob> mask;

  private Redaction(List<Glob> drop, List<Glob> mask) {
    this.drop = drop;
    this.mask = mask;
  }

  /** The settings' redaction; {@link #NONE} without patterns. */
  static Redaction of(SettingsProto.Settings s) {
    if (s.getDropAttributesCount() == 0 && s.getMaskAttributesCount() == 0) {
      return NONE;
    }
    return new Redaction(s.getDropAttributesList().stream().map(Glob::ignoringAsciiCase).toList(),
        s.getMaskAttributesList().stream().map(Glob::ignoringAsciiCase).toList());
  }

  boolean any() {
    return !drop.isEmpty() || !mask.isEmpty();
  }

  /** Attributes as exported, and how many were dropped. {@code attributes} itself when none changes. */
  record Redacted(Attributes attributes, int dropped) {}

  Redacted apply(Attributes attributes) {
    boolean[] changed = {false};
    int[] dropped = {0};
    AttributesBuilder b = Attributes.builder();
    attributes.forEach((key, value) -> {
      String name = key.getKey();
      if (drop.stream().anyMatch(g -> g.matches(name))) {
        dropped[0]++;
        changed[0] = true;
      } else if (mask.stream().anyMatch(g -> g.matches(name))) {
        b.put(AttributeKey.stringKey(name), MASK);
        changed[0] = true;
      } else {
        put(b, key, value);
      }
    });
    return changed[0] ? new Redacted(b.build(), dropped[0]) : new Redacted(attributes, 0);
  }

  @SuppressWarnings("unchecked")
  private static <T> void put(AttributesBuilder b, AttributeKey<T> key, Object value) {
    b.put(key, (T) value);
  }

  /** The span as exported: itself when nothing changes. */
  SpanData span(SpanData span) {
    Redacted attrs = apply(span.getAttributes());
    boolean changed = attrs.attributes() != span.getAttributes();
    List<EventData> events = new ArrayList<>(span.getEvents().size());
    for (EventData e : span.getEvents()) {
      Redacted r = apply(e.getAttributes());
      if (r.attributes() == e.getAttributes()) {
        events.add(e);
        continue;
      }
      changed = true;
      int total = e.getTotalAttributeCount() - r.dropped();
      events.add(e instanceof ExceptionEventData x
          ? ExceptionEventData.create(x.getEpochNanos(), x.getException(), r.attributes(), total)
          : EventData.create(e.getEpochNanos(), e.getName(), r.attributes(), total));
    }
    List<LinkData> links = new ArrayList<>(span.getLinks().size());
    for (LinkData l : span.getLinks()) {
      Redacted r = apply(l.getAttributes());
      if (r.attributes() == l.getAttributes()) {
        links.add(l);
        continue;
      }
      changed = true;
      links.add(LinkData.create(l.getSpanContext(), r.attributes(), l.getTotalAttributeCount() - r.dropped()));
    }
    if (!changed) {
      return span;
    }
    int total = span.getTotalAttributeCount() - attrs.dropped();
    return new DelegatingSpanData(span) {
      @Override
      public Attributes getAttributes() {
        return attrs.attributes();
      }

      @Override
      public int getTotalAttributeCount() {
        return total;
      }

      @Override
      public List<EventData> getEvents() {
        return events;
      }

      @Override
      public List<LinkData> getLinks() {
        return links;
      }
    };
  }

  /** The log record as exported: itself when nothing changes. */
  LogRecordData log(LogRecordData r) {
    Redacted attrs = apply(r.getAttributes());
    return attrs.attributes() == r.getAttributes() ? r : new RedactedLogRecord(r, attrs);
  }

  /** A log record with other attributes; every other method is the original's. */
  private static final class RedactedLogRecord implements LogRecordData {
    private final LogRecordData r;
    private final Redacted attrs;

    RedactedLogRecord(LogRecordData r, Redacted attrs) {
      this.r = r;
      this.attrs = attrs;
    }

    @Override
    public Attributes getAttributes() {
      return attrs.attributes();
    }

    @Override
    public int getTotalAttributeCount() {
      return r.getTotalAttributeCount() - attrs.dropped();
    }

    @Override
    public Resource getResource() {
      return r.getResource();
    }

    @Override
    public InstrumentationScopeInfo getInstrumentationScopeInfo() {
      return r.getInstrumentationScopeInfo();
    }

    @Override
    public long getTimestampEpochNanos() {
      return r.getTimestampEpochNanos();
    }

    @Override
    public long getObservedTimestampEpochNanos() {
      return r.getObservedTimestampEpochNanos();
    }

    @Override
    public SpanContext getSpanContext() {
      return r.getSpanContext();
    }

    @Override
    public Severity getSeverity() {
      return r.getSeverity();
    }

    @Override
    public String getSeverityText() {
      return r.getSeverityText();
    }

    @Override
    @SuppressWarnings("deprecation")
    public Body getBody() {
      return r.getBody();
    }

    @Override
    public Value<?> getBodyValue() {
      return r.getBodyValue();
    }

    @Override
    public String getEventName() {
      return r.getEventName();
    }

    @Override
    public String toString() {
      return "Redacted{" + r + ", attributes=" + attrs.attributes() + "}";
    }
  }
}
