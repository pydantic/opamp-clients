// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.opamp.javaagent;

import dev.pydantic.logfire.opamp.sdk.SettingsProto;
import io.opentelemetry.sdk.common.CompletableResultCode;
import java.util.function.Supplier;

/**
 * What one signal's gate counted since start, for export health: exports that succeeded or failed
 * (after the exporter's own retries: what its result says), the items in failed ones, and the
 * items the gate dropped on purpose (a send_* switch, a level, a cap). Thread-safe.
 */
final class GateCounts {
  private long exportsOk;
  private long exportsFailed;
  private long itemsFailed;
  private long itemsFiltered;
  private boolean lastFailed;
  private String lastError = "";

  /** {@code n} items dropped on purpose. */
  synchronized void filtered(long n) {
    itemsFiltered += n;
  }

  /** Count an export of {@code items} items when its result comes; {@code result} is returned. */
  CompletableResultCode exported(CompletableResultCode result, long items) {
    result.whenComplete(() -> completed(result.isSuccess(), items, result.getFailureThrowable()));
    return result;
  }

  /**
   * Run a gate's export of {@code items} items: an error inside the gate (a value redaction can't
   * walk) fails the export, counted as failed, where the processor would drop the batch uncounted.
   */
  CompletableResultCode guarded(long items, Supplier<CompletableResultCode> export) {
    try {
      return export.get();
    } catch (RuntimeException e) {
      String why = "gate: " + (e.getMessage() != null ? e.getMessage() : e.getClass().getSimpleName());
      IllegalStateException failure = new IllegalStateException(why, e);
      completed(false, items, failure);
      return CompletableResultCode.ofExceptionalFailure(failure);
    }
  }

  synchronized void completed(boolean ok, long items, Throwable error) {
    if (ok) {
      exportsOk++;
      lastFailed = false;
      return;
    }
    exportsFailed++;
    itemsFailed += items;
    lastFailed = true;
    // The OTLP exporters often fail with no throwable (an HTTP status they logged): no text then.
    lastError = error == null ? "" : error.getMessage() != null ? error.getMessage() : error.getClass().getSimpleName();
  }

  synchronized SettingsProto.SignalCounts snapshot() {
    return SettingsProto.SignalCounts.newBuilder().setExportsOk(exportsOk).setExportsFailed(exportsFailed)
        .setItemsFailed(itemsFailed).setItemsFiltered(itemsFiltered).setLastFailed(lastFailed)
        .setLastError(lastError).build();
  }
}
