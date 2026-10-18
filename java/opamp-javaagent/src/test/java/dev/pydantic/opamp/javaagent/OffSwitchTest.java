// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import java.io.IOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.logging.Level;
import java.util.stream.Stream;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

/**
 * The off switch (tests/shell-tables/off.json): every case through the agent's properties, each
 * variable as its property and the code's endpoint as otel.opamp.endpoint (the agent has no code
 * of its own: a system property is the nearest). Off: no client starts and the reason is said
 * once, at WARNING; on: the client starts and the server hears from it. Every endpoint of a case
 * is the test server's.
 */
class OffSwitchTest {
  static Stream<Arguments> cases() throws IOException {
    List<Arguments> out = new ArrayList<>();
    Vectors.JSON.readTree(Path.of("../../tests/shell-tables/off.json").toFile()).get("cases")
        .forEach(c -> out.add(Arguments.of(c.get("id").asText(), c)));
    assertEquals(16, out.size());
    return out.stream();
  }

  @ParameterizedTest(name = "{0}")
  @MethodSource("cases")
  void theOffSwitchTable(String id, JsonNode c) throws Exception {
    try (InventoryTest.Server server = new InventoryTest.Server()) {
      Map<String, String> props = new HashMap<>(Map.of("otel.opamp.state.dir", "none",
          "otel.traces.exporter", "none", "otel.metrics.exporter", "none", "otel.logs.exporter", "none"));
      c.get("env").fields().forEachRemaining(v ->
          props.put(v.getKey().toLowerCase(Locale.ROOT).replace('_', '.'), local(v.getValue().asText(), server)));
      if (!c.get("code_endpoint").isNull()) {
        props.put("otel.opamp.endpoint", local(c.get("code_endpoint").asText(), server));
      }
      boolean off = c.get("off").asBoolean();
      Extension e = Extension.forTest(null);
      List<String> said = ExtensionTest.logged(Level.WARNING, () -> {
        try (InventoryTest.Sdk sdk = InventoryTest.sdk(e, props)) {
          // What the agent listener does once the agent has built the SDK: start the client.
          e.built(sdk.built().getOpenTelemetrySdk());
          e.start(null);
          assertEquals(!off, e.on(), id);
          if (!off) {
            try {
              server.next(m -> true);
              e.stopForTest();
            } catch (Exception x) {
              throw new AssertionError(x);
            }
          }
        }
      });
      if (off) {
        assertTrue(server.got.isEmpty(), id + ": off, but the server was polled");
      }
      String reason = c.get("reason").asText();
      List<String> offLines = said.stream().filter(l -> l.contains("OpAMP: off")).toList();
      assertEquals(reason.isEmpty() ? List.of() : List.of("WARNING OpAMP: " + reason), offLines, id);
    }
  }

  private static String local(String v, InventoryTest.Server server) {
    return v.startsWith("http://") || v.startsWith("https://") ? server.endpoint() : v;
  }
}
