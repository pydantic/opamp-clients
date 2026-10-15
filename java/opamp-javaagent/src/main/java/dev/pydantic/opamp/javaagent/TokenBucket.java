// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

/**
 * A rate cap's token bucket, the algorithm every language runs (the layer's {@code Bucket},
 * pinned by testdata/sdk-settings/caps.json): full at its first take; it refills at
 * {@code perSecond} tokens a second on a monotonic clock, up to {@code max(perSecond, 1)} (a
 * second's burst, at least one); a take of n gets the whole tokens there are, up to n. In
 * doubles, in this order: {@code tokens = min(capacity, tokens + elapsed_s * perSecond)}, then
 * {@code taken = min(n, floor(tokens))} and {@code tokens -= taken}.
 */
final class TokenBucket {
  final double perSecond;
  private double tokens;
  private long lastNs;
  private boolean started;

  /** A full bucket of {@code perSecond}. */
  TokenBucket(double perSecond) {
    this.perSecond = perSecond;
    this.tokens = Math.max(perSecond, 1.0);
  }

  /**
   * Take up to {@code n} tokens at {@code nowNs} ({@link System#nanoTime}); how many were taken. A
   * clock that goes back refills nothing.
   */
  synchronized long take(long nowNs, long n) {
    long last = started ? lastNs : nowNs;
    long elapsedNs = Math.max(0, nowNs - last);
    tokens = Math.min(tokens + elapsedNs / 1e9 * perSecond, Math.max(perSecond, 1.0));
    lastNs = nowNs - last > 0 ? nowNs : last;
    started = true;
    double taken = Math.min((double) n, Math.floor(tokens));
    tokens -= taken;
    return (long) taken;
  }

  /** The bucket for a cap of {@code perSecond} (null: none), kept when the cap is the same. */
  static TokenBucket of(Double perSecond, TokenBucket now) {
    if (perSecond == null) {
      return null;
    }
    return now != null && now.perSecond == perSecond ? now : new TokenBucket(perSecond);
  }

  @Override
  public String toString() {
    return "TokenBucket{" + perSecond + "/s}";
  }
}
