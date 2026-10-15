// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.javaagent.extension.AgentListener;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import java.util.logging.Logger;

/**
 * Starts the OpAMP client once the agent has built the SDK, or says why it can't: an endpoint is
 * configured but the agent ran none of the extension's hooks.
 */
public final class OpampAgentListener implements AgentListener {
  @Override
  public void afterAgent(AutoConfiguredOpenTelemetrySdk sdk) {
    Extension e = Extension.INSTANCE;
    String why = e.notHooked(Declarative.config());
    if (why != null) {
      Logger.getLogger(Extension.class.getName()).warning(why);
      return;
    }
    e.start(e.seesResource() ? null : Declarative.resource(sdk));
  }
}
