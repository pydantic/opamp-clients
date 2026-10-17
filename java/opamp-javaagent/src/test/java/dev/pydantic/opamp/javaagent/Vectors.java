// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.google.protobuf.ByteString;
import dev.pydantic.opamp.sdk.SdkSettings;
import dev.pydantic.opamp.sdk.SettingsProto;
import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import opamp.proto.v1.Opamp;

/**
 * The SDK settings layer's shared test vectors (testdata/sdk-settings), which every
 * language's controls are held to, and the layer itself, to read each case's policy into typed
 * settings as a decision hands them over. Also the policies the tests send.
 */
final class Vectors {
  static final ObjectMapper JSON = new ObjectMapper();
  static final Path DIR = Path.of("../../testdata/sdk-settings");
  static final SdkSettings LAYER = new SdkSettings();
  /**
   * Every setting declared, each live one at once and each next-start one at the next start: the
   * vectors cover them all. The agent declares as much ({@link Extension#HOST}, checked in
   * ExtensionTest).
   */
  static final SettingsProto.Host ALL = SettingsProto.Host.newBuilder().addAllLive(List.of("debug_sampling_rate",
      "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes", "sampling_rate", "sampling_rules",
      "sampling_traces_per_second", "send_logs", "send_logs_level", "send_logs_level_by_scope",
      "send_logs_per_second", "send_metrics", "send_traces"))
      .addAllNextStart(List.of("deactivate_all_instrumentations", "deactivate_instrumentations")).build();
  /** 2026-10-14T12:00:00Z: the vectors' clock where a case gives none. */
  static final long NOW = 1_791_979_200_000L;

  private Vectors() {}

  /** The cases of {@code testdata/<name>.json}. */
  static List<JsonNode> cases(String name) {
    try {
      List<JsonNode> out = new ArrayList<>();
      // OPAMP_SETTINGS_VECTORS: a directory of random cases, in place of testdata's where it has the file.
      Path file = DIR.resolve(name + ".json");
      String other = System.getenv("OPAMP_SETTINGS_VECTORS");
      if (other != null && !other.isEmpty() && Files.exists(Path.of(other, name + ".json"))) {
        file = Path.of(other, name + ".json");
      }
      JSON.readTree(file.toFile()).get("cases").forEach(out::add);
      return out;
    } catch (IOException e) {
      throw new UncheckedIOException(e);
    }
  }

  /** The remote config's only file. */
  static final String POLICY_FILE = "logfire-sdk-policy.json";

  /** A remote config of one policy, as the server sends it. */
  static Opamp.AgentRemoteConfig config(String document) {
    return Opamp.AgentRemoteConfig.newBuilder().setConfig(Opamp.AgentConfigMap.newBuilder()
        .putConfigMap(POLICY_FILE, Opamp.AgentConfigObject.newBuilder().setContentType("application/json")
            .setBody(ByteString.copyFromUtf8(document)).build()))
        .setConfigHash(ByteString.copyFromUtf8(document)).build();
  }

  /** The settings a policy puts in force, every setting declared, at {@code nowUnixMs}. */
  static SettingsProto.Settings settings(String document, long nowUnixMs) {
    SettingsProto.Decision d = LAYER.decide(config(document), ByteString.EMPTY, ByteString.EMPTY, ALL,
        nowUnixMs, null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.getStatus().getStatus(),
        document + ": " + d.getStatus().getErrorMessage());
    return d.getApply();
  }

  /** A case's settings, at its clock. */
  static SettingsProto.Settings settings(JsonNode c) {
    return settings(c.get("settings").toString(), c.has("now_unix_ms") ? c.get("now_unix_ms").asLong() : NOW);
  }

  /** The controls of a policy in order: it must send the first two, and may leave out the rest (as local). */
  static final List<String> CONTROLS = List.of("head_sampling", "signal_exports", "sampling_rules",
      "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug", "app_settings",
      "instrumentations");

  /** A policy of the first two controls: {@code set} holds pairs of a control's name and its JSON value; the rest local. */
  static String v1(String... set) {
    return policy("logfire-sdk-policy/v1", 2, set);
  }

  /** A policy that sends every control: {@code set} holds pairs of a control's name and its JSON value; the rest local. */
  static String every(String... set) {
    return policy("logfire-sdk-policy/v1", CONTROLS.size(), set);
  }

  /** A control's override with the given fields. */
  static String override(String fields) {
    return "{\"mode\":\"override\"," + fields + "}";
  }

  /** A signal_exports control: each of traces, metrics and logs is "" (local), "on" or "off". */
  static String exports(String traces, String metrics, String logs) {
    return "{\"traces\":" + signal(traces) + ",\"metrics\":" + signal(metrics) + ",\"logs\":" + signal(logs) + "}";
  }

  private static String signal(String s) {
    return switch (s) {
      case "on" -> override("\"enabled\":true");
      case "off" -> override("\"enabled\":false");
      default -> "{\"mode\":\"local\"}";
    };
  }

  private static String policy(String schema, int n, String... set) {
    java.util.Map<String, String> given = new java.util.HashMap<>();
    for (int i = 0; i + 1 < set.length; i += 2) {
      if (!CONTROLS.subList(0, n).contains(set[i])) {
        throw new IllegalArgumentException("no control " + set[i] + " in the first " + n);
      }
      given.put(set[i], set[i + 1]);
    }
    StringBuilder b = new StringBuilder("{\"schema\":\"" + schema + "\"");
    for (String c : CONTROLS.subList(0, n)) {
      String v = given.getOrDefault(c, c.equals("signal_exports") ? exports("", "", "") : "{\"mode\":\"local\"}");
      b.append(",\"").append(c).append("\":").append(v);
    }
    return b.append('}').toString();
  }

  private static final ObjectMapper SORTED = new ObjectMapper()
      .configure(com.fasterxml.jackson.databind.SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS, true);

  /** A policy as the layer writes an effective config: compact, keys sorted. */
  static String sorted(String doc) {
    try {
      return SORTED.writeValueAsString(SORTED.readValue(doc, Object.class));
    } catch (IOException e) {
      throw new UncheckedIOException(e);
    }
  }

  /** A policy as the layer reports it in force: every control, those it leaves out local, keys sorted. */
  static String full(String doc) {
    try {
      @SuppressWarnings("unchecked")
      java.util.Map<String, Object> m = SORTED.readValue(doc, java.util.Map.class);
      for (String c : CONTROLS) {
        m.putIfAbsent(c, java.util.Map.of("mode", "local"));
      }
      return SORTED.writeValueAsString(m);
    } catch (IOException e) {
      throw new UncheckedIOException(e);
    }
  }

  /** The policy an effective config reports: the test fails if it holds another file. */
  static String effective(Opamp.EffectiveConfig e) {
    var files = e.getConfigMap().getConfigMapMap();
    assertEquals(List.of(POLICY_FILE), List.copyOf(files.keySet()));
    assertEquals("application/json", files.get(POLICY_FILE).getContentType());
    return files.get(POLICY_FILE).getBody().toStringUtf8();
  }
}
