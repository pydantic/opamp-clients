// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import javax.net.ssl.SSLSession;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;
import org.junit.jupiter.api.io.TempDir;

/**
 * A response that is complete before the shell attaches what reads its body. A {@code
 * CompletableFuture} runs a continuation attached to a future already complete on the thread that
 * attaches it, here the client's: a slow or stalled body then held up the client's loop (its
 * timers, the other effects, stop). The network here ({@link FakeHttp#atSend}) completes every
 * response before {@code sendAsync} returns, with a body that gives nothing until it is closed.
 */
// covers: RULE-SHELL-05, RULE-SHELL-07
@Timeout(value = 60, threadMode = Timeout.ThreadMode.SEPARATE_THREAD)
class BodyThreadTest {
  /** A body that gives nothing until it is closed (then the read fails), and says who read it. */
  static final class Stalled extends InputStream {
    final CountDownLatch closed = new CountDownLatch(1);
    final LinkedBlockingQueue<String> readers = new LinkedBlockingQueue<>();

    @Override
    public int read() throws IOException {
      return read(new byte[1], 0, 1);
    }

    @Override
    public int read(byte[] b, int off, int len) throws IOException {
      readers.add(Thread.currentThread().getName());
      try {
        closed.await();
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
      throw new IOException("closed");
    }

    @Override
    public void close() {
      closed.countDown();
    }
  }

  record Response(HttpRequest request, int statusCode, InputStream body) implements HttpResponse<InputStream> {
    @Override
    public Optional<HttpResponse<InputStream>> previousResponse() {
      return Optional.empty();
    }

    @Override
    public HttpHeaders headers() {
      return HttpHeaders.of(Map.of(), (a, b) -> true);
    }

    @Override
    public Optional<SSLSession> sslSession() {
      return Optional.empty();
    }

    @Override
    public URI uri() {
      return request.uri();
    }

    @Override
    public HttpClient.Version version() {
      return HttpClient.Version.HTTP_1_1;
    }
  }

  /** {@code effect} run on a thread of its own, as the client's: it must return at once. */
  private static void runsAtOnce(Executor x, CoreProto.Effect effect) throws InterruptedException {
    Thread client = new Thread(() -> x.run(effect), "opamp-client");
    client.setDaemon(true);
    client.start();
    client.join(2_000);
    assertFalse(client.isAlive(), "the effect held the client's thread: it read the body there");
  }

  @Test
  void aRequestsBodyIsNotReadOnTheClientsThread(@TempDir Path dir) throws Exception {
    Stalled body = new Stalled();
    FakeHttp http = new FakeHttp();
    http.atSend = r -> r.future.complete(new Response(r.request, 200, body));
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(http), got::add, dir.resolve("state"));
    runsAtOnce(x, CoreProto.Effect.newBuilder().setHttpRequest(CoreProto.HttpRequest.newBuilder()
        .setId(1).setUrl("http://fake/v1/opamp").setBody(ByteString.copyFromUtf8("hi"))
        .setTimeoutMs(30_000).setMaxResponseBytes(1 << 20)).build());
    String reader = body.readers.poll(5, TimeUnit.SECONDS);
    assertNotNull(reader, "the body is read");
    assertFalse(reader.equals("opamp-client"), "the body is read on " + reader);
    x.run(CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(1)).build());
    assertTrue(body.closed.await(5, TimeUnit.SECONDS), "the cancel closes the body");
    CoreProto.Input in = got.poll(5, TimeUnit.SECONDS);
    assertNotNull(in, "the request ends");
    assertTrue(in.hasHttpFailed(), "it failed: " + in);
    x.close().get(10, TimeUnit.SECONDS);
  }

  @Test
  void aDownloadsBodyIsNotReadOnTheClientsThread(@TempDir Path dir) throws Exception {
    Stalled body = new Stalled();
    FakeHttp http = new FakeHttp();
    http.atSend = r -> r.future.complete(new Response(r.request, 200, body));
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(http), got::add, dir.resolve("state"));
    runsAtOnce(x, CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
        .setId(2).setUrl("http://fake/p").setTimeoutMs(30_000).setMaxBytes(1 << 20)).build());
    String reader = body.readers.poll(5, TimeUnit.SECONDS);
    assertNotNull(reader, "the body is read");
    assertFalse(reader.equals("opamp-client"), "the body is read on " + reader);
    x.run(CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(2)).build());
    assertTrue(body.closed.await(5, TimeUnit.SECONDS), "the cancel closes the body");
    CoreProto.Input in = got.poll(5, TimeUnit.SECONDS);
    assertNotNull(in, "the download ends");
    assertTrue(in.hasDownloadFailed(), "it failed: " + in);
    x.close().get(10, TimeUnit.SECONDS);
  }

  /**
   * The whole client: its first poll's response is complete before the shell reads it, and its
   * body stalls. The loop goes on all the same: the core's request timeout fires and its cancel
   * closes the body, the next poll is answered, and stop (the goodbye answered) is quick, with
   * {@code stopped} told.
   */
  @Test
  void theLoopGoesOnWhileABodyStalls() throws Exception {
    Stalled body = new Stalled();
    AtomicInteger posts = new AtomicInteger();
    FakeHttp http = new FakeHttp();
    http.atSend = r -> {
      if (posts.incrementAndGet() == 1) {
        r.future.complete(new Response(r.request, 200, body));
        return;
      }
      try {
        Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(r.body);
        r.answer(200, Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid()).build().toByteArray());
      } catch (IOException e) {
        r.fail(e.getMessage());
      }
    };
    LinkedBlockingQueue<String> heard = new LinkedBlockingQueue<>();
    OpampClient c = OpampClient.start(OpampClient.Config.builder("http://fake/v1/opamp", new byte[16])
        .httpClient(http).requestTimeout(Duration.ofMillis(500)).heartbeatInterval(Duration.ofMillis(200))
        .stopTimeout(Duration.ofSeconds(2)).build(),
        OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()),
        n -> heard.add(n.getClass().getSimpleName()));
    String reader = body.readers.poll(5, TimeUnit.SECONDS);
    assertNotNull(reader, "the first poll's body is read");
    assertFalse(reader.equals("opamp-client"), "the body is read on the client's thread");
    assertTrue(body.closed.await(5, TimeUnit.SECONDS), "the request timeout's cancel closes the stalled body");
    for (long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(10); posts.get() < 2 && System.nanoTime() < end; ) {
      Thread.sleep(20);
    }
    assertTrue(posts.get() >= 2, "the client polls again");
    long t0 = System.nanoTime();
    c.stop().get(10, TimeUnit.SECONDS);
    long ms = (System.nanoTime() - t0) / 1_000_000;
    assertTrue(ms < 2_000 + 900, "stop took " + ms + " ms");
    for (String n = heard.poll(2, TimeUnit.SECONDS); ; n = heard.poll(2, TimeUnit.SECONDS)) {
      assertNotNull(n, "stopped is told");
      if (n.equals("Stopped")) {
        break;
      }
    }
    assertEquals(List.of(), List.copyOf(http.violations));
  }
}
