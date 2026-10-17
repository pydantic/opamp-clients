// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfigurationCustomizerProvider;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * {@link OpampCustomizerProvider}'s counterpart under declarative configuration
 * ({@code otel.config.file}): puts the live sampler and gated exporters around those the
 * configuration file names ({@link Declarative#wrap}). Last, so the controls wrap what every other
 * customizer configured. {@link First} runs before the agent reads the model. With no
 * {@code otel.opamp.endpoint} neither changes anything.
 */
public final class OpampDeclarativeCustomizerProvider implements DeclarativeConfigurationCustomizerProvider {
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());

  @Override
  public void customize(DeclarativeConfigurationCustomizer customizer) {
    Extension e = Extension.INSTANCE;
    customizer.addModelCustomizer(model -> {
      if (!e.on()) {
        return model;
      }
      try {
        e.notGated(Declarative.ungated(model));
        return Declarative.wrap(model, e.inventory);
      } catch (RuntimeException err) {
        // Better unmanaged than answering for controls that aren't there.
        e.off();
        LOG.log(Level.SEVERE, "OpAMP: can't put the controls into the configuration file's SDK; the agent runs unmanaged", err);
        return model;
      }
    });
  }

  @Override
  public int order() {
    return Integer.MAX_VALUE;
  }

  /**
   * First: load the saved settings, then put the instance UID in the resource and the next-start
   * settings where the agent reads which instrumentations run, before it reads them.
   */
  public static final class First implements DeclarativeConfigurationCustomizerProvider {
    @Override
    public void customize(DeclarativeConfigurationCustomizer customizer) {
      Extension e = Extension.INSTANCE;
      customizer.addModelCustomizer(model -> {
        try {
          return Declarative.setUp(e, Declarative.config(), model);
        } catch (RuntimeException err) {
          e.off();
          LOG.log(Level.SEVERE, "OpAMP: can't set up under the configuration file; the agent runs unmanaged", err);
          return model;
        }
      });
    }

    @Override
    public int order() {
      return Integer.MIN_VALUE;
    }
  }
}
