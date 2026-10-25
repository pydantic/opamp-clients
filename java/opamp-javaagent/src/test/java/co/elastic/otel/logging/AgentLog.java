// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package co.elastic.otel.logging;

import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.Collection;

/**
 * A stand-in, by name, for the class of Elastic's distribution of the agent (EDOT Java) whose span
 * logger the extension leaves alone while nothing can switch it on (OutsideTest).
 */
public final class AgentLog {
  private AgentLog() {}

  /** EDOT's span logger: writes spans to the agent's log once debug logging is on. */
  public static final class DebugLogSpanExporter implements SpanExporter {
    @Override
    public CompletableResultCode export(Collection<SpanData> spans) {
      return CompletableResultCode.ofSuccess();
    }

    @Override
    public CompletableResultCode flush() {
      return CompletableResultCode.ofSuccess();
    }

    @Override
    public CompletableResultCode shutdown() {
      return CompletableResultCode.ofSuccess();
    }
  }
}
