// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.sdk;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.google.protobuf.ByteString;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import opamp.proto.v1.Opamp;

/** Logfire SDK policies for the tests: every control local unless a test gives it. */
public final class Policy {
  private Policy() {}

  /** The remote config's only file. */
  public static final String FILE = "logfire-sdk-policy.json";

  /** The controls of a policy in order: it must send the first two, and may leave out the rest (as local). */
  static final List<String> CONTROLS = List.of("head_sampling", "signal_exports", "sampling_rules",
      "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug", "app_settings",
      "instrumentations", "polling_interval");

  /** A policy of the first two controls: {@code set} holds pairs of a control's name and its JSON value. */
  public static String v1(String... set) {
    return policy("logfire-sdk-policy/v1", 2, set);
  }

  /** A policy that sends every control: {@code set} holds pairs of a control's name and its JSON value. */
  public static String every(String... set) {
    return policy("logfire-sdk-policy/v1", CONTROLS.size(), set);
  }

  /** A control's override with the given fields. */
  public static String override(String fields) {
    return "{\"mode\":\"override\"," + fields + "}";
  }

  /** A signal_exports control: each of traces, metrics and logs is "" (local), "on" or "off". */
  public static String exports(String traces, String metrics, String logs) {
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
    Map<String, String> given = new HashMap<>();
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
      .configure(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS, true);

  /** A policy as the layer writes an effective config: compact, keys sorted. */
  public static String sorted(String doc) {
    try {
      return SORTED.writeValueAsString(SORTED.readValue(doc, Object.class));
    } catch (JsonProcessingException e) {
      throw new IllegalArgumentException(e);
    }
  }

  /** A policy as the layer reports it in force: every control, those it leaves out local, keys sorted. */
  public static String full(String doc) {
    try {
      @SuppressWarnings("unchecked")
      Map<String, Object> m = SORTED.readValue(doc, Map.class);
      for (String c : CONTROLS) {
        m.putIfAbsent(c, Map.of("mode", "local"));
      }
      return SORTED.writeValueAsString(m);
    } catch (JsonProcessingException e) {
      throw new IllegalArgumentException(e);
    }
  }

  /** A remote config with the policy {@code body} as its only file, and the hash "h". */
  public static Opamp.AgentRemoteConfig config(String body) {
    return Opamp.AgentRemoteConfig.newBuilder().setConfigHash(ByteString.copyFromUtf8("h"))
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap(FILE, Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFromUtf8(body)).setContentType("application/json").build()))
        .build();
  }
}
