// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.opentelemetry.sdk.trace.samplers.SamplingDecision;
import io.opentelemetry.sdk.trace.samplers.SamplingResult;
import java.util.List;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * The agent's sampler, until the server says otherwise. For a new trace (a span with no parent):
 * the first of {@code sampling_rules} its root span matches decides the rate, else
 * {@code sampling_rate}, else the agent's own sampler; a span with a parent follows it. The rate is
 * applied by the SDK's trace-ID ratio sampler. Then {@code sampling_traces_per_second}: a sampled
 * root, or a span whose remote parent was sampled, takes a token, and without one is dropped; a
 * child of a span sampled here takes none, so a trace is never cut in two.
 *
 * <p>With nothing set, the agent's own sampler decides every span, children too, as before.
 */
final class LiveSampler implements Sampler {
  private static final AttributeKey<String> URL_PATH = AttributeKey.stringKey("url.path");
  private static final AttributeKey<String> HTTP_TARGET = AttributeKey.stringKey("http.target");

  /** A sampling rule, its globs and ratio sampler made once. */
  record Rule(double rate, Glob name, SpanKind kind, Glob path, List<Map.Entry<String, Glob>> attributes,
      Sampler ratio) {
    static Rule of(SettingsProto.SamplingRule r) {
      return new Rule(r.getRate(), r.hasName() ? Glob.of(r.getName()) : null, r.hasKind() ? LiveSampler.kind(r.getKind()) : null,
          r.hasPath() ? Glob.of(r.getPath()) : null,
          r.getAttributesMap().entrySet().stream().map(e -> Map.entry(e.getKey(), Glob.of(e.getValue()))).toList(),
          Sampler.traceIdRatioBased(r.getRate()));
    }

    /** Whether a span starting so matches every matcher the rule has (none: every span). */
    boolean matches(String spanName, SpanKind spanKind, Attributes attrs) {
      if (name != null && !name.matches(spanName)) {
        return false;
      }
      if (kind != null && kind != spanKind) {
        return false;
      }
      if (path != null) {
        String p = LiveSampler.path(attrs);
        if (p == null || !path.matches(p)) {
          return false;
        }
      }
      for (Map.Entry<String, Glob> a : attributes) {
        String text = text(attrs, a.getKey());
        if (text == null || !a.getValue().matches(text)) {
          return false;
        }
      }
      return true;
    }
  }

  static SpanKind kind(SettingsProto.SpanKind k) {
    return switch (k) {
      case SPAN_KIND_SERVER -> SpanKind.SERVER;
      case SPAN_KIND_CLIENT -> SpanKind.CLIENT;
      case SPAN_KIND_PRODUCER -> SpanKind.PRODUCER;
      case SPAN_KIND_CONSUMER -> SpanKind.CONSUMER;
      default -> SpanKind.INTERNAL;
    };
  }

  /** The path a rule's {@code path} matches: {@code url.path}, else {@code http.target} up to its {@code ?}. */
  static String path(Attributes attrs) {
    String p = attrs.get(URL_PATH);
    if (p != null) {
      return p;
    }
    String target = attrs.get(HTTP_TARGET);
    if (target == null) {
      return null;
    }
    int q = target.indexOf('?');
    return q < 0 ? target : target.substring(0, q);
  }

  /**
   * The text a rule's attribute glob matches: a string's own, an integer's decimal, true or false.
   * A double or an array has none (languages print floats differently), nor has a missing one.
   */
  static String text(Attributes attrs, String key) {
    String s = attrs.get(AttributeKey.stringKey(key));
    if (s != null) {
      return s;
    }
    Long n = attrs.get(AttributeKey.longKey(key));
    if (n != null) {
      return Long.toString(n);
    }
    Boolean b = attrs.get(AttributeKey.booleanKey(key));
    return b == null ? null : b.toString();
  }

  /** How a span is sampled: as the layer's reference sampler says (testdata/sampling.json). */
  enum How { FOLLOW_PARENT, RATE, APP_SAMPLER }

  /** {@code rule}: the index of the rule that decided ({@code RATE}), or null for sampling_rate. */
  record Decision(How how, boolean parentSampled, double rate, Integer rule) {}

  /** What decides, swapped whole. {@code rated}: sampling_rate's ratio sampler, or null. */
  private record State(Sampler configured, Double rate, Sampler rated, List<Rule> rules, TokenBucket cap) {}

  private final LongSupplier nanoTime;
  private volatile State state = new State(Sampler.parentBased(Sampler.alwaysOn()), null, null, List.of(), null);

  /** {@code nanoTime}: the monotonic clock the trace cap refills by. */
  LiveSampler(LongSupplier nanoTime) {
    this.nanoTime = nanoTime;
  }

  /**
   * The sampler the agent's configuration built: in force while the server sets no rate and no
   * rules. Settings put in force before (from saved settings, at start) still hold.
   */
  synchronized Sampler wrap(Sampler agents) {
    State s = state;
    state = new State(agents, s.rate(), s.rated(), s.rules(), s.cap());
    return this;
  }

  /** Carry out the sampling settings: rate, rules and the trace cap; each left out is none. */
  synchronized void apply(SettingsProto.Settings s) {
    State now = state;
    Double rate = s.hasSamplingRate() ? s.getSamplingRate() : null;
    state = new State(now.configured(), rate, rate == null ? null : Sampler.traceIdRatioBased(rate),
        s.getSamplingRulesList().stream().map(Rule::of).toList(),
        TokenBucket.of(s.hasSamplingTracesPerSecond() ? s.getSamplingTracesPerSecond() : null, now.cap()));
  }

  /** The agent's own sampler, or with a rate and no rules the parent-based ratio sampler in its place. */
  Sampler current() {
    State s = state;
    if (s.rate() == null && s.rules().isEmpty()) {
      return s.configured();
    }
    return s.rules().isEmpty() ? Sampler.parentBased(s.rated()) : this;
  }

  /** The trace cap's bucket; null: none. */
  TokenBucket cap() {
    return state.cap();
  }

  /** How a span starting so is sampled, before the trace cap. */
  Decision decide(SpanContext parent, String name, SpanKind kind, Attributes attributes) {
    return decide(state, parent, name, kind, attributes);
  }

  private static Decision decide(State s, SpanContext parent, String name, SpanKind kind, Attributes attributes) {
    if (s.rules().isEmpty() && s.rate() == null) {
      return new Decision(How.APP_SAMPLER, false, 0, null);
    }
    if (parent.isValid()) {
      return new Decision(How.FOLLOW_PARENT, parent.isSampled(), 0, null);
    }
    List<Rule> rules = s.rules();
    for (int i = 0; i < rules.size(); i++) {
      if (rules.get(i).matches(name, kind, attributes)) {
        return new Decision(How.RATE, false, rules.get(i).rate(), i);
      }
    }
    return s.rate() == null ? new Decision(How.APP_SAMPLER, false, 0, null)
        : new Decision(How.RATE, false, s.rate(), null);
  }

  @Override
  public SamplingResult shouldSample(Context parentContext, String traceId, String name, SpanKind kind,
      Attributes attributes, List<LinkData> links) {
    State s = state;
    SpanContext parent = Span.fromContext(parentContext).getSpanContext();
    Decision d = decide(s, parent, name, kind, attributes);
    SamplingResult result = switch (d.how()) {
      case APP_SAMPLER -> s.configured().shouldSample(parentContext, traceId, name, kind, attributes, links);
      case FOLLOW_PARENT -> d.parentSampled() ? SamplingResult.recordAndSample() : SamplingResult.drop();
      case RATE -> (d.rule() == null ? s.rated() : s.rules().get(d.rule()).ratio())
          .shouldSample(parentContext, traceId, name, kind, attributes, links);
    };
    TokenBucket cap = s.cap();
    if (cap != null && result.getDecision() == SamplingDecision.RECORD_AND_SAMPLE
        && (!parent.isValid() || (parent.isRemote() && parent.isSampled()))
        && cap.take(nanoTime.getAsLong(), 1) == 0) {
      return SamplingResult.drop();
    }
    return result;
  }

  @Override
  public String getDescription() {
    State s = state;
    String inner = s.rules().isEmpty() && s.rate() == null ? s.configured().getDescription()
        : (s.rules().isEmpty() ? "" : "rules=" + s.rules().size() + ",")
            + (s.rate() == null ? s.configured().getDescription() : s.rated().getDescription());
    return "OpampLiveSampler{" + inner + (s.cap() == null ? "" : "," + s.cap()) + "}";
  }

  @Override
  public String toString() {
    return getDescription();
  }
}
