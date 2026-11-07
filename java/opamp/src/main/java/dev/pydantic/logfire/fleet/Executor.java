// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet;

import com.google.protobuf.ByteString;
import dev.pydantic.logfire.fleet.core.CoreProto;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.WebSocket;
import java.net.http.WebSocketHandshakeException;
import java.nio.ByteBuffer;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.RejectedExecutionException;
import java.util.function.Consumer;

/**
 * Carries out the core's effects with the JDK's HTTP client (HTTP requests, WebSocket connections)
 * and the state file, and reports what happened as inputs, each naming the effect it
 * belongs to. It decides nothing about OpAMP and keeps no time of its own: every effect ends with
 * its result or the core's cancel. What is in flight, and so what reaches the core, is {@link
 * InFlight}'s. {@link #run} is called on the client's thread; the inputs arrive from the HTTP
 * client's threads (and the saver's) through {@code deliver}.
 */
final class Executor {
  // Headers the JDK sets itself and refuses from the caller.
  private static final Set<String> RESTRICTED = Set.of("connection", "content-length", "expect", "host", "upgrade");

  private final Transport transport;
  private final HttpClient http;
  /** Headers of the transport's own (the proxy's credentials) for every request. */
  private final List<Map.Entry<String, String>> extra;
  private final Consumer<CoreProto.Input> deliver;
  /**
   * The URL of the OpAMP request or connection whose outcome came last (null: none yet). Set before
   * the outcome reaches the core: a notification of it finds it in place.
   */
  volatile String used;
  private final Path stateFile;
  final InFlight inFlight = new InFlight();
  /** Saves, one at a time and in order, beside the client's thread: a slow disk holds up nothing. */
  private final ExecutorService saver;
  /** Where saves are written (journaled into the core tap, if there is one). */
  private final Disk disk;
  /**
   * Where response bodies are read, never the client's thread: a
   * continuation attached to a future already complete runs on the thread attaching it, so each
   * is attached with an executor of its own. Shut down when the client ends.
   */
  private final ExecutorService bodies;

  Executor(Transport transport, Consumer<CoreProto.Input> deliver, Path stateFile) {
    this(transport, deliver, stateFile, Disk.REAL);
  }

  Executor(Transport transport, Consumer<CoreProto.Input> deliver, Path stateFile, Disk disk) {
    this.disk = disk;
    this.transport = transport;
    this.http = transport.http;
    this.extra = transport.headers;
    this.deliver = deliver;
    this.stateFile = stateFile;
    this.bodies = Executors.newCachedThreadPool(r -> {
      Thread t = new Thread(r, "opamp-body");
      t.setDaemon(true);
      return t;
    });
    this.saver = stateFile == null ? null : Executors.newSingleThreadExecutor(r -> {
      Thread t = new Thread(r, "opamp-save");
      t.setDaemon(true);
      return t;
    });
  }

  void run(CoreProto.Effect effect) {
    switch (effect.getKindCase()) {
      case HTTP_REQUEST -> post(effect.getHttpRequest());
      case WS_OPEN -> open(effect.getWsOpen());
      case WS_SEND -> {
        if (inFlight.get(effect.getWsSend().getId()) instanceof Conn c) {
          c.send(effect.getWsSend().getData());
        }
      }
      case WS_CLOSE -> {
        if (inFlight.closing(effect.getWsClose().getId()) instanceof Conn c) {
          c.close();
        }
      }
      case STORE -> store(effect.getStore());
      // covers: RULE-CLIENT-15
      case CANCEL -> inFlight.cancel(effect.getCancel().getId());
      case NOTIFY -> {} // notifications are the client's
      // A newer module than this client, or what this client never asks the core for (a download):
      // fail loudly rather than skip what the core asked for.
      default -> throw new IllegalStateException("the core asked for an effect this client doesn't know: the client is older than its core");
    }
  }

  /** Whether {@code input} goes to the core ({@link InFlight#admit}). */
  boolean admit(CoreProto.Input input) {
    return inFlight.admit(input);
  }

  /**
   * The client has ended: everything in flight is aborted, and nothing starts. The future completes
   * when their tasks are over, a save already writing included (it finishes whole).
   */
  CompletableFuture<Void> close() {
    CompletableFuture<Void> over = inFlight.close();
    // Nothing still being sent or connected goes out after the client has ended.
    CompletableFuture<Void> released = transport.shutdown();
    // The bodies being read end with their aborts; a response that comes now is failed, unread.
    bodies.shutdown();
    if (saver != null) {
      saver.shutdown();
    }
    return CompletableFuture.allOf(over, released);
  }

  /**
   * Save, beside the client's thread. A save cancelled before its rename doesn't land ({@link
   * InFlight#whileInFlight}); one whose rename has begun lands, unreported. Answered stored only
   * once the rename is flushed (the directory too), store_failed if any step fails.
   */
  private void store(CoreProto.Store s) {
    long id = s.getId();
    CompletableFuture<Void> task = new CompletableFuture<>();
    if (!inFlight.add(id, () -> {}, task)) {
      return;
    }
    if (stateFile == null) {
      task.complete(null);
      deliver.accept(storeFailed(id, "no state file"));
      return;
    }
    byte[] data = s.getData().toByteArray();
    StateFile.begin(stateFile);
    try {
      saver.execute(() -> {
        try {
          if (StateFile.save(disk, stateFile, data, rename -> inFlight.whileInFlight(id, rename))) {
            deliver.accept(CoreProto.Input.newBuilder().setStored(CoreProto.Id.newBuilder().setId(id)).build());
          }
        } catch (IOException | RuntimeException e) {
          deliver.accept(storeFailed(id, String.valueOf(e.getMessage())));
        } finally {
          StateFile.end(stateFile);
          task.complete(null);
        }
      });
    } catch (RejectedExecutionException e) {
      StateFile.end(stateFile);
      task.complete(null);
      deliver.accept(storeFailed(id, "the client has stopped"));
    }
  }

  private static CoreProto.Input storeFailed(long id, String detail) {
    return CoreProto.Input.newBuilder().setStoreFailed(CoreProto.Failed.newBuilder().setId(id).setDetail(detail)).build();
  }

  private static void headers(List<CoreProto.Header> hs, Consumer<CoreProto.Header> set) {
    for (CoreProto.Header h : hs) {
      if (!RESTRICTED.contains(h.getName().toLowerCase(java.util.Locale.ROOT))) {
        set.accept(h);
      }
    }
  }

  private static List<CoreProto.Header> fromHeaders(HttpHeaders hs) {
    List<CoreProto.Header> out = new ArrayList<>();
    hs.map().forEach((name, values) -> values.forEach(
        v -> out.add(CoreProto.Header.newBuilder().setName(name).setValue(v).build())));
    return out;
  }

  /** A uint64 limit as a long: 2^63 or more (negative here) is as good as none. */
  static long limit(long uint64) {
    return uint64 < 0 ? Long.MAX_VALUE : uint64;
  }

  private static String message(Throwable e) {
    Throwable t = e instanceof CompletionException && e.getCause() != null ? e.getCause() : e;
    return t.getMessage() == null ? t.getClass().getSimpleName() : t.getMessage();
  }

  private static CoreProto.Input failedHttp(long id, String detail) {
    return CoreProto.Input.newBuilder()
        .setHttpFailed(CoreProto.Failed.newBuilder().setId(id).setDetail(detail))
        .build();
  }

  private void post(CoreProto.HttpRequest r) {
    long limit = limit(r.getMaxResponseBytes());
    Body body = new Body();
    CompletableFuture<HttpResponse<InputStream>> sent;
    try {
      // HTTP/1.1: on http:// the JDK's HTTP/2 default adds an h2c upgrade (Connection, Upgrade,
      // HTTP2-Settings) of its own to the POST.
      HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(r.getUrl()))
          .version(HttpClient.Version.HTTP_1_1)
          .POST(HttpRequest.BodyPublishers.ofByteArray(r.getBody().toByteArray()));
      if (r.getTimeoutMs() > 0) {
        b.timeout(Duration.ofMillis(r.getTimeoutMs()));
      }
      headers(r.getHeadersList(), h -> b.header(h.getName(), h.getValue()));
      extra.forEach(h -> b.header(h.getKey(), h.getValue()));
      sent = http.sendAsync(b.build(), HttpResponse.BodyHandlers.ofInputStream());
    } catch (IllegalArgumentException e) {
      sent = CompletableFuture.failedFuture(e);
    }
    CompletableFuture<HttpResponse<InputStream>> request = sent;
    CompletableFuture<Void> task = request
        .thenApplyAsync(resp -> {
          try (InputStream in = body.take(resp.body())) {
            byte[] data = in.readNBytes((int) Math.min(limit, Integer.MAX_VALUE - 9) + 1);
            if (data.length > limit) {
              return failedHttp(r.getId(), "response over " + limit + " bytes");
            }
            return CoreProto.Input.newBuilder()
                .setHttpResponse(CoreProto.HttpResponse.newBuilder()
                    .setId(r.getId())
                    .setStatus(resp.statusCode())
                    .addAllHeaders(fromHeaders(resp.headers()))
                    .setBody(ByteString.copyFrom(data))
                    .setUnixMs(System.currentTimeMillis()))
                .build();
          } catch (IOException e) {
            return failedHttp(r.getId(), message(e));
          }
        }, bodies)
        .exceptionally(e -> failedHttp(r.getId(), message(e)))
        .thenAccept(in -> {
          used = r.getUrl();
          deliver.accept(in);
        });
    inFlight.add(r.getId(), () -> {
      body.cancel();
      request.cancel(true);
    }, task);
  }

  /**
   * A response body being read: a cancel closes it, which ends the read (and the connection), and
   * one that comes after the cancel is closed at once.
   */
  private static final class Body {
    private InputStream in;
    private boolean cancelled;

    synchronized InputStream take(InputStream s) {
      in = s;
      if (cancelled) {
        closeQuietly(s);
      }
      return s;
    }

    synchronized void cancel() {
      cancelled = true;
      if (in != null) {
        closeQuietly(in);
      }
    }

    synchronized boolean cancelled() {
      return cancelled;
    }

    private static void closeQuietly(InputStream s) {
      try {
        s.close();
      } catch (IOException e) {
        // it is going anyway
      }
    }
  }

  private void open(CoreProto.WsOpen o) {
    long id = o.getId();
    Conn conn = new Conn(id, limit(o.getMaxMessageBytes()));
    conn.url = o.getUrl();
    // In flight before it starts: it may open before buildAsync returns.
    if (!inFlight.add(id, conn, null)) {
      return;
    }
    CompletableFuture<WebSocket> opening;
    try {
      WebSocket.Builder b = http.newWebSocketBuilder();
      headers(o.getHeadersList(), h -> b.header(h.getName(), h.getValue()));
      extra.forEach(h -> b.header(h.getKey(), h.getValue()));
      opening = b.buildAsync(URI.create(o.getUrl()), conn);
    } catch (IllegalArgumentException e) {
      opening = CompletableFuture.failedFuture(e);
    }
    conn.opening = opening;
    opening.whenComplete((ws, e) -> {
      if (e == null) {
        return; // onOpen reports it (or closes it, if it is no longer wanted)
      }
      CoreProto.WsFailed.Builder f = CoreProto.WsFailed.newBuilder()
          .setId(id)
          .setDetail(message(e))
          .setUnixMs(System.currentTimeMillis());
      Throwable cause = e instanceof CompletionException && e.getCause() != null ? e.getCause() : e;
      if (cause instanceof WebSocketHandshakeException h) {
        f.setStatus(h.getResponse().statusCode()).addAllHeaders(fromHeaders(h.getResponse().headers()));
      }
      used = o.getUrl();
      deliver.accept(CoreProto.Input.newBuilder().setWsFailed(f).build());
    });
  }

  /**
   * One WebSocket connection, from its opening to its end: its listener, and the sends and close
   * asked of it, in order. After {@code ws_close} it reports {@code ws_closed} when the closing
   * handshake is done (our Close sent, the server's received), or {@code ws_lost} if it fails.
   */
  /** 1006: a WebSocket that ended without a Close frame (RFC 6455 7.1.5), never a code sent. */
  static final int ABNORMAL_CLOSURE = 1006;

  private final class Conn implements WebSocket.Listener, InFlight.Work {
    private final long id;
    private final long maxMessageBytes;
    /** Where it goes. */
    private volatile String url;
    private final ByteArrayOutputStream partial = new ByteArrayOutputStream();
    private volatile CompletableFuture<WebSocket> opening;
    private volatile WebSocket ws;
    /** The core closed it: no more messages are passed on. */
    private volatile boolean closing;
    /** The server's Close frame came: its code. */
    private final CompletableFuture<Integer> peerClosed = new CompletableFuture<>();
    /** The core answered the server's Close (ws_close): the JDK may close its output then. */
    private final CompletableFuture<Void> answered = new CompletableFuture<>();
    /** The last send or close: the next waits for it (the JDK allows one outstanding send). */
    private CompletableFuture<?> last = CompletableFuture.completedFuture(null);
    /** Guards {@link #last} and {@link #over}: a send or close takes its place in the chain under it. */
    private final Object chain = new Object();
    /**
     * The connection is over (lost, aborted, or both Close frames gone): nothing more starts on it,
     * a send or Close still waiting in the chain included (it fails instead). A loss is reported,
     * and stop's abort done, only once this is set, and a send or Close starts under the lock: none
     * starts after stop has returned, nor after the core has heard the connection end.
     */
    private boolean over;

    Conn(long id, long maxMessageBytes) {
      this.id = id;
      this.maxMessageBytes = maxMessageBytes;
    }

    @Override
    public void onOpen(WebSocket webSocket) {
      ws = webSocket;
      // Opened after its cancel, or after the client ended: closed at once, never reported.
      if (!inFlight.attach(id, webSocket::abort)) {
        return;
      }
      used = url;
      deliver.accept(CoreProto.Input.newBuilder().setWsOpened(CoreProto.Id.newBuilder().setId(id)).build());
      webSocket.request(1);
    }

    @Override
    public CompletionStage<?> onBinary(WebSocket webSocket, ByteBuffer data, boolean lastPart) {
      if ((long) partial.size() + data.remaining() > maxMessageBytes) {
        // Too big: drop the connection (the spec's 1009, as a lost connection).
        lost("message over " + maxMessageBytes + " bytes");
        webSocket.abort();
        return null;
      }
      byte[] chunk = new byte[data.remaining()];
      data.get(chunk);
      partial.writeBytes(chunk);
      if (lastPart) {
        byte[] message = partial.toByteArray();
        partial.reset();
        if (!closing) {
          deliver.accept(CoreProto.Input.newBuilder()
              .setWsMessage(CoreProto.WsMessage.newBuilder().setId(id).setData(ByteString.copyFrom(message)))
              .build());
        }
      }
      webSocket.request(1);
      return null;
    }

    @Override
    public CompletionStage<?> onText(WebSocket webSocket, CharSequence data, boolean lastPart) {
      webSocket.request(1); // text messages mean nothing to OpAMP
      return null;
    }

    @Override
    public CompletionStage<?> onClose(WebSocket webSocket, int statusCode, String reason) {
      if (statusCode == ABNORMAL_CLOSURE) {
        // The JDK's word for a connection gone without a Close frame (1006 is never sent on the
        // wire): not the server's Close, a loss (RULE-SHELL-04).
        IOException gone = new IOException("the connection went without a Close frame (1006)");
        peerClosed.completeExceptionally(gone);
        lost(gone.getMessage());
        webSocket.abort();
        return null;
      }
      if (!closing) {
        // The server closes first: the core answers it (ws_close), and the JDK sends our Close
        // then, not before. Not the connection's end, even if the core closes it before it hears
        // of this: the end of its close follows.
        inFlight.serverClosed(id);
        deliver.accept(CoreProto.Input.newBuilder()
            .setWsClosed(CoreProto.WsClosed.newBuilder().setId(id).setCode(statusCode))
            .build());
      }
      peerClosed.complete(statusCode);
      return answered;
    }

    @Override
    public void onError(WebSocket webSocket, Throwable error) {
      peerClosed.completeExceptionally(error);
      lost(message(error));
    }

    /**
     * The connection is lost: reported, unless the server's Close, reported as ws_closed, was its
     * end already (until the core closes it: then the handshake's end is reported). A drop or a
     * failed send meanwhile is not a second end.
     */
    private void lost(String detail) {
      over();
      if (peerClosed.isDone() && !peerClosed.isCompletedExceptionally() && !closing) {
        WebSocket w = ws;
        if (w != null) {
          w.abort();
        }
        return;
      }
      deliver.accept(CoreProto.Input.newBuilder()
          .setWsLost(CoreProto.Failed.newBuilder().setId(id).setDetail(detail))
          .build());
    }

    /**
     * Send {@code data}, after anything sent before it. On the client's thread; the chain is taken
     * under its lock all the same, so that it holds from any thread (ConnLincheckTest).
     */
    void send(ByteString data) {
      WebSocket w = ws;
      synchronized (chain) {
        last = last.handle((v, e) -> null)
            .thenCompose(v -> start(() -> w.sendBinary(data.asReadOnlyByteBuffer(), true)))
            .whenComplete((v, e) -> {
              if (e != null) {
                lost(message(e));
                w.abort();
              }
            });
      }
    }

    /**
     * Our Close frame (or the answer to the server's), after anything sent before it; the
     * connection is closed once the server's Close has come too. No time limit of our own: the
     * core cancels a handshake that takes too long. On the client's thread (the chain under its
     * lock, as in send).
     */
    void close() {
      closing = true;
      WebSocket w = ws;
      CompletableFuture<?> sent;
      synchronized (chain) {
        sent = last.handle((v, e) -> null)
            .thenCompose(v -> start(() -> w.sendClose(WebSocket.NORMAL_CLOSURE, "")));
        last = sent;
      }
      sent.whenComplete((v, e) -> {
        answered.complete(null);
        if (e != null) {
          lost(message(e));
          w.abort();
        }
      });
      sent.thenCombine(peerClosed, (v, code) -> code).thenAccept(code -> {
        over();
        deliver.accept(CoreProto.Input.newBuilder()
            .setWsClosed(CoreProto.WsClosed.newBuilder().setId(id).setCode(code))
            .build());
        w.abort(); // both Close frames have gone: nothing is left to say on it
      });
    }

    /** Start {@code op} on the socket, unless the connection is over: then it fails, never started. */
    private CompletableFuture<WebSocket> start(java.util.function.Supplier<CompletableFuture<WebSocket>> op) {
      synchronized (chain) {
        return over ? CompletableFuture.failedFuture(new IOException("the connection is over")) : op.get();
      }
    }

    /** Nothing more starts on the connection. */
    private void over() {
      synchronized (chain) {
        over = true;
      }
    }

    /** Drop it now, whatever it is doing: opening, open or closing. */
    @Override
    public void abort() {
      over();
      CompletableFuture<WebSocket> o = opening;
      if (o != null) {
        o.cancel(true);
      }
      WebSocket w = ws;
      if (w != null) {
        w.abort();
      }
      answered.complete(null);
    }
  }
}
