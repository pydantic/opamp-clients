// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Base64;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.Function;
import java.util.stream.IntStream;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * Properties of the client around the core, over random plans (a seeded generator): the
 * application's calls, spread over one to four threads of its own that call the client at the same
 * time, and the notification callback (so they reach the client while it is still carrying out the step that
 * called back), over either transport. Order is only defined per caller. Whatever the plan: every
 * custom message reaches the server exactly once, each caller's in the order it sent them; the
 * server ends with the description set last; sequence numbers run without a gap; the goodbye is the
 * last message; and nothing is reported as an error. OPAMP_PROP_RUNS sets the number of plans
 * (default 25); a failure prints its run's seed, and OPAMP_PROP_SEED replays that one run.
 */
class PropertiesTest {
  private static final byte[] UID = new byte[16];
  private static final String CAPABILITY = "io.example.tick";

  static {
    java.util.Arrays.fill(UID, (byte) 0x42);
  }

  enum Kind { DESCRIBE, SEND }

  /**
   * A call: a new description or a custom message, by {@code caller} (a thread, or the callback when
   * it is the plan's thread count). {@code pause}: the caller waits first (a thread a millisecond,
   * the callback for the next notification); otherwise it goes straight on from its previous call.
   */
  record Step(Kind kind, int caller, boolean pause) {}

  /** A plan, and the transport: WebSocket or plain HTTP. */
  record Plan(int threads, List<Step> steps, boolean ws) {}

  static Plan plan(SplittableRandom r) {
    int threads = 1 + r.nextInt(4);
    List<Step> steps = new ArrayList<>();
    for (int i = 1 + r.nextInt(16); i > 0; i--) {
      steps.add(new Step(r.nextBoolean() ? Kind.DESCRIBE : Kind.SEND, r.nextInt(threads + 1), r.nextBoolean()));
    }
    return new Plan(threads, steps, r.nextBoolean());
  }

  @Test
  void callsFromAnyThreadArriveOnceAndInOrder() throws Exception {
    String runsEnv = System.getenv("OPAMP_PROP_RUNS");
    String seedEnv = System.getenv("OPAMP_PROP_SEED");
    int runs = seedEnv != null && !seedEnv.isEmpty() ? 1
        : runsEnv == null || runsEnv.isEmpty() ? 25 : Integer.parseInt(runsEnv);
    long base = System.nanoTime();
    for (int i = 0; i < runs; i++) {
      // Each run has a seed of its own, so one can be replayed alone.
      long seed = seedEnv != null && !seedEnv.isEmpty() ? Long.parseLong(seedEnv) : base + i * 0x9E3779B97F4A7C15L;
      Plan plan = plan(new SplittableRandom(seed));
      try {
        check(plan);
      } catch (Throwable e) {
        throw new AssertionError("run " + i + " of " + runs + " (replay: OPAMP_PROP_SEED=" + seed + "), " + plan
            + ": " + e.getMessage(), e);
      }
    }
  }

  private static void check(Plan plan) throws Exception {
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    Function<Opamp.AgentToServer, byte[]> reply = msg -> {
      got.add(msg);
      // Every answer carries a custom message, so the application is called back often.
      Opamp.CustomMessage tick = Opamp.CustomMessage.newBuilder().setCapability(CAPABILITY).setType("tick").build();
      return Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid()).setCustomMessage(tick).build()
          .toByteArray();
    };
    try (Server server = plan.ws() ? new WsServer(reply) : new HttpOpamp(reply)) {
      run(plan, plan.ws(), server.port(), got);
    }
  }

  private static void run(Plan plan, boolean ws, int port, List<Opamp.AgentToServer> got) throws Exception {
    int callback = plan.threads();
    List<List<Integer>> byCaller = IntStream.rangeClosed(0, callback)
        .mapToObj(c -> IntStream.range(0, plan.steps().size()).filter(i -> plan.steps().get(i).caller() == c).boxed()
            .toList())
        .toList();
    // What was sent, per caller, and the description set last (under the lock, with the call).
    List<List<Integer>> sent = IntStream.rangeClosed(0, callback)
        .mapToObj(c -> (List<Integer>) new CopyOnWriteArrayList<Integer>()).toList();
    Object lock = new Object();
    AtomicInteger lastDescription = new AtomicInteger();
    List<Object> errors = new CopyOnWriteArrayList<>();
    AtomicReference<OpampClient> client = new AtomicReference<>();

    // Step i of the plan; its number (i + 1) is the description, or the custom message's data.
    java.util.function.IntConsumer carryOut = i -> {
      Step s = plan.steps().get(i);
      int n = i + 1;
      if (s.kind() == Kind.DESCRIBE) {
        synchronized (lock) {
          lastDescription.set(n);
          client.get().setDescription(description(n));
        }
      } else {
        sent.get(s.caller()).add(n);
        client.get().sendCustomMessage(Opamp.CustomMessage.newBuilder().setCapability(CAPABILITY).setType("t")
            .setData(ByteString.copyFromUtf8(Integer.toString(n))).build());
      }
    };

    List<Integer> fromCallback = byCaller.get(callback);
    AtomicInteger callbackDone = new AtomicInteger();
    java.util.function.Consumer<Notification> notify = n -> {
      if (n instanceof Notification.Stopped st && st.error() != null) {
        errors.add(st.error());
      }
      if (n instanceof Notification.CustomMessageNotSent ns) {
        errors.add(ns);
      }
      try {
        // The callback's calls, from inside it: the next, and those that follow straight on.
        int k = callbackDone.get();
        if (client.get() != null && k < fromCallback.size()) {
          do {
            carryOut.accept(fromCallback.get(k++));
          } while (k < fromCallback.size() && !plan.steps().get(fromCallback.get(k)).pause());
          callbackDone.set(k);
        }
      } catch (RuntimeException e) {
        errors.add(e);
      }
    };

    String endpoint = (ws ? "ws" : "http") + "://127.0.0.1:" + port + "/v1/opamp";
    client.set(OpampClient.start(
        OpampClient.Config.builder(endpoint, UID).mode(OpampClient.Mode.PLATFORM).heartbeatInterval(Duration.ofMillis(20)).build(),
        new OpampClient.Status(description(0), null, null, null, null,
            Opamp.CustomCapabilities.newBuilder().addCapabilities(CAPABILITY).build()),
        notify));

    // The threads, started together.
    CountDownLatch go = new CountDownLatch(1);
    List<Thread> threads = new ArrayList<>();
    for (int t = 0; t < plan.threads(); t++) {
      List<Integer> mine = byCaller.get(t);
      threads.add(start(() -> {
        try {
          go.await();
          for (int i : mine) {
            if (plan.steps().get(i).pause()) {
              Thread.sleep(1);
            }
            carryOut.accept(i);
          }
        } catch (Throwable e) {
          errors.add(e);
        }
      }));
    }
    java.util.function.Supplier<List<Integer>> delivered = () -> got.stream()
        .filter(m -> m.hasCustomMessage() && m.getCustomMessage().getType().equals("t"))
        .map(m -> Integer.parseInt(m.getCustomMessage().getData().toStringUtf8()))
        .toList();
    java.util.function.Supplier<List<Long>> descriptions = () -> got.stream()
        .filter(Opamp.AgentToServer::hasAgentDescription)
        .map(m -> m.getAgentDescription().getNonIdentifyingAttributes(0).getValue().getIntValue())
        .toList();
    try {
      go.countDown();
      // Generous deadlines, for a loaded machine: each wait ends as soon as it can.
      for (Thread t : threads) {
        t.join(20_000);
      }
      waitFor(() -> callbackDone.get() == fromCallback.size());
      assertTrue(threads.stream().noneMatch(Thread::isAlive) && callbackDone.get() == fromCallback.size(),
          "the plan stalled; errors: " + errors);
      waitFor(() -> {
        List<Long> d = descriptions.get();
        return delivered.get().stream().sorted().toList().equals(sent.stream().flatMap(List::stream).sorted().toList())
            && !d.isEmpty() && d.get(d.size() - 1) == lastDescription.get();
      });
    } finally {
      client.get().stop().get(20, TimeUnit.SECONDS);
    }

    assertEquals(List.of(), errors);
    List<Integer> allSent = sent.stream().flatMap(List::stream).sorted().toList();
    List<Integer> arrived = delivered.get();
    assertEquals(allSent, arrived.stream().sorted().toList(), "every custom message exactly once");
    for (int c = 0; c <= callback; c++) {
      Set<Integer> theirs = Set.copyOf(sent.get(c));
      assertEquals(sent.get(c), arrived.stream().filter(theirs::contains).toList(),
          (c == callback ? "the callback's" : "thread " + c + "'s") + " custom messages in order");
    }
    List<Long> d = descriptions.get();
    assertEquals((long) lastDescription.get(), d.get(d.size() - 1), "the server has the description set last");
    assertEquals(OpampClientTest.expectedSequence(got), OpampClientTest.sequence(got),
        "sequence numbers 0 until the first answer, then without a gap");
    assertTrue(got.get(got.size() - 1).hasAgentDisconnect(), "the goodbye is last");
  }

  private static void waitFor(java.util.function.BooleanSupplier done) throws InterruptedException {
    for (long end = System.nanoTime() + 10_000_000_000L; !done.getAsBoolean() && System.nanoTime() < end; ) {
      Thread.sleep(2);
    }
  }

  private static Opamp.AgentDescription description(int n) {
    return Opamp.AgentDescription.newBuilder()
        .addNonIdentifyingAttributes(Anyvalue.KeyValue.newBuilder()
            .setKey("n").setValue(Anyvalue.AnyValue.newBuilder().setIntValue(n)))
        .build();
  }

  private static Thread start(Runnable r) {
    Thread t = new Thread(r);
    t.setDaemon(true);
    t.start();
    return t;
  }

  /** A local OpAMP server answering every message with {@code reply}'s ServerToAgent. */
  interface Server extends AutoCloseable {
    int port();
  }

  /** Over plain HTTP. */
  static final class HttpOpamp implements Server {
    private final HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    private final ExecutorService pool = Executors.newCachedThreadPool();

    HttpOpamp(Function<Opamp.AgentToServer, byte[]> reply) throws IOException {
      server.setExecutor(pool);
      server.createContext("/v1/opamp", ex -> {
        byte[] out = reply.apply(Opamp.AgentToServer.parseFrom(ex.getRequestBody().readAllBytes()));
        ex.getResponseHeaders().set("Content-Type", "application/x-protobuf");
        ex.sendResponseHeaders(200, out.length);
        ex.getResponseBody().write(out);
        ex.close();
      });
      server.start();
    }

    @Override
    public int port() {
      return server.getAddress().getPort();
    }

    @Override
    public void close() {
      server.stop(0);
      pool.shutdownNow();
    }
  }

  /**
   * An OpAMP WebSocket server, just enough of RFC 6455 for the JDK's client: the handshake, masked
   * frames in (binary, continuation, ping, close), unmasked frames out, and each message answered
   * with the header byte and {@code reply}'s ServerToAgent.
   */
  static final class WsServer implements Server {
    private final ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
    private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();
    private final Function<Opamp.AgentToServer, byte[]> reply;

    WsServer(Function<Opamp.AgentToServer, byte[]> reply) throws IOException {
      this.reply = reply;
      start(() -> {
        while (!listener.isClosed()) {
          try {
            Socket s = listener.accept();
            sockets.add(s);
            start(() -> serve(s));
          } catch (IOException e) {
            return; // closed
          }
        }
      });
    }

    @Override
    public int port() {
      return listener.getLocalPort();
    }

    @Override
    public void close() throws IOException {
      listener.close();
      for (Socket s : sockets) {
        s.close();
      }
    }

    private void serve(Socket s) {
      try (s) {
        DataInputStream in = new DataInputStream(new java.io.BufferedInputStream(s.getInputStream()));
        OutputStream out = s.getOutputStream();
        String key = null;
        for (String line = line(in); !line.isEmpty(); line = line(in)) {
          int colon = line.indexOf(':');
          if (colon > 0 && line.substring(0, colon).trim().equalsIgnoreCase("Sec-WebSocket-Key")) {
            key = line.substring(colon + 1).trim();
          }
        }
        String accept = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-1")
            .digest((Objects.requireNonNull(key) + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
                .getBytes(StandardCharsets.US_ASCII)));
        out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
        out.flush();
        ByteArrayOutputStream message = new ByteArrayOutputStream();
        while (true) {
          int b0 = in.read();
          if (b0 < 0) {
            return;
          }
          int b1 = in.readUnsignedByte();
          long len = b1 & 0x7f;
          if (len == 126) {
            len = in.readUnsignedShort();
          } else if (len == 127) {
            len = in.readLong();
          }
          byte[] mask = new byte[4];
          if ((b1 & 0x80) != 0) {
            in.readFully(mask);
          }
          byte[] payload = new byte[(int) len];
          in.readFully(payload);
          for (int i = 0; i < payload.length; i++) {
            payload[i] ^= mask[i % 4];
          }
          switch (b0 & 0x0f) {
            case 0, 2 -> {
              message.writeBytes(payload);
              if ((b0 & 0x80) != 0) {
                byte[] data = message.toByteArray();
                message.reset();
                // The OpAMP header (0: no compression), then the message.
                Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(
                    java.util.Arrays.copyOfRange(data, 1, data.length));
                byte[] answer = reply.apply(msg);
                byte[] framed = new byte[answer.length + 1];
                System.arraycopy(answer, 0, framed, 1, answer.length);
                frame(out, 2, framed);
              }
            }
            case 8 -> {
              frame(out, 8, payload); // answer the close, and the connection goes
              return;
            }
            case 9 -> frame(out, 10, payload);
            default -> {}
          }
        }
      } catch (Exception e) {
        // the connection went
      } finally {
        sockets.remove(s);
      }
    }

    private static String line(InputStream in) throws IOException {
      ByteArrayOutputStream b = new ByteArrayOutputStream();
      for (int c = in.read(); c != '\n'; c = in.read()) {
        if (c < 0) {
          throw new IOException("closed in the handshake");
        }
        if (c != '\r') {
          b.write(c);
        }
      }
      return b.toString(StandardCharsets.US_ASCII);
    }

    private static void frame(OutputStream out, int opcode, byte[] payload) throws IOException {
      ByteArrayOutputStream f = new ByteArrayOutputStream();
      f.write(0x80 | opcode);
      if (payload.length < 126) {
        f.write(payload.length);
      } else if (payload.length < 65536) {
        f.write(126);
        f.write(payload.length >>> 8);
        f.write(payload.length);
      } else {
        f.write(127);
        for (int i = 7; i >= 0; i--) {
          f.write((int) ((long) payload.length >>> (8 * i)));
        }
      }
      f.writeBytes(payload);
      synchronized (out) {
        out.write(f.toByteArray());
        out.flush();
      }
    }
  }
}
