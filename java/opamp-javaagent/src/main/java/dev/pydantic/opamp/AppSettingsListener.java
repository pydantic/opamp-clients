// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.util.Map;
import java.util.Set;

/** Told of each change to the application's own settings: {@link AppSettings#onChange}. */
@FunctionalInterface
public interface AppSettingsListener {
  /**
   * The settings changed.
   *
   * @param settings every setting now in force, unmodifiable (what {@link AppSettings#snapshot()}
   *     returns now)
   * @param changed the keys added, changed or removed, sorted and unmodifiable; never empty
   */
  void changed(Map<String, String> settings, Set<String> changed);
}
