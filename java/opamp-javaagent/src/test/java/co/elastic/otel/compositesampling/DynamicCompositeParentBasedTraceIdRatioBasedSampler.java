// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package co.elastic.otel.compositesampling;

import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.opentelemetry.sdk.trace.samplers.SamplingResult;
import java.util.List;

/**
 * A stand-in, by name, for the default sampler of Elastic's distribution of the agent (EDOT Java,
 * 1.14), whose {@code getDescription} calls itself and never returns (OutsideTest).
 */
public enum DynamicCompositeParentBasedTraceIdRatioBasedSampler implements Sampler {
  INSTANCE;

  @Override
  public SamplingResult shouldSample(Context parentContext, String traceId, String name, SpanKind spanKind,
      Attributes attributes, List<LinkData> parentLinks) {
    return SamplingResult.recordAndSample();
  }

  @Override
  public String getDescription() {
    return INSTANCE.getDescription();
  }

  @Override
  public String toString() {
    return "DynamicCompositeParentBasedTraceIdRatioBasedSampler(ratio=1.0)";
  }
}
