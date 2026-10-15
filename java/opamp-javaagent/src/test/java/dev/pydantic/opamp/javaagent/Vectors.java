// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

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
 * language's controls are held to, and the layer itself, to read each case's settings document
 * into typed settings as a decision hands them over.
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
      .addAllNextStart(List.of("deactivate_all_instrumentations", "deactivate_instrumentations",
          "opamp_polling_interval")).build();
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

  /** A remote config of one settings document, as the server sends it. */
  static Opamp.AgentRemoteConfig config(String document) {
    return Opamp.AgentRemoteConfig.newBuilder().setConfig(Opamp.AgentConfigMap.newBuilder()
        .putConfigMap("elastic", Opamp.AgentConfigObject.newBuilder().setContentType("application/json")
            .setBody(ByteString.copyFromUtf8(document)).build()))
        .setConfigHash(ByteString.copyFromUtf8(document)).build();
  }

  /** The settings a document puts in force, every setting declared, at {@code nowUnixMs}. */
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
}
