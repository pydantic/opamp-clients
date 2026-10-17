// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.javaagent.extension.AgentListener;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import java.util.logging.Logger;

/**
 * Starts the OpAMP client once the agent has built the SDK, or says why it can't: an endpoint is
 * configured but the agent ran none of the extension's hooks. Before it starts, the SDK as built
 * says whether its sampler is the extension's ({@link Extension#built}).
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
    e.built(sdk.getOpenTelemetrySdk());
    e.start(e.seesResource() ? null : Declarative.resource(sdk));
  }
}
