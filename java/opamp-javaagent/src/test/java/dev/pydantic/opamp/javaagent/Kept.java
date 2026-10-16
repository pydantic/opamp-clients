// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import com.google.protobuf.ByteString;
import com.google.protobuf.CodedOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.HexFormat;
import opamp.proto.v1.Opamp;

/**
 * The client's saved state (opamp-state) holding a config applied, as the client leaves it once
 * the server has heard APPLIED: what the next start's settings come from. A test without a client
 * writes it here.
 *
 * <p>The bytes are a state the client saved before it kept the config applied (the shared vectors'
 * testdata/sdk-settings/start.json), with the config applied added: its field 16, a message, which
 * protobuf takes from the end of the encoding.
 */
final class Kept {
  private static final String BEFORE =
      "a core state saved before the core kept the config applied: the document kept before carries over";

  private Kept() {}

  /** A client state whose config applied has {@code document} as its settings entry. */
  static byte[] coreState(String document) {
    String base = null;
    for (var c : Vectors.cases("start")) {
      if (BEFORE.equals(c.get("name").asText())) {
        base = c.get("core_state").asText();
      }
    }
    if (base == null) {
      throw new IllegalStateException("start.json has no case " + BEFORE);
    }
    Opamp.AgentRemoteConfig applied = Opamp.AgentRemoteConfig.newBuilder()
        .setConfigHash(ByteString.copyFromUtf8("applied"))
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("elastic", Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFrom(document, StandardCharsets.UTF_8)).setContentType("application/json").build()))
        .build();
    try {
      ByteArrayOutputStream out = new ByteArrayOutputStream();
      out.write(HexFormat.of().parseHex(base));
      CodedOutputStream coded = CodedOutputStream.newInstance(out);
      coded.writeBytes(16, applied.toByteString());
      coded.flush();
      return out.toByteArray();
    } catch (IOException e) {
      throw new UncheckedIOException(e);
    }
  }

  /** Leave in {@code dir} what the client would after a config with {@code document} was applied. */
  static void keepApplied(Path dir, String document) {
    try {
      Files.write(dir.resolve("opamp-state"), coreState(document));
    } catch (IOException e) {
      throw new UncheckedIOException(e);
    }
  }
}
