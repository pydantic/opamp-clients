// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.sdk.common.CompletableResultCode;

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
