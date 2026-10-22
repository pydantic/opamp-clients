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
 * {@code drop_attributes} and {@code mask_attributes}, then scrubbing ({@link Scrubbing}), at
 * export: an attribute whose key a drop pattern matches (ignoring ASCII case) is removed, one a
 * mask pattern matches has the string {@code [REDACTED]} for its value, whatever its type; a key
 * both match is dropped; every other one is scrubbed. Span, span event, span link and log record
 * attributes, and log bodies (scrubbing only); never resource or scope attributes, or names.
 *
 * <p>The application's span or record is never changed: other processors and exporters share it.
 * A redacted one is a new object over it, made only when an attribute changes. A drop leaves
 * {@code dropped_attributes_count} as it was: the total attribute count goes down with it.
 */
final class Redaction {
  static final String MASK = "[REDACTED]";
  static final Redaction NONE = new Redaction(List.of(), List.of(), Scrubbing.NONE);

  private final List<Glob> drop;
  private final List<Glob> mask;
  private final Scrubbing scrub;

  private Redaction(List<Glob> drop, List<Glob> mask, Scrubbing scrub) {
    this.drop = drop;
    this.mask = mask;
    this.scrub = scrub;
  }

  /** The settings' redaction and scrubbing; {@link #NONE} without either. */
  static Redaction of(SettingsProto.Settings s) {
    Scrubbing scrub = Scrubbing.of(s);
    if (s.getDropAttributesCount() == 0 && s.getMaskAttributesCount() == 0 && !scrub.any()) {
      return NONE;
    }
    return new Redaction(s.getDropAttributesList().stream().map(Glob::ignoringAsciiCase).toList(),
        s.getMaskAttributesList().stream().map(Glob::ignoringAsciiCase).toList(), scrub);
  }

  boolean any() {
    return !drop.isEmpty() || !mask.isEmpty() || scrub.any();
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
      } else if (scrub(b, key, value)) {
        changed[0] = true;
      } else {
        put(b, key, value);
      }
    });
    return changed[0] ? new Redacted(b.build(), dropped[0]) : new Redacted(attributes, 0);
  }

  /**
   * Puts the attribute as scrubbing leaves it, if that changes it: its key's string in place of
   * its value, whatever its type; a string scrubbed; a string array's items each; a {@link Value}
   * as a body is. False, putting nothing, when it stays as it is.
   */
  @SuppressWarnings("unchecked")
  private boolean scrub(AttributesBuilder b, AttributeKey<?> key, Object value) {
    if (!scrub.any()) {
      return false;
    }
    String name = key.getKey();
    String what = scrub.key(name);
    if (what == Scrubbing.KEEP) {
      return false;
    }
    if (!what.isEmpty()) {
      b.put(AttributeKey.stringKey(name), what);
      return true;
    }
    switch (key.getType()) {
      case STRING -> {
        String s = scrub.text((String) value);
        if (s == null) {
          return false;
        }
        b.put(AttributeKey.stringKey(name), s);
        return true;
      }
      case STRING_ARRAY -> {
        List<String> items = (List<String>) value;
        List<String> out = null;
        for (int i = 0; i < items.size(); i++) {
          String s = items.get(i) == null ? null : scrub.text(items.get(i));
          if (s == null) {
            continue;
          }
          if (out == null) {
            out = new ArrayList<>(items);
          }
          out.set(i, s);
        }
        if (out == null) {
          return false;
        }
        b.put(AttributeKey.stringArrayKey(name), out);
        return true;
      }
      case VALUE -> {
        Value<?> v = (Value<?>) value;
        Value<?> s = scrub.value(null, v);
        if (s == v) {
          return false;
        }
        b.put(AttributeKey.valueKey(name), s);
        return true;
      }
      default -> {
        return false;
      }
    }
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
    Value<?> body = r.getBodyValue();
    Value<?> scrubbed = scrub.any() ? scrub.value(null, body) : body;
    return attrs.attributes() == r.getAttributes() && scrubbed == body ? r : new RedactedLogRecord(r, attrs, scrubbed);
  }

  /** A log record with other attributes and body; every other method is the original's. */
  private static final class RedactedLogRecord implements LogRecordData {
    private final LogRecordData r;
    private final Redacted attrs;
    private final Value<?> body;

    RedactedLogRecord(LogRecordData r, Redacted attrs, Value<?> body) {
      this.r = r;
      this.attrs = attrs;
      this.body = body;
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
      if (body == r.getBodyValue()) {
        return r.getBody();
      }
      return body == null ? Body.empty() : Body.string(body.asString());
    }

    @Override
    public Value<?> getBodyValue() {
      return body;
    }

    @Override
    public String getEventName() {
      return r.getEventName();
    }

    @Override
    public String toString() {
      return "Redacted{" + r + ", attributes=" + attrs.attributes() + ", body=" + body + "}";
    }
  }
}
