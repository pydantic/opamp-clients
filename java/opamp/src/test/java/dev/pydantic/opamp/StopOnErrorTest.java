// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;

import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;

class StopOnErrorTest {
  @Test
  void aCallbackThatThrowsStopsTheClientWhichSaysSoOnce() throws Exception {
    RuntimeException bug = new IllegalStateException("the application's bug");
    List<Notification> notes = new CopyOnWriteArrayList<>();
    CompletableFuture<Notification.Stopped> stopped = new CompletableFuture<>();
    OpampClient.Config config = OpampClient.Config.builder("http://127.0.0.1:1", new byte[16]).mode(OpampClient.Mode.JAVA).build();
    OpampClient client = OpampClient.start(config, OpampClient.Status.of(null), n -> {
      notes.add(n);
      if (n instanceof Notification.Stopped s) {
        stopped.complete(s);
      }
      // The refused connection is told, and the callback throws; Stopped is thrown at too.
      throw bug;
    });
    // The application hears without calling stop().
    assertSame(bug, stopped.get(10, TimeUnit.SECONDS).error());
    assertEquals(1, notes.stream().filter(n -> n instanceof Notification.Stopped).count());
    ExecutionException e = assertThrows(ExecutionException.class, () -> client.stop().get(10, TimeUnit.SECONDS));
    assertSame(bug, e.getCause());
  }
}
