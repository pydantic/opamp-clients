// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

/**
 * The settings' glob, as the SDK settings layer matches it (held to it by
 * the shared test vectors): {@code *} matches any run of
 * characters, none included; every other character is itself. By Unicode code points, so a
 * character past the BMP (two UTF-16 units) is one character, as in every other language.
 */
final class Glob {
  private final int[] pattern;
  private final boolean ignoreAsciiCase;

  private Glob(String pattern, boolean ignoreAsciiCase) {
    this.pattern = fold(pattern.codePoints().toArray(), ignoreAsciiCase);
    this.ignoreAsciiCase = ignoreAsciiCase;
  }

  /** A glob matched case-sensitively: span names, paths, attribute values. */
  static Glob of(String pattern) {
    return new Glob(pattern, false);
  }

  /** A glob matched ignoring ASCII case ({@code A} is {@code a}; {@code É} isn't {@code é}): redaction keys. */
  static Glob ignoringAsciiCase(String pattern) {
    return new Glob(pattern, true);
  }

  boolean matches(String text) {
    int[] t = fold(text.codePoints().toArray(), ignoreAsciiCase);
    int[] p = pattern;
    int pi = 0;
    int ti = 0;
    // The last `*` seen: the pattern after it, and the text it has taken up to (-1: none yet).
    int starAfter = -1;
    int starTaken = 0;
    while (ti < t.length) {
      if (pi < p.length && p[pi] == '*') {
        pi++;
        starAfter = pi;
        starTaken = ti;
      } else if (pi < p.length && p[pi] == t[ti]) {
        pi++;
        ti++;
      } else if (starAfter >= 0) {
        // The last `*` takes one more character, and the pattern after it starts again.
        starTaken++;
        pi = starAfter;
        ti = starTaken;
      } else {
        return false;
      }
    }
    while (pi < p.length && p[pi] == '*') {
      pi++;
    }
    return pi == p.length;
  }

  private static int[] fold(int[] cps, boolean ignoreAsciiCase) {
    if (ignoreAsciiCase) {
      for (int i = 0; i < cps.length; i++) {
        if (cps[i] >= 'A' && cps[i] <= 'Z') {
          cps[i] += 'a' - 'A';
        }
      }
    }
    return cps;
  }

  @Override
  public String toString() {
    return new String(pattern, 0, pattern.length);
  }
}
