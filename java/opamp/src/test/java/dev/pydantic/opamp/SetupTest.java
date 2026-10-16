// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import org.junit.jupiter.api.Test;

/**
 * The core's Setup call: off without an endpoint; the endpoint, the headers and the heartbeat as
 * it reads them; the instance UID saved, or made and to be saved.
 */
class SetupTest {
  private static CoreProto.Variable variable(String name, String value) {
    return CoreProto.Variable.newBuilder().setName(name).setValue(value).build();
  }

  @Test
  void setupIsTheCoresCall() {
    assertTrue(Setup.start(CoreProto.SetupStart.getDefaultInstance()).getOff());
    CoreProto.SetupStarted s = Setup.start(CoreProto.SetupStart.newBuilder()
        .addEnv(variable("OTEL_OPAMP_ENDPOINT", "https://user:pw@opamp.example.com/v1/opamp?k=v"))
        .addEnv(variable("OTEL_OPAMP_HEADERS", "Authorization=Bearer%20k"))
        .addEnv(variable("OTEL_OPAMP_HEARTBEAT_INTERVAL", "30s"))
        .addEnv(variable("OTEL_OPAMP_STATE_DIR", "none"))
        .build());
    assertFalse(s.getOff());
    assertEquals("https://opamp.example.com/v1/opamp", s.getShownEndpoint());
    assertEquals(1, s.getHeadersCount());
    assertEquals("Bearer k", s.getHeaders(0).getValue());
    assertEquals(30_000, s.getHeartbeatIntervalMs());
    assertEquals("", s.getStateDir());
    assertEquals(0, s.getDefaultSlotsCount());

    String saved = "0199e3a0-0000-7000-8000-000000000007";
    ByteString entropy = ByteString.copyFrom(new byte[16]);
    CoreProto.SetupIdentified id = Setup.identify(CoreProto.SetupIdentity.newBuilder()
        .setStateDir(true).setSaved(saved + "\n").setEntropy(entropy).build());
    assertEquals(CoreProto.SetupUidFrom.SETUP_UID_FROM_SAVED, id.getFrom());
    assertFalse(id.getSave());
    assertEquals(saved, id.getText());
    CoreProto.SetupIdentified made = Setup.identify(CoreProto.SetupIdentity.newBuilder()
        .setStateDir(true).setEntropy(entropy).setUnixMs(1).build());
    assertEquals(CoreProto.SetupUidFrom.SETUP_UID_FROM_MADE, made.getFrom());
    assertTrue(made.getSave());
    assertEquals(16, made.getInstanceUid().size());
    // No entropy: the core refuses the call.
    assertThrows(Setup.Failed.class, () -> Setup.identify(CoreProto.SetupIdentity.getDefaultInstance()));
  }
}
