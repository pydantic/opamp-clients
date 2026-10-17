// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.GeneratedMessageLite;
import dev.pydantic.opamp.core.CoreProto;
import dev.pydantic.opamp.sdk.SettingsProto;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * The messages are protobuf's lite code, on its lite runtime: the full runtime's descriptors and
 * reflection, which nothing here uses, were most of the classes an application loaded for the
 * client (in the OpenTelemetry demo's Java apps, metaspace they had to make room for).
 */
class ProtobufLiteTest {
  @Test
  void theMessagesAreLite() {
    for (Class<?> c :
        new Class<?>[] {
          Opamp.AgentToServer.class,
          Opamp.ServerToAgent.class,
          Anyvalue.AnyValue.class,
          CoreProto.Input.class,
          SettingsProto.Settings.class
        }) {
      assertTrue(GeneratedMessageLite.class.isAssignableFrom(c), c.getName());
    }
  }

  @Test
  void theFullRuntimeIsNotADependency() {
    assertThrows(ClassNotFoundException.class, () -> Class.forName("com.google.protobuf.Descriptors"));
  }
}
