// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import java.io.IOException;
import java.nio.file.Path;

/** For the application side's tests: the jar the extension puts on the bootstrap class path. */
public final class AppSettingsSinkAccess {
  private AppSettingsSinkAccess() {}

  public static Path bridgeJar() throws IOException {
    return AppSettingsSink.bridgeJar();
  }
}
