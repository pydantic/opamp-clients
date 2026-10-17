// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.BufferedInputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.Socket;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The effects' ends: each kind of cancel aborts its effect at once and nothing
 * more about it reaches the core, and a connection closed by the core is reported closed when the
 * closing handshake is done. The executor is driven as the client drives it: {@link Executor#run}
 * for an effect, {@link Executor#admit} before an input would go to the core.
 */
class EffectsTest {
  /** An executor over the JDK's HTTP client, and what it reports. */
  static final class Rig {
    final LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    final Executor x;

    Rig(Path state) {
      x = new Executor(Transport.of(HttpClient.newHttpClient()), got::add, state);
    }

    void run(CoreProto.Effect.Builder e) {
      x.run(e.build());
    }

    void cancel(long id) {
      run(CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(id)));
    }

    /** The next input reported that goes to the core. */
    CoreProto.Input next() throws InterruptedException {
      for (long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(20); System.nanoTime() < end; ) {
        CoreProto.Input in = got.poll(100, TimeUnit.MILLISECONDS);
        if (in != null && x.admit(in)) {
          return in;
        }
      }
      throw new AssertionError("nothing came");
    }

    /** Nothing goes to the core for {@code ms}: whatever is reported is dropped. */
    void nothingFor(long ms) throws InterruptedException {
      for (long end = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms); System.nanoTime() < end; ) {
        CoreProto.Input in = got.poll(20, TimeUnit.MILLISECONDS);
        assertTrue(in == null || !x.admit(in), () -> "reported after its end: " + in);
      }
    }
  }

  static CoreProto.Effect.Builder wsOpen(long id, String url) {
    return CoreProto.Effect.newBuilder().setWsOpen(CoreProto.WsOpen.newBuilder()
        .setId(id).setUrl(url).setMaxMessageBytes(1 << 20).setTimeoutMs(30_000));
  }

  static CoreProto.Effect.Builder wsClose(long id) {
    return CoreProto.Effect.newBuilder().setWsClose(CoreProto.Id.newBuilder().setId(id));
  }

  // covers: RULE-CLIENT-15
  @Test
  void aCancelAbortsARequestInFlight(@TempDir Path dir) throws Exception {
    try (Raw server = new Raw(Raw::hold)) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(CoreProto.Effect.newBuilder().setHttpRequest(CoreProto.HttpRequest.newBuilder()
          .setId(1).setUrl(server.url("http") + "/v1/opamp").setBody(ByteString.copyFromUtf8("hi"))
          .setTimeoutMs(30_000).setMaxResponseBytes(1 << 20)));
      Socket s = server.held();
      r.cancel(1);
      assertTrue(Raw.closedWithin(s, 5000), "the request's connection is dropped");
      r.nothingFor(300);
    }
  }

  // covers: RULE-CLIENT-15
  @Test
  void aCancelAbortsAConnectionBeingOpened(@TempDir Path dir) throws Exception {
    try (Raw server = new Raw(Raw::hold)) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      Socket s = server.held();
      r.cancel(1);
      assertTrue(Raw.closedWithin(s, 5000), "the handshake is aborted");
      r.nothingFor(300);
    }
  }

  // A connection that opens after its cancel is closed at once, and never reported.
  // covers: RULE-CLIENT-15
  @Test
  void aConnectionThatOpensAfterItsCancelIsClosed(@TempDir Path dir) throws Exception {
    CountDownLatch answer = new CountDownLatch(1);
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head h = TransportTest.Head.read(in);
      Raw.HELD.add(s);
      answer.await();
      Raw.upgrade(s.getOutputStream(), h);
      Raw.hold(s, in);
    })) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      Socket s = server.held();
      r.cancel(1);
      answer.countDown();
      assertTrue(Raw.closedWithin(s, 5000), "closed when it opened");
      r.nothingFor(300);
    }
  }

  // covers: RULE-CLIENT-15
  @Test
  void aCancelDropsAnOpenConnection(@TempDir Path dir) throws Exception {
    try (Raw server = new Raw(Raw::websocket)) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, r.next().getKindCase());
      Socket s = server.held();
      r.cancel(1);
      assertTrue(Raw.closedWithin(s, 5000), "dropped at once");
      r.nothingFor(300);
    }
  }

  // The server never answers our Close: the core's cancel drops the connection, and it is never
  // reported closed.
  // covers: RULE-CLIENT-15
  @Test
  void aCancelDropsAClosingConnection(@TempDir Path dir) throws Exception {
    LinkedBlockingQueue<Integer> frames = new LinkedBlockingQueue<>();
    try (Raw server = new Raw(s -> Raw.websocket(s, frames, false))) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, r.next().getKindCase());
      Socket s = server.held();
      r.run(wsClose(1));
      assertEquals(0x8, frames.poll(10, TimeUnit.SECONDS), "our Close frame");
      r.nothingFor(300); // the handshake isn't done: no ws_closed, and no timeout of our own
      r.cancel(1);
      assertTrue(Raw.closedWithin(s, 5000), "dropped at once");
      r.nothingFor(300);
    }
  }

  // After ws_close, ws_closed when the server's Close has come: the handshake is done.
  @Test
  void aClosedConnectionIsReportedWhenTheHandshakeIsDone(@TempDir Path dir) throws Exception {
    LinkedBlockingQueue<Integer> frames = new LinkedBlockingQueue<>();
    try (Raw server = new Raw(s -> Raw.websocket(s, frames, true))) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, r.next().getKindCase());
      r.run(wsClose(1));
      CoreProto.Input closed = r.next();
      assertEquals(CoreProto.Input.KindCase.WS_CLOSED, closed.getKindCase());
      assertEquals(1000, closed.getWsClosed().getCode());
      assertEquals(0x8, frames.poll(1, TimeUnit.SECONDS));
      // It was the last word about it.
      assertFalse(r.x.admit(CoreProto.Input.newBuilder().setWsLost(CoreProto.Failed.newBuilder().setId(1)).build()));
    }
  }

  // The server closes first: that is reported, the core answers when it likes (the JDK sends no
  // Close of its own before), and the handshake's end is reported after the answer.
  @Test
  void theServersCloseIsAnsweredWhenTheCoreSays(@TempDir Path dir) throws Exception {
    LinkedBlockingQueue<Integer> frames = new LinkedBlockingQueue<>();
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head h = TransportTest.Head.read(in);
      OutputStream out = s.getOutputStream();
      Raw.upgrade(out, h);
      out.write(new byte[] {(byte) 0x88, 2, 0x03, (byte) 0xe9}); // Close, 1001
      out.flush();
      Raw.HELD.add(s);
      frames.add(Raw.frame(in));
      s.close();
    })) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, r.next().getKindCase());
      CoreProto.Input first = r.next();
      assertEquals(1001, first.getWsClosed().getCode());
      assertNull(frames.poll(300, TimeUnit.MILLISECONDS), "no Close of ours before the core's ws_close");
      r.run(wsClose(1));
      assertEquals(0x8, frames.poll(10, TimeUnit.SECONDS));
      CoreProto.Input last = r.next();
      assertEquals(CoreProto.Input.KindCase.WS_CLOSED, last.getKindCase());
      assertEquals(1001, last.getWsClosed().getCode());
      assertFalse(r.x.admit(CoreProto.Input.newBuilder().setWsLost(CoreProto.Failed.newBuilder().setId(1)).build()));
    }
  }

  // The server's end goes without a Close frame (a TCP drop): the connection is lost, once. The JDK
  // reports it to onClose as 1006, a code never sent on the wire; it is not the server's Close
  // (found by the differential run of accept-then-drop: Java alone said ws_closed(1006), then
  // ws_closed(1006) again once the core answered it).
  // covers: RULE-SHELL-04
  @Test
  void aConnectionDroppedWithoutACloseFrameIsLost(@TempDir Path dir) throws Exception {
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      Raw.upgrade(s.getOutputStream(), TransportTest.Head.read(in));
      Thread.sleep(200);
      s.close();
    })) {
      Rig r = new Rig(dir.resolve("state"));
      r.run(wsOpen(1, server.url("ws") + "/v1/opamp"));
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, r.next().getKindCase());
      CoreProto.Input end = r.next();
      assertEquals(CoreProto.Input.KindCase.WS_LOST, end.getKindCase(), () -> "reported " + end);
      r.nothingFor(300);
    }
  }

  // A send waiting behind another, and the core's Close behind it, when the connection drops: the
  // JDK's side reports the loss, then fails the outstanding write; stop came in between (the loss
  // reported, nothing in flight), so the write failed after stop had returned, and the chain then
  // started the waiting send and the Close on the dead socket (ShellFrayTest's aSendRacingTheClose,
  // about one schedule in 130). Once the connection is over, nothing more starts on it.
  // covers: RULE-SHELL-07
  @Test
  void nothingStartsOnAConnectionOnceItIsOver() throws Exception {
    FakeHttp http = new FakeHttp();
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(http), got::add, null);
    x.run(wsOpen(1, "ws://fake/v1/opamp").build());
    FakeHttp.FakeWs ws = ((FakeHttp.Open) http.calls.poll()).open();
    assertTrue(x.admit(got.poll(5, TimeUnit.SECONDS)), "ws_opened");
    for (String m : new String[] {"a", "b"}) {
      x.run(CoreProto.Effect.newBuilder().setWsSend(CoreProto.WsMessage.newBuilder().setId(1)
          .setData(ByteString.copyFromUtf8(m))).build());
    }
    x.run(wsClose(1).build()); // "a" outstanding; "b", then the Close, waiting for it
    // The drop, as the JDK's side does it: the listener hears it first...
    ws.open.listener.onError(ws, new java.io.IOException("dropped"));
    CoreProto.Input lost = got.poll(5, TimeUnit.SECONDS);
    assertEquals(CoreProto.Input.KindCase.WS_LOST, lost.getKindCase(), () -> "reported " + lost);
    assertTrue(x.admit(lost), "ws_lost is its end");
    x.close().get(10, TimeUnit.SECONDS); // stop: nothing in flight to abort
    http.seal("stop returned");
    ws.abort(); // ...then its outstanding write fails, after stop has returned
    assertEquals(java.util.List.of(), http.violations, "nothing started after stop returned");
  }

  // The server's Close reported, and the core closing the connection (at stop, say) before it has
  // heard of it: that ws_closed is the server's, not the end of the core's close. Taken for the end,
  // it removed the connection from what is in flight: stop left it open, and the send waiting in
  // its chain started after stop returned (ShellFrayTest's aSendRacingTheClose, after the fix above).
  // covers: RULE-SHELL-04, RULE-SHELL-07
  @Test
  void theServersCloseIsNotTheEndOfTheCoresClose() throws Exception {
    FakeHttp http = new FakeHttp();
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(http), got::add, null);
    x.run(wsOpen(1, "ws://fake/v1/opamp").build());
    FakeHttp.FakeWs ws = ((FakeHttp.Open) http.calls.poll()).open();
    assertTrue(x.admit(got.poll(5, TimeUnit.SECONDS)), "ws_opened");
    x.run(CoreProto.Effect.newBuilder().setWsSend(CoreProto.WsMessage.newBuilder().setId(1)
        .setData(ByteString.copyFromUtf8("goodbye"))).build()); // outstanding
    ws.serverClose(1000);
    CoreProto.Input serverClose = got.poll(5, TimeUnit.SECONDS);
    assertEquals(CoreProto.Input.KindCase.WS_CLOSED, serverClose.getKindCase(), () -> "reported " + serverClose);
    x.run(wsClose(1).build()); // the core closes it before it has heard the server's Close
    assertTrue(x.admit(serverClose), "the server's Close goes to the core");
    x.close().get(10, TimeUnit.SECONDS); // stop: the connection is still in flight, and aborted
    http.seal("stop returned");
    ws.completeWrite(true); // the outstanding send's write completes, after stop returned
    assertTrue(ws.ended(), "the connection is closed at stop");
    assertEquals(java.util.List.of(), http.violations, "nothing started after stop returned");
  }

  // The same, without stop: the end of the core's close is still its end.
  // covers: RULE-SHELL-04
  @Test
  void theEndOfTheCoresCloseAfterTheServersCloseIsItsEnd() throws Exception {
    FakeHttp http = new FakeHttp();
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(http), got::add, null);
    x.run(wsOpen(1, "ws://fake/v1/opamp").build());
    FakeHttp.FakeWs ws = ((FakeHttp.Open) http.calls.poll()).open();
    assertTrue(x.admit(got.poll(5, TimeUnit.SECONDS)), "ws_opened");
    x.run(CoreProto.Effect.newBuilder().setWsSend(CoreProto.WsMessage.newBuilder().setId(1)
        .setData(ByteString.copyFromUtf8("goodbye"))).build());
    ws.serverClose(1000);
    CoreProto.Input serverClose = got.poll(5, TimeUnit.SECONDS);
    x.run(wsClose(1).build());
    assertTrue(x.admit(serverClose), "the server's Close goes to the core");
    assertTrue(x.inFlight.has(1), "the server's Close is not the end of the core's close");
    assertTrue(ws.completeWrite(false), "the send went");
    assertTrue(ws.completeWrite(false), "our Close went");
    CoreProto.Input end = got.poll(5, TimeUnit.SECONDS);
    assertEquals(CoreProto.Input.KindCase.WS_CLOSED, end.getKindCase(), () -> "reported " + end);
    assertTrue(x.admit(end), "the end of the core's close goes to the core");
    assertFalse(x.inFlight.has(1), "and is its end");
    assertTrue(ws.ended(), "the connection is closed");
    assertEquals(java.util.List.of(), http.violations);
  }

  // A download cancelled halfway: aborted, its partial file deleted (by the thread writing it, once
  // the transfer is aborted: nothing is written to it after), never reported.
  // covers: RULE-CLIENT-15
  @Test
  void aCancelledDownloadLeavesNothing(@TempDir Path dir) throws Exception {
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head.read(in);
      OutputStream out = s.getOutputStream();
      out.write("HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\n0123456789".getBytes(StandardCharsets.US_ASCII));
      out.flush();
      Raw.hold(s, in);
    })) {
      Path state = dir.resolve("state");
      Rig r = new Rig(state);
      r.run(CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
          .setId(1).setUrl(server.url("http") + "/file").setMaxBytes(1 << 20).setTimeoutMs(30_000)));
      Path file = r.x.downloadPath(1);
      for (int i = 0; i < 1000 && !(Files.exists(file) && Files.size(file) >= 10); i++) {
        Thread.sleep(10);
      }
      assertEquals(10, Files.size(file), "a partial file");
      Socket s = server.held();
      r.cancel(1);
      assertTrue(Raw.closedWithin(s, 5000), "the download is aborted");
      StateFile.settled(state); // its file work is over
      assertFalse(Files.exists(file), "deleted");
      r.nothingFor(300);
      assertFalse(Files.exists(file));
    }
  }

  // A save the core gave up on before it landed doesn't land, and is never reported.
  // covers: RULE-CLIENT-15
  @Test
  void aCancelledSaveIsNeverReported(@TempDir Path dir) throws Exception {
    Path state = dir.resolve("state");
    CountDownLatch release = new CountDownLatch(1);
    CountDownLatch saving = new CountDownLatch(1);
    StateFile.beforeSave = p -> {
      if (p.equals(state)) {
        saving.countDown();
        Raw.await(release);
      }
    };
    try {
      Rig r = new Rig(state);
      byte[] data = "the state".getBytes(StandardCharsets.US_ASCII);
      r.run(CoreProto.Effect.newBuilder().setStore(CoreProto.Store.newBuilder().setId(1).setData(ByteString.copyFrom(data))));
      assertTrue(saving.await(10, TimeUnit.SECONDS));
      r.cancel(1);
      release.countDown();
      StateFile.settled(state);
      assertFalse(Files.exists(state), "not landed");
      r.nothingFor(300);
    } finally {
      StateFile.beforeSave = p -> {};
    }
  }

  // S3: a client ends while its download waits for the file's headers; the next client on the same
  // state file downloads with the same effect id. The old download's response comes after: the
  // next client's file is still its own.
  @Test
  void anEndedClientsDownloadLeavesTheNextClientsFileAlone(@TempDir Path dir) throws Exception {
    byte[] body = "the package".repeat(20).getBytes(StandardCharsets.US_ASCII);
    CountDownLatch release = new CountDownLatch(1);
    CountDownLatch firstAsked = new CountDownLatch(1);
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head.read(in);
      byte[] b = body;
      if (firstAsked.getCount() > 0) {
        firstAsked.countDown();
        release.await();
        b = "WRONG".repeat(40).getBytes(StandardCharsets.US_ASCII);
      }
      OutputStream out = s.getOutputStream();
      out.write(("HTTP/1.1 200 OK\r\nContent-Length: " + b.length + "\r\nConnection: close\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
      out.write(b);
      out.flush();
      s.close();
    })) {
      Path state = dir.resolve("state");
      CoreProto.Effect.Builder get = CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
          .setId(1).setUrl(server.url("http") + "/p").setMaxBytes(1 << 20).setTimeoutMs(30_000));
      Rig a = new Rig(state);
      a.run(get);
      assertTrue(firstAsked.await(10, TimeUnit.SECONDS));
      a.x.close(); // the client ends
      Rig b = new Rig(state);
      b.run(get);
      assertEquals(200, b.next().getDownloadResponse().getStatus());
      release.countDown();
      Thread.sleep(500);
      assertArrayEquals(body, Files.readAllBytes(b.x.downloadPath(1)), "the next client's file");
    }
  }
}
