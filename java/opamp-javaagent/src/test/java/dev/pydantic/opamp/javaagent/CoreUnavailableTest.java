// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.api.trace.Span;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.logging.Level;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

/** A core that can't be loaded: the agent builds its SDK unmanaged, and says why once. */
class CoreUnavailableTest {
  private static final String WHY = "java.lang.NoClassDefFoundError: dev/pydantic/opamp/CoreModule";

  @ParameterizedTest
  @CsvSource({"core, true", "layer, true", "layer, false"})
  void aCoreThatCantBeLoadedIsSaidOnceAndTheSdkRunsUnmanaged(String which, boolean server, @TempDir Path dir) {
    Extension e = Extension.forTest(null);
    if (which.equals("core")) {
      e.coreUnavailable = () -> WHY;
    } else {
      e.layerUnavailable = () -> WHY;
    }
    Map<String, String> props = InventoryTest.props("http://127.0.0.1:1/v1/opamp", dir,
        "otel.traces.exporter", "none", "otel.metrics.exporter", "none", "otel.logs.exporter", "none");
    if (!server) {
      props.remove("otel.opamp.endpoint");
    }
    List<String> said = ExtensionTest.logged(Level.WARNING, () -> {
      for (int i = 0; i < 2; i++) { // set up again: not said again
        try (InventoryTest.Sdk sdk = InventoryTest.sdk(e, props)) {
          assertFalse(e.on(), "managed without a core");
          Span span = sdk.built().getOpenTelemetrySdk().getTracer("app").spanBuilder("work").startSpan();
          assertTrue(span.isRecording(), "the SDK records the application's span");
          span.end();
        }
      }
    });
    // Without a server the settings layer is never loaded: nothing to say.
    assertEquals(server
        ? List.of("WARNING OpAMP: the core can't be loaded on " + System.getProperty("os.name") + "-"
            + System.getProperty("os.arch") + "; the agent runs unmanaged: " + WHY)
        : List.of(), said);
  }
}
