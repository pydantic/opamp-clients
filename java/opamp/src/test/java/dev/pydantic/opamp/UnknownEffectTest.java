// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.core.CoreProto;
import java.net.http.HttpClient;
import org.junit.jupiter.api.Test;

class UnknownEffectTest {
  @Test
  void anEffectThisClientDoesntKnowIsAnError() {
    Executor x = new Executor(Transport.of(HttpClient.newHttpClient()), input -> {}, null);
    IllegalStateException e =
        assertThrows(IllegalStateException.class, () -> x.run(CoreProto.Effect.getDefaultInstance()));
    assertTrue(e.getMessage().contains("older than its core"));
  }

  @Test
  void aClientThatFollowsRedirectsIsRefused() {
    HttpClient follows = HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NORMAL).build();
    assertThrows(IllegalArgumentException.class,
        () -> OpampClient.Config.builder("http://x", new byte[16]).httpClient(follows));
    OpampClient.Config.builder("http://x", new byte[16]).httpClient(HttpClient.newHttpClient());
  }
}
