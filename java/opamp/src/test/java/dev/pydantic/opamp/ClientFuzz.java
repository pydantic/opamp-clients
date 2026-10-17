// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicInteger;
import javax.net.ssl.SSLSession;
import opamp.proto.v1.Opamp;

/**
 * Jazzer target: the Java client's own code around the core, fed by a server whose every answer is
 * the fuzzer's and an application whose callback calls the client as the fuzzer says (as
 * python/tests/fuzz/fuzz_client.py does for Python). One input is one run of a real client on the
 * shipped core (the module compiled to JVM classes, through {@link Core}'s buffers), in either mode:
 *
 * <ul>
 *   <li>Java mode: the executor's HttpClient path over {@link FakeHttp} (bodies in pieces, resets,
 *       error statuses with headers, package downloads into the state file's directory; WebSocket
 *       upgrades taken or refused, messages, closes and drops);
 *   <li>Rust mode: the core's own HTTP over {@link Streams}, on {@link MemoryNet}, the server's
 *       bytes arriving in pieces (or any bytes at all, status line included), package files spliced
 *       or written by the streams' file writer.
 * </ul>
 *
 * <p>The server answers each request with any bytes, a ServerToAgent built from the bytes (remote
 * configs, packages with files it serves, connection settings, commands, custom messages, errors,
 * a new instance UID) and perhaps damaged, an error status, or a reset. Each notification makes the
 * callback call the client (statuses, remote config and package answers, custom messages, stop).
 * Then the application stops the client. What must hold, whatever came: the client never stops on
 * its own (the callback never throws, so an error is this shell's or the core's); callbacks one at
 * a time, never after Stopped; Stopped once, last; stop completes within its bound without an
 * error; every answer the server gave reaches the core unless the core gave up on it first (Java
 * mode, read from the core tap); install_package names a file under the state file's downloads directory, there as the
 * callback runs, whose SHA-256 is the offer's content hash when it has one; a heartbeat interval is
 * never negative; no temporary state file is left; nothing is started once stop has returned.
 *
 * <p>scripts/java_fuzz.sh runs it under Jazzer; {@link FuzzInputsTest} runs {@link #SEEDS} and every
 * input under src/test/resources/fuzz/ClientFuzz/ with every {@code mvn test}.
 */
public final class ClientFuzz {
  private ClientFuzz() {}

  static final long STOP_MS = 2000;
  static final long MARGIN_MS = 3000;
  /** How long the client may ask for nothing before the application stops it (it is backing off). */
  static final long IDLE_MS = 60;
  static final long RUN_MS = 5000;
  /** Java mode: the header that names each answer the server gave. */
  static final String TAG = "X-Answer";

  /** A property broken: the fuzzer's crash. */
  static final class Problem extends AssertionError {
    Problem(String m) {
      super(m);
    }
  }

  public static void fuzzerTestOneInput(byte[] data) throws Exception {
    Path tmp = Files.createTempDirectory("opamp-fuzz-");
    try {
      new Run(data, tmp).run();
    } finally {
      try (var walk = Files.walk(tmp)) {
        walk.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete());
      }
    }
  }

  /** A response with headers, its body read in pieces. */
  record Response(HttpRequest request, int statusCode, Map<String, List<String>> head, List<byte[]> pieces)
      implements HttpResponse<InputStream> {
    @Override
    public Optional<HttpResponse<InputStream>> previousResponse() {
      return Optional.empty();
    }

    @Override
    public HttpHeaders headers() {
      return HttpHeaders.of(head, (a, b) -> true);
    }

    @Override
    public InputStream body() {
      return new InputStream() {
        private int piece;
        private int at;

        @Override
        public int read() throws IOException {
          byte[] b = new byte[1];
          return read(b, 0, 1) < 0 ? -1 : b[0] & 0xff;
        }

        @Override
        public int read(byte[] b, int off, int len) {
          while (piece < pieces.size() && at == pieces.get(piece).length) {
            piece++;
            at = 0;
          }
          if (piece == pieces.size()) {
            return -1;
          }
          if (len == 0) {
            return 0;
          }
          int n = Math.min(len, pieces.get(piece).length - at);
          System.arraycopy(pieces.get(piece), at, b, off, n);
          at += n;
          return n;
        }
      };
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

  static final class Run {
    final FuzzInput.Reader r;
    final FuzzInput.Server server;
    final boolean rust;
    final Path state;
    final OpampClient.Config config;
    final FakeHttp http = new FakeHttp();
    final MemoryNet net = new MemoryNet();
    final List<Notification> notes = new CopyOnWriteArrayList<>();
    final Map<Class<?>, Notification> seen = new HashMap<>();
    final List<String> problems = new CopyOnWriteArrayList<>();
    final AtomicInteger active = new AtomicInteger();
    final CompletableFuture<OpampClient> client = new CompletableFuture<>();
    volatile CompletableFuture<Void> stopped;
    volatile long lastAsk = System.nanoTime();
    final List<Thread> servers = new CopyOnWriteArrayList<>();
    /** The core tap: every step, in and out. */
    final Path tap;
    /**
     * Java mode: the tag (X-Answer) of each OpAMP request's answer, with the body it answered, and
     * of each upgrade's, in the order the shell asked (empty: no answer to tag). Each must reach the core, unless it gave up on
     * the effect first.
     */
    final Map<String, byte[]> postTags = new java.util.concurrent.ConcurrentHashMap<>();
    final List<String> openTags = new CopyOnWriteArrayList<>();
    final java.util.concurrent.atomic.AtomicInteger tags = new java.util.concurrent.atomic.AtomicInteger();

    Run(byte[] data, Path tmp) {
      r = new FuzzInput.Reader(data);
      int flags = r.u8();
      rust = (flags & 1) != 0;
      int more = r.u8();
      server = new FuzzInput.Server(r);
      state = tmp.resolve("state");
      tap = tmp.resolve("tap");
      String endpoint = (more & 2) != 0 ? "ws://opamp.test/v1/opamp" : "http://opamp.test/v1/opamp";
      OpampClient.Config.Builder b = ((flags & 2) != 0 ? OpampClient.Config.builder(endpoint, new byte[16])
          : OpampClient.Config.builder(endpoint))
          .capabilities(FuzzInput.CAPABILITIES)
          .heartbeatInterval(Duration.ofMillis(new long[] {1, 5, 30_000, 0}[flags >> 2 & 3]))
          .requestTimeout(Duration.ofMillis((flags & 16) != 0 ? 1000 : 0))
          .stateFile(state).acceptPackages(true)
          .acceptConnectionSettings((flags & 32) != 0).requestInstanceUid((flags & 64) != 0)
          .downloadTimeout(Duration.ofSeconds(5)).maxPackageBytes((flags & 128) != 0 ? 1000 : 0)
          .stopTimeout(Duration.ofMillis(STOP_MS));
      if (rust) {
        b.mode(OpampClient.Mode.RUST).streamsPlatform(net).directConnection().rustSplice((more & 1) != 0);
      } else {
        b.httpClient(http);
      }
      config = b.build();
    }

    void problem(String what) {
      problems.add(what);
    }

    void notify(Notification n) {
      if (active.incrementAndGet() > 1) {
        problem("two callbacks at once: " + n);
      }
      try {
        for (Notification m : notes) {
          if (m instanceof Notification.Stopped) {
            problem("a callback after stopped: " + n);
          }
        }
        notes.add(n);
        seen.put(n.getClass(), n);
        if (n instanceof Notification.Stopped s && s.error() != null) {
          problem("the client stopped on its own: " + s.error());
        } else if (n instanceof Notification.HeartbeatInterval h && h.interval().isNegative()) {
          problem("heartbeat interval " + h.interval());
        } else if (n instanceof Notification.InstallPackage ip) {
          checkPackage(ip);
        }
        if (n instanceof Notification.Stopped) {
          return;
        }
        OpampClient c = client.join();
        for (int k = r.u8() % 4; k > 0; k--) {
          if (FuzzInput.call(r, c, r.pick(FuzzInput.CALLS), seen) && stopped == null) {
            stopped = c.stop();
          }
        }
      } finally {
        active.decrementAndGet();
      }
    }

    void checkPackage(Notification.InstallPackage ip) {
      Path dir = Executor.downloadsDir(state);
      if (ip.path() == null || !dir.equals(ip.path().getParent())) {
        problem("install_package " + ip.name() + ": " + ip.path() + " is not in " + dir);
        return;
      }
      byte[] got;
      try {
        got = Files.readAllBytes(ip.path());
      } catch (IOException e) {
        problem("install_package " + ip.name() + ": its file can't be read: " + e);
        return;
      }
      byte[] want = ip.pkg().getFile().getContentHash().toByteArray();
      if (want.length > 0 && !java.util.Arrays.equals(FuzzInput.sha256(got), want)) {
        problem("install_package " + ip.name() + ": the file isn't the one offered (" + got.length + " bytes)");
      }
    }

    /** Java mode: answer what the shell sends, as the server says. */
    static Map<String, List<String>> head(List<String[]> headers) {
      Map<String, List<String>> head = new HashMap<>();
      for (String[] h : headers) {
        head.computeIfAbsent(h[0], k -> new ArrayList<>()).add(h[1]);
      }
      return head;
    }

    void serveHttp(FakeHttp.Call call) {
      lastAsk = System.nanoTime();
      if (call instanceof FakeHttp.Open o) {
        FuzzInput.Ws u = server.upgrade();
        if (u.how().equals("reset")) {
          openTags.add("");
          o.fail("connection reset by the server");
        } else if (u.how().equals("status")) {
          String tag = Integer.toString(tags.incrementAndGet());
          openTags.add(tag);
          List<String[]> hs = new ArrayList<>(u.headers());
          hs.add(new String[] {TAG, tag});
          HttpRequest req = HttpRequest.newBuilder(java.net.URI.create(o.uri.toString().replaceFirst("^ws", "http"))).build();
          o.future.completeExceptionally(new java.net.http.WebSocketHandshakeException(
              new Response(req, u.status(), head(hs), List.of())));
        } else {
          openTags.add("");
          o.open();
        }
        return;
      }
      FakeHttp.Request q = (FakeHttp.Request) call;
      boolean post = q.request.method().equals("POST");
      FuzzInput.Answer a = post ? server.answer(q.body) : server.download(q.request.uri().toString());
      if (!a.how().equals("ok")) {
        q.fail("connection reset by the server");
        return;
      }
      List<String[]> hs = new ArrayList<>(a.headers());
      if (post) {
        String tag = Integer.toString(tags.incrementAndGet());
        postTags.put(tag, q.body);
        hs.add(new String[] {TAG, tag});
      }
      q.future.complete(new Response(q.request, a.status(), head(hs), FuzzInput.pieces(a.data(), a.cuts())));
    }

    /** Java mode: the network takes each WebSocket write the shell made, and the server answers it. */
    void serveWire() {
      for (Object o; (o = http.wire.poll()) != null; ) {
        FakeHttp.FakeWs w = (FakeHttp.FakeWs) o;
        FakeHttp.FakeWs.Write done = w.completeNext(true);
        if (done == null || done.close()) {
          continue;
        }
        lastAsk = System.nanoTime();
        FuzzInput.Ws a = server.wsAnswer(done.data());
        switch (a.how()) {
          case "drop" -> w.drop("reset by the server");
          case "close" -> w.serverClose(a.code() == 0 ? 1005 : a.code());
          default -> w.receive(a.data());
        }
      }
    }

    /** One turn of the server: whatever the shell asked for, answered. */
    void serve(long waitMs) throws InterruptedException {
      if (rust) {
        MemoryNet.Dial d = net.dials.poll(waitMs, TimeUnit.MILLISECONDS);
        if (d != null) {
          lastAsk = System.nanoTime();
          serveConn(d);
        }
        return;
      }
      FakeHttp.Call call = http.calls.poll(waitMs, TimeUnit.MILLISECONDS);
      if (call != null) {
        serveHttp(call);
      }
      serveWire();
    }

    /** Rust mode: one connection's server, reading requests and writing answers, in pieces. */
    void serveConn(MemoryNet.Dial d) {
      MemoryNet.End end = d.accept();
      String host = d.host;
      Thread t = new Thread(() -> {
        try {
          InputStream in = end.input();
          OutputStream out = end.output();
          while (true) {
            ByteArrayOutputStream head = new ByteArrayOutputStream();
            int c;
            while ((c = in.read()) >= 0) {
              head.write(c);
              byte[] h = head.toByteArray();
              int n = h.length;
              if (n >= 4 && h[n - 4] == '\r' && h[n - 3] == '\n' && h[n - 2] == '\r' && h[n - 1] == '\n') {
                break;
              }
            }
            if (c < 0) {
              return;
            }
            String[] lines = head.toString(StandardCharsets.ISO_8859_1).split("\r\n");
            int length = 0;
            for (String line : lines) {
              int colon = line.indexOf(':');
              if (colon > 0 && line.substring(0, colon).trim().equalsIgnoreCase("content-length")) {
                length = Integer.parseInt(line.substring(colon + 1).trim());
              }
            }
            byte[] body = in.readNBytes(length);
            lastAsk = System.nanoTime();
            String key = null;
            boolean upgrade = false;
            for (String line : lines) {
              int colon = line.indexOf(':');
              if (colon > 0 && line.substring(0, colon).trim().equalsIgnoreCase("upgrade")) {
                upgrade = line.substring(colon + 1).trim().equalsIgnoreCase("websocket");
              }
              if (colon > 0 && line.substring(0, colon).trim().equalsIgnoreCase("sec-websocket-key")) {
                key = line.substring(colon + 1).trim();
              }
            }
            if (upgrade) {
              FuzzInput.Ws u = server.upgrade();
              if (u.how().equals("reset")) {
                end.abort();
                return;
              }
              if (u.how().equals("status")) {
                out.write(head(u.status(), u.headers(), 0));
                continue;
              }
              String accept = java.util.Base64.getEncoder().encodeToString(java.security.MessageDigest.getInstance("SHA-1")
                  .digest((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").getBytes(StandardCharsets.US_ASCII)));
              out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                  + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
              frames(in, out, end);
              return;
            }
            String[] first = lines[0].split(" ");
            FuzzInput.Answer a = lines[0].startsWith("POST") ? server.answer(body)
                : server.download("http://" + host + (first.length > 1 ? first[1] : "/"));
            if (a.how().equals("reset")) {
              end.abort();
              return;
            }
            byte[] raw;
            List<Integer> cuts = new ArrayList<>();
            if (a.how().equals("garbage")) {
              raw = a.data();
            } else {
              byte[] top = head(a.status(), a.headers(), a.data().length);
              raw = java.util.Arrays.copyOf(top, top.length + a.data().length);
              System.arraycopy(a.data(), 0, raw, top.length, a.data().length);
              for (int at : a.cuts()) {
                cuts.add(at + top.length);
              }
            }
            for (byte[] p : FuzzInput.pieces(raw, cuts)) {
              out.write(p);
              Thread.yield();
            }
            if (a.how().equals("garbage")) {
              end.close();
              return;
            }
          }
        } catch (IOException | RuntimeException | java.security.NoSuchAlgorithmException e) {
          end.abort();
        }
      }, "fuzz-server");
      t.setDaemon(true);
      servers.add(t);
      t.start();
    }

    static byte[] head(int status, List<String[]> headers, int length) {
      StringBuilder s = new StringBuilder("HTTP/1.1 " + status + " X\r\nContent-Length: " + length + "\r\n");
      for (String[] h : headers) {
        s.append(h[0]).append(": ").append(h[1]).append("\r\n");
      }
      return s.append("\r\n").toString().getBytes(StandardCharsets.ISO_8859_1);
    }

    static byte[] frame(int op, byte[] payload) {
      int n = payload.length;
      java.io.ByteArrayOutputStream f = new java.io.ByteArrayOutputStream();
      f.write(0x80 | op);
      if (n < 126) {
        f.write(n);
      } else if (n < 65536) {
        f.write(126);
        f.write(n >> 8);
        f.write(n);
      } else {
        f.write(127);
        for (int i = 7; i >= 0; i--) {
          f.write(i >= 4 ? 0 : n >> (8 * i));
        }
      }
      f.writeBytes(payload);
      return f.toByteArray();
    }

    /** Rust mode: an upgraded connection's frames from the client (masked), each answered. */
    void frames(InputStream in, OutputStream out, MemoryNet.End end) throws IOException {
      ByteArrayOutputStream message = new ByteArrayOutputStream();
      while (true) {
        int b0 = in.read();
        int b1 = in.read();
        if (b0 < 0 || b1 < 0) {
          return;
        }
        long n = b1 & 0x7f;
        if (n == 126) {
          n = (in.read() << 8) | in.read();
        } else if (n == 127) {
          n = 0;
          for (int i = 0; i < 8; i++) {
            n = n << 8 | in.read();
          }
        }
        byte[] mask = (b1 & 0x80) != 0 ? in.readNBytes(4) : new byte[4];
        if (n < 0 || n > 1 << 24) {
          end.abort();
          return;
        }
        byte[] payload = in.readNBytes((int) n);
        for (int i = 0; i < payload.length; i++) {
          payload[i] ^= mask[i % 4];
        }
        int op = b0 & 0x0f;
        if (op == 8) { // the client's Close: answered, and the connection closed
          out.write(frame(8, java.util.Arrays.copyOf(payload, Math.min(2, payload.length))));
          end.close();
          return;
        }
        if (op == 9) {
          out.write(frame(10, payload));
          continue;
        }
        if (op != 0 && op != 2) {
          continue;
        }
        message.writeBytes(payload);
        if ((b0 & 0x80) == 0) {
          continue;
        }
        lastAsk = System.nanoTime();
        FuzzInput.Ws a = server.wsAnswer(message.toByteArray());
        message.reset();
        switch (a.how()) {
          case "drop" -> {
            end.abort();
            return;
          }
          case "close" -> out.write(frame(8, a.code() == 0 ? new byte[0] : new byte[] {(byte) (a.code() >> 8), (byte) a.code()}));
          case "raw" -> out.write(a.data()); // any bytes: frames or not
          default -> out.write(frame(2, a.data()));
        }
      }
    }

    /** The core tap's records: (kind, bytes). */
    List<Map.Entry<Integer, byte[]>> tapped() throws IOException {
      List<Map.Entry<Integer, byte[]>> out = new ArrayList<>();
      // The process's first tap is at the path given, each later one beside it (path.PID.N).
      Path file;
      try (java.util.stream.Stream<Path> all = Files.list(tap.getParent())) {
        file = all.filter(f -> f.getFileName().toString().startsWith(tap.getFileName().toString())).findFirst().orElse(null);
      }
      if (file == null) {
        return out;
      }
      java.nio.ByteBuffer b = java.nio.ByteBuffer.wrap(Files.readAllBytes(file)).order(java.nio.ByteOrder.LITTLE_ENDIAN);
      while (b.remaining() >= 13) {
        int kind = b.get();
        int len = b.getInt();
        b.getLong();
        byte[] rec = new byte[len];
        b.get(rec);
        out.add(Map.entry(kind, rec));
      }
      return out;
    }

    /**
     * Java mode: every answer the server gave reached the core, unless the core gave up on its
     * effect first. A request is the http_request with its body; the k-th upgrade the shell asked
     * for is the core's k-th ws_open (checked only when the shell asked for each).
     */
    void checkAnswers() throws IOException {
      Map<String, List<Long>> posts = new HashMap<>();
      List<Long> opens = new ArrayList<>();
      java.util.Set<Long> cancelled = new java.util.HashSet<>();
      java.util.Set<String> reached = new java.util.HashSet<>();
      for (Map.Entry<Integer, byte[]> rec : tapped()) {
        if (rec.getKey() == 4 || rec.getKey() == 2) {
          for (dev.pydantic.opamp.core.CoreProto.Effect e : dev.pydantic.opamp.core.CoreProto.Reply.parseFrom(rec.getValue())
              .getOutput().getEffectsList()) {
            switch (e.getKindCase()) {
              case HTTP_REQUEST -> posts.computeIfAbsent(java.util.HexFormat.of().formatHex(e.getHttpRequest().getBody().toByteArray()),
                  k -> new ArrayList<>()).add(e.getHttpRequest().getId());
              case WS_OPEN -> opens.add(e.getWsOpen().getId());
              case CANCEL -> cancelled.add(e.getCancel().getId());
              default -> {}
            }
          }
        } else if (rec.getKey() == 3) {
          dev.pydantic.opamp.core.CoreProto.Input in = dev.pydantic.opamp.core.CoreProto.Step.parseFrom(rec.getValue()).getInput();
          List<dev.pydantic.opamp.core.CoreProto.Header> hs = in.hasHttpResponse() ? in.getHttpResponse().getHeadersList()
              : in.hasWsFailed() ? in.getWsFailed().getHeadersList() : List.of();
          for (dev.pydantic.opamp.core.CoreProto.Header h : hs) {
            if (h.getName().equalsIgnoreCase(TAG)) {
              reached.add(h.getValue());
            }
          }
        }
      }
      List<String> lost = new ArrayList<>();
      for (Map.Entry<String, byte[]> t : postTags.entrySet()) {
        List<Long> ids = posts.getOrDefault(java.util.HexFormat.of().formatHex(t.getValue()), List.of());
        if (!reached.contains(t.getKey()) && !ids.isEmpty() && ids.stream().noneMatch(cancelled::contains)) {
          lost.add("request " + ids);
        }
      }
      for (int i = 0; i < openTags.size() && openTags.size() == opens.size(); i++) {
        if (!openTags.get(i).isEmpty() && !reached.contains(openTags.get(i)) && !cancelled.contains(opens.get(i))) {
          lost.add("upgrade " + opens.get(i));
        }
      }
      if (!lost.isEmpty()) {
        problem("answers the server gave never reached the core: " + lost);
      }
    }

    void run() throws Exception {
      OpampClient c = OpampClient.start(config, new OpampClient.Status(Opamp.AgentDescription.getDefaultInstance(), null,
          null, null, null, Opamp.CustomCapabilities.newBuilder().addCapabilities(FuzzInput.CAPABILITY).build()), this::notify,
          null, tap.toString(), d -> d);
      client.complete(c);
      long begun = System.nanoTime();
      while (!r.empty() && stopped == null && System.nanoTime() - begun < RUN_MS * 1_000_000) {
        serve(5);
        if (System.nanoTime() - lastAsk > IDLE_MS * 1_000_000) {
          break;
        }
      }
      if (stopped == null) {
        stopped = c.stop();
      }
      // What the shell asks for while stopping is answered too, as the server would.
      long asked = System.nanoTime();
      Thread pump = new Thread(() -> {
        try {
          while (!stopped.isDone()) {
            serve(5);
          }
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
        }
      }, "fuzz-stop-server");
      pump.setDaemon(true);
      pump.start();
      try {
        stopped.get(STOP_MS + MARGIN_MS, TimeUnit.MILLISECONDS);
      } catch (TimeoutException e) {
        problem("stop took over " + (STOP_MS + MARGIN_MS) + " ms");
      } catch (ExecutionException e) {
        problem("stop completed with " + e.getCause());
      }
      pump.join(1000);
      http.seal("stop returned");
      long took = (System.nanoTime() - asked) / 1_000_000;
      if (took > STOP_MS + MARGIN_MS) {
        problem("stop took " + took + " ms");
      }
      long stops = notes.stream().filter(n -> n instanceof Notification.Stopped).count();
      if (stops != 1 || !(notes.get(notes.size() - 1) instanceof Notification.Stopped)) {
        problem("stopped told " + stops + " times, last " + (notes.isEmpty() ? null : notes.get(notes.size() - 1)));
      }
      if (Files.exists(StateFile.tmp(state))) {
        problem("a temporary state file left behind");
      }
      problems.addAll(http.violations);
      if (!rust) {
        checkAnswers();
      }
      for (Thread t : servers) {
        t.interrupt();
      }
      if (!problems.isEmpty()) {
        throw new Problem((rust ? "Rust" : "Java") + " mode, " + server.answers + " answers: " + String.join("; ", problems));
      }
    }
  }

  private static byte[] seed(int... b) {
    byte[] out = new byte[b.length];
    for (int i = 0; i < b.length; i++) {
      out[i] = (byte) b[i];
    }
    return out;
  }

  /** Inputs that reach the deeper paths at once (python/tests/fuzz/fuzz_client.py's, read alike). */
  static final List<byte[]> SEEDS = List.of(
      seed(0),
      seed(1, 0),
      // Java mode, 1 ms heartbeats: a remote config, answered from the callback.
      seed(0, 0, 5, 2, 0, 2, 'a', 'b', 1, 0, 4, 'c', 'o', 'n', 'f', 0, 0, 1, 3, 1),
      // Rust mode: a package offered, its file served.
      seed(1, 0, 1, 5, 4, 0, 0, 1, 0, 0, 1, '1', 2, 'h', '1', 1, 0, 5, 'h', 'e', 'l', 'l', 'o', 0, 0, 0, 0, 0, 0, 1, 4, 1, 1, 0),
      // Connection settings offered, with a new endpoint.
      seed(0x24, 0, 6, 8, 0, 2, 'c', 's', 1, 1, 1, 0, 0, 0, 0),
      // Raw bytes, a reset, a damaged message, an error status with Retry-After.
      seed(4, 0, 1, 5, 0x0a, 3, 'a', 'b', 'c', 2, 9, 0xff, 3, 3, 7, 1, 1),
      seed(5, 0, 1, 4, 16, 'H', 'T', 'T', 'P', '/', '1', '.', '1', ' ', '2', '0', '0', ' ', 'O', 'K', '\r', '\n', 1, 3, 0, 0xff),
      // A WebSocket endpoint, in each mode: the upgrade taken, a remote config over it, then refused.
      seed(0, 2, 0, 5, 2, 0, 2, 'a', 'b', 1, 0, 4, 'c', 'o', 'n', 'f', 0, 0, 1, 3, 1, 1, 6),
      seed(1, 2, 0, 5, 2, 0, 2, 'a', 'b', 1, 0, 4, 'c', 'o', 'n', 'f', 0, 0, 1, 3, 1, 1, 6));
}
