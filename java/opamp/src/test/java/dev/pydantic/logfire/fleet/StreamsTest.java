// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.logfire.fleet.core.CoreProto;
import java.io.IOException;
import java.io.InputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLSocket;
import org.junit.jupiter.api.Test;

/**
 * Core mode's executor ({@link Streams}) on its own, the test playing the core: its effects go in
 * on a loop of its own, what it reports comes out. Over {@link MemoryNet} (the test chooses when
 * each connection is made, and how fast the peer reads) and, for TLS, real sockets. The registry: a
 * close, a cancel and shutdown end what they name at once; a connection, a handshake or a
 * resolution that completes after its close is closed and reported to no one; nothing is reported
 * after a close or after shutdown. Backpressure both ways.
 */
// covers: RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07
class StreamsTest {
  /** A Streams on a loop of its own, and what it reports. */
  static final class Harness implements AutoCloseable {
    final ScheduledExecutorService loop = Executors.newSingleThreadScheduledExecutor();
    final LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    final Streams x;
    /** Run on the loop for each input, after it is recorded (the core's answer, say). */
    volatile Consumer<CoreProto.Input> core = in -> {};

    Harness(Streams.Platform platform) {
      this(platform, TransportTest.trusting(TransportTest.CA));
    }

    /** With the operator's TLS settings {@code tls}. */
    Harness(Streams.Platform platform, Tls tls) {
      x = new Streams(loop, in -> {
        got.add(in);
        core.accept(in);
      }, platform, Streams.originEngines(tls),
          url -> CoreProto.ProxyAnswer.newBuilder());
    }

    void run(CoreProto.Effect e) throws Exception {
      loop.submit(() -> x.run(e)).get(10, TimeUnit.SECONDS);
    }

    CoreProto.Input next() throws InterruptedException {
      CoreProto.Input in = got.poll(10, TimeUnit.SECONDS);
      assertNotNull(in, "an input");
      return in;
    }

    /** Nothing reported for {@code ms}. */
    void quiet(long ms) throws Exception {
      CoreProto.Input in = got.poll(ms, TimeUnit.MILLISECONDS);
      assertNull(in, () -> "reported: " + in);
    }

    CompletableFuture<Void> shutdown() throws Exception {
      return loop.submit(x::shutdown).get(10, TimeUnit.SECONDS);
    }

    boolean empty() throws Exception {
      return left().isEmpty();
    }

    String left() throws Exception {
      return loop.submit(() -> x.in.left()).get(10, TimeUnit.SECONDS);
    }

    @Override
    public void close() throws Exception {
      loop.submit(x::shutdown).get(10, TimeUnit.SECONDS);
      loop.shutdownNow();
    }
  }

  static CoreProto.Effect open(long id) {
    return CoreProto.Effect.newBuilder().setStreamOpen(CoreProto.StreamOpen.newBuilder().setId(id).setHost("server").setPort(80)).build();
  }

  static CoreProto.Effect write(long id, byte[] data) {
    return CoreProto.Effect.newBuilder().setStreamWrite(CoreProto.WsMessage.newBuilder().setId(id).setData(ByteString.copyFrom(data))).build();
  }

  static CoreProto.Effect close(long id, boolean abort) {
    return CoreProto.Effect.newBuilder().setStreamClose(CoreProto.StreamClose.newBuilder().setId(id).setAbort(abort)).build();
  }

  static CoreProto.Effect startTls(long id, String host) {
    return startTls(id, host, true);
  }

  /** TLS, at the server's own origin or not. */
  static CoreProto.Effect startTls(long id, String host, boolean serverOrigin) {
    return CoreProto.Effect.newBuilder()
        .setStreamStartTls(CoreProto.StreamName.newBuilder().setId(id).setHost(host).setServerOrigin(serverOrigin)).build();
  }

  static CoreProto.Effect resolve(long id, String host) {
    return CoreProto.Effect.newBuilder().setStreamResolve(CoreProto.StreamName.newBuilder().setId(id).setHost(host)).build();
  }

  static byte[] ascii(String s) {
    return s.getBytes(StandardCharsets.US_ASCII);
  }

  static MemoryNet.End opened(Harness h, MemoryNet net, long id) throws Exception {
    h.run(open(id));
    MemoryNet.End peer = net.dials.poll(10, TimeUnit.SECONDS).accept();
    assertEquals(CoreProto.Input.KindCase.STREAM_OPENED, h.next().getKindCase());
    return peer;
  }

  // ---------------------------------------------------------------------------------------------
  // Streams.

  @Test
  void aStreamCarriesBytesBothWaysAndACloseLetsWrittenBytesGo() throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net)) {
      MemoryNet.End peer = opened(h, net, 1);
      h.run(write(1, ascii("hello")));
      assertArrayEquals(ascii("hello"), peer.read(5));
      peer.write(ascii("world"));
      CoreProto.Input in = h.next();
      assertEquals("world", in.getStreamData().getData().toStringUtf8());
      // Written, then closed: the bytes go, then the FIN; nothing more is reported.
      h.run(write(1, ascii("last")));
      h.run(close(1, false));
      assertArrayEquals(ascii("last"), peer.read(10));
      try {
        peer.write(ascii("unheard"));
      } catch (IOException e) {
        // it reads nothing more: as good as unheard
      }
      h.quiet(200);
      assertTrue(net.dials.isEmpty());
      for (int i = 0; i < 100 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertTrue(h.empty(), "the linger is over once the FIN went");
      assertEquals("", net.leftOpen());
    }
  }

  @Test
  void aRefusedConnectionAPeerThatClosesAndOneThatResetsAreReported() throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net)) {
      h.run(open(1));
      net.dials.poll(10, TimeUnit.SECONDS).refuse("connection refused");
      CoreProto.Input failed = h.next();
      assertEquals(1, failed.getStreamFailed().getId());
      assertEquals("connection refused", failed.getStreamFailed().getDetail());

      MemoryNet.End peer = opened(h, net, 2);
      peer.write(ascii("bye"));
      peer.finish();
      assertEquals("bye", h.next().getStreamData().getData().toStringUtf8());
      assertEquals(2, h.next().getStreamEof().getId());

      MemoryNet.End reset = opened(h, net, 3);
      reset.abort();
      CoreProto.Input lost = h.next();
      assertEquals(3, lost.getStreamFailed().getId(), lost.toString());
      h.quiet(100);
    }
  }

  /**
   * A stream the core closed lingers while what was written goes; its peer resets meanwhile, and
   * its writer is slow to see the close (a thread not yet run): the reset ends the linger, so it
   * must end the connection too, or nothing closes it before the end (StreamsFrayTest seed
   * 6806190798101515795 left it open after shutdown).
   */
  @Test
  void aLingeringStreamWhosePeerResetsIsClosedAtOnce() throws Exception {
    MemoryNet net = new MemoryNet();
    CountDownLatch writers = new CountDownLatch(1);
    Streams.Platform slowWriters = new Streams.Platform() {
      @Override
      public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
        return net.connect(host, port, attempt);
      }

      @Override
      public List<String> resolve(String host) throws IOException {
        return net.resolve(host);
      }

      @Override
      public void start(String name, Runnable r) {
        Streams.Platform.super.start(name, !name.endsWith("-write") ? r : () -> {
          try {
            writers.await();
          } catch (InterruptedException e) {
            return;
          }
          r.run();
        });
      }
    };
    try (Harness h = new Harness(slowWriters)) {
      MemoryNet.End peer = opened(h, net, 1);
      h.run(close(1, false));
      peer.abort();
      for (int i = 0; i < 500 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertTrue(h.empty(), "the reset ended the linger: " + h.left());
      assertEquals("", net.leftOpen(), "a linger over is a connection over");
      h.shutdown().get(5, TimeUnit.SECONDS);
      assertEquals("", net.leftOpen());
      h.quiet(100);
    } finally {
      writers.countDown();
    }
  }

  @Test
  void aStreamThatConnectsAfterItsCloseIsAbortedAtOnce() throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net)) {
      for (boolean abort : new boolean[] {true, false}) {
        h.run(open(1));
        MemoryNet.Dial d = net.dials.poll(10, TimeUnit.SECONDS);
        h.run(close(1, abort));
        assertTrue(d.aborted(), "the attempt is given up at once");
        MemoryNet.End peer = d.accept();
        assertTrue(d.client().aborted(), "a connection made after the close is reset");
        assertEquals(-1, readOrEnd(peer));
        h.quiet(100);
      }
      assertTrue(h.empty());
    }
  }

  /** -1 at the end or on a reset. */
  static int readOrEnd(MemoryNet.End peer) {
    try {
      return peer.in.read(new byte[1], 0, 1);
    } catch (IOException e) {
      return -1;
    }
  }

  @Test
  void namesAreResolvedForAStreamAndACloseStopsAResolution() throws Exception {
    MemoryNet net = new MemoryNet();
    net.names.put("proxy.example", List.of("192.0.2.1", "2001:db8::1"));
    try (Harness h = new Harness(net)) {
      h.run(resolve(1, "proxy.example"));
      CoreProto.Input in = h.next();
      assertEquals(List.of("192.0.2.1", "2001:db8::1"), in.getStreamResolved().getAddressesList());
      h.run(resolve(2, "nowhere.example"));
      assertEquals(2, h.next().getStreamFailed().getId());
      // A resolution closed while it runs: its answer goes nowhere.
      net.holdResolutions(true);
      h.run(resolve(3, "proxy.example"));
      h.run(close(3, true));
      net.holdResolutions(false);
      h.quiet(200);
      assertTrue(h.empty());
    }
  }

  @Test
  void shutdownEndsConnectsStreamsResolutionsAndLingeringClosesAtOnce() throws Exception {
    MemoryNet net = new MemoryNet(16);
    try (Harness h = new Harness(net)) {
      MemoryNet.End lingering = opened(h, net, 1);
      // Closed with more written than the peer takes: it lingers.
      h.run(write(1, new byte[1000]));
      h.run(close(1, false));
      MemoryNet.End open = opened(h, net, 2);
      h.run(open(3));
      MemoryNet.Dial connecting = net.dials.poll(10, TimeUnit.SECONDS);
      net.holdResolutions(true);
      h.run(resolve(4, "x"));
      assertFalse(h.empty());
      long t0 = System.nanoTime();
      h.shutdown().get(5, TimeUnit.SECONDS);
      assertTrue((System.nanoTime() - t0) / 1_000_000 < 1_000, "at once");
      assertTrue(h.empty());
      assertTrue(connecting.aborted());
      assertEquals("", net.leftOpen(), "every connection reset, the lingering one too");
      assertTrue(lingering.in.waiting() < 1000, "what was still to go was dropped");
      net.holdResolutions(false);
      try {
        open.write(ascii("late"));
      } catch (IOException e) {
        // reset
      }
      h.quiet(200);
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Backpressure.

  @Test
  void aPeerThatStopsReadingIsNotReadUntilItDrainsAndPastABoundFails() throws Exception {
    MemoryNet net = new MemoryNet(1024);
    try (Harness h = new Harness(net)) {
      MemoryNet.End peer = opened(h, net, 1);
      // The stream is in its first read before anything is written: on a loaded machine its
      // reader had not got there when the write below passed WRITE_HIGH, so no read was waiting
      // and "ping" was never read.
      for (int i = 0; i < 5_000 && !peer.out.readerWaiting(); i++) {
        Thread.sleep(2);
      }
      assertTrue(peer.out.readerWaiting(), "the stream's reader is waiting for a byte");
      // The peer reads nothing: what is written waits; past WRITE_HIGH the stream reads nothing.
      byte[] big = new byte[Streams.WRITE_HIGH + 1];
      h.run(write(1, big));
      Thread.sleep(100);
      // A read already waiting completes (a blocking read can't be taken back); the next waits.
      peer.write(ascii("ping"));
      assertEquals("ping", h.next().getStreamData().getData().toStringUtf8());
      peer.write(ascii("pong"));
      h.quiet(300);
      // It reads: the stream drains, and reads again.
      assertEquals(big.length, peer.read(big.length).length);
      assertEquals("pong", h.next().getStreamData().getData().toStringUtf8());
      // Past MAX_UNSENT: the next write fails the stream.
      h.run(write(1, new byte[(int) Streams.MAX_UNSENT + 1]));
      Thread.sleep(100);
      h.run(write(1, ascii("x")));
      CoreProto.Input failed = h.next();
      assertTrue(failed.getStreamFailed().getDetail().contains("has not taken"), failed.toString());
      assertEquals("", net.leftOpen());
    }
  }

  /**
   * A stream this gives up from within an effect (a write past the bound of what waits unsent)
   * tells the core from the loop's next turn: if the core closes the stream first (a close in the
   * same output, or in the output of an input that was waiting ahead of it), nothing is said.
   */
  // covers: RULE-SHELL-06
  @Test
  void aFailureOnItsWayWhenItsStreamIsClosedIsDropped() throws Exception {
    MemoryNet net = new MemoryNet(1024);
    try (Harness h = new Harness(net)) {
      opened(h, net, 1);
      // The peer reads nothing: more than MAX_UNSENT waits.
      h.run(write(1, new byte[(int) Streams.MAX_UNSENT + 1]));
      Thread.sleep(100);
      // One turn of the loop, as the effects of one output: the write that fails it, and its close.
      h.loop.submit(() -> {
        h.x.run(write(1, ascii("x")));
        h.x.run(close(1, true));
      }).get(10, TimeUnit.SECONDS);
      h.quiet(300);
      assertEquals("", net.leftOpen());
      assertTrue(h.empty(), h.left());
    }
  }

  // TLS, over real sockets.

  /**
   * The client certificate goes only to a handshake the core says is with the server's own origin
   * (stream_start_tls's server_origin); any other presents none, and the CA still applies.
   */
  // covers: RULE-TLS-02
  @Test
  void theClientCertificateOnlyAtTheServersOrigin() throws Exception {
    SSLContext server = Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
    try (ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
        Harness h = new Harness(new Streams.Platform() {
          @Override
          public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
            return Streams.SOCKETS.connect("127.0.0.1", listener.getLocalPort(), attempt);
          }

          @Override
          public List<String> resolve(String host) {
            return List.of();
          }
        }, TransportTest.withClientCert())) {
      List<String> peers = new CopyOnWriteArrayList<>();
      Thread t = new Thread(() -> {
        try {
          for (int i = 0; i < 2; i++) {
            Socket raw = listener.accept();
            SSLSocket s = (SSLSocket) server.getSocketFactory().createSocket(raw, null, raw.getPort(), true);
            s.setUseClientMode(false);
            s.setWantClientAuth(true);
            try (s) {
              s.startHandshake();
              String peer;
              try {
                peer = ((java.security.cert.X509Certificate) s.getSession().getPeerCertificates()[0])
                    .getSubjectX500Principal().getName();
              } catch (javax.net.ssl.SSLPeerUnverifiedException none) {
                peer = "none";
              }
              peers.add(peer);
              s.getInputStream().read(); // until the client goes
            }
          }
        } catch (Exception e) {
          peers.add("failed: " + e);
        }
      });
      t.start();
      for (long id : new long[] {1, 2}) {
        boolean origin = id == 1;
        h.run(open(id));
        CoreProto.Input opened = h.next();
        assertTrue(opened.hasStreamOpened(), opened.toString());
        h.run(startTls(id, "127.0.0.1", origin));
        CoreProto.Input ready = h.next();
        assertEquals(id, ready.getStreamTlsReady().getId(), ready.toString());
        for (int i = 0; i < 500 && peers.size() < id; i++) {
          Thread.sleep(10);
        }
        String peer = peers.get((int) id - 1);
        assertEquals(origin, peer.contains("test-agent"), "server_origin " + origin + ": " + peer);
        assertTrue(origin || peer.equals("none"), peer);
        h.run(close(id, false));
      }
      t.join(10_000);
    }
  }

  /** TLS up, bytes both ways under it; a handshake still going when its stream is closed says nothing. */
  @Test
  void tlsOverAStreamAndAHandshakeCompletingAfterItsCloseIsAborted() throws Exception {
    SSLContext server = Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
    try (ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
        Harness h = new Harness(new Streams.Platform() {
          @Override
          public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
            return Streams.SOCKETS.connect("127.0.0.1", listener.getLocalPort(), attempt);
          }

          @Override
          public List<String> resolve(String host) {
            return List.of();
          }
        })) {
      // Echo over TLS.
      CountDownLatch go = new CountDownLatch(1);
      List<Throwable> serverSaw = new CopyOnWriteArrayList<>();
      Thread t = new Thread(() -> {
        try {
          for (int i = 0; i < 4; i++) {
            Socket raw = listener.accept();
            if (i == 1) {
              go.await();
            }
            SSLSocket s = (SSLSocket) server.getSocketFactory().createSocket(raw, null, raw.getPort(), true);
            s.setUseClientMode(false);
            try (s) {
              InputStream in = s.getInputStream();
              byte[] b = new byte[5];
              int n = in.readNBytes(b, 0, 5);
              s.getOutputStream().write(b, 0, n);
              s.getOutputStream().flush();
            } catch (IOException e) {
              serverSaw.add(e);
            }
          }
        } catch (Exception e) {
          serverSaw.add(e);
        }
      });
      t.start();
      h.run(open(1));
      assertTrue(h.next().hasStreamOpened());
      h.run(startTls(1, "127.0.0.1"));
      assertEquals(1, h.next().getStreamTlsReady().getId());
      h.run(write(1, ascii("hello")));
      assertEquals("hello", h.next().getStreamData().getData().toStringUtf8());
      h.run(close(1, false));
      // The second: closed during its handshake, which the server then completes (or tries to).
      h.run(open(2));
      CoreProto.Input in = h.next();
      if (in.hasStreamEof()) {
        in = h.next(); // the first's: the server ended it at once, maybe before its close
      }
      assertTrue(in.hasStreamOpened(), in.toString());
      h.run(startTls(2, "localhost"));
      Thread.sleep(100);
      h.run(close(2, false));
      go.countDown();
      // A certificate verified for the name or the address the core gives: one it doesn't name fails.
      for (long id : new long[] {3, 4}) {
        h.run(open(id));
        CoreProto.Input opened = h.next();
        assertTrue(opened.hasStreamOpened(), opened.toString());
        h.run(startTls(id, id == 3 ? "not-in-the-certificate.example" : "192.0.2.7"));
        CoreProto.Input failed = h.next();
        assertEquals(id, failed.getStreamFailed().getId(), failed.toString());
      }
      t.join(10_000);
      h.quiet(300);
      for (int i = 0; i < 100 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertEquals("", h.left());
    }
  }
}
