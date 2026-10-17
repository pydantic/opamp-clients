// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.Authenticator;
import java.net.CookieHandler;
import java.net.ProxySelector;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.WebSocket;
import java.nio.ByteBuffer;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.Flow;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.atomic.AtomicInteger;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLParameters;
import javax.net.ssl.SSLSession;

/**
 * A {@link HttpClient} with no network under it, for schedule exploration: every request, download
 * and WebSocket the shell starts becomes a {@link Call} on {@link #calls}, and the test's own
 * threads (its "network") answer it, fail it, hold it or answer it late, as the plan says. The
 * client takes it as the application's own client ({@link OpampClient.Config.Builder#httpClient}),
 * so production code is the same as with the JDK's. No sockets: under Fray, only threads.
 *
 * <p>It also checks what the JDK would refuse or what the shell must never do, as violations:
 * a binary send while another is outstanding (the JDK fails it), a send after our Close, a second
 * Close, and anything started after {@link #seal} (stop has returned: RULE-SHELL-07).
 */
final class FakeHttp extends HttpClient {
  /** What the shell started, in order, for the network to take. */
  final LinkedBlockingQueue<Call> calls = new LinkedBlockingQueue<>();
  final List<Call> all = new CopyOnWriteArrayList<>();
  final List<FakeWs> sockets = new CopyOnWriteArrayList<>();
  /** A socket with a new write outstanding, once per write: for the network's side to complete. */
  final LinkedBlockingQueue<Object> wire = new LinkedBlockingQueue<>();
  /** Completed by a network that sees a download asked for (its own choice: a test's signal). */
  final CompletableFuture<Void> downloadAsked = new CompletableFuture<>();
  final List<String> violations = new CopyOnWriteArrayList<>();
  /**
   * Called with each request or download as it is sent, before {@code sendAsync} returns: a test
   * may complete it there, so that the shell attaches its continuation to a response already
   * complete.
   */
  volatile java.util.function.Consumer<Request> atSend = r -> {};
  /** Why nothing may start any more (stop has returned), or null. */
  private volatile String sealed;
  private final AtomicInteger next = new AtomicInteger();

  /** Nothing may start from now on: stop has returned. */
  void seal(String why) {
    sealed = why;
  }

  void violation(String what) {
    violations.add(what);
  }

  private void starting(String what) {
    String s = sealed;
    if (s != null) {
      violation("RULE-SHELL-07: " + what + " started after " + s);
    }
  }

  /** Something the shell started. */
  abstract static sealed class Call permits Request, Open {
    final int n;

    Call(int n) {
      this.n = n;
    }
  }

  /** A request or a download: its future is the response the shell waits for. */
  static final class Request extends Call {
    final HttpRequest request;
    final byte[] body;
    final CompletableFuture<HttpResponse<InputStream>> future = new CompletableFuture<>();
    /** The client it was sent with (null: a test's own sentinel). */
    FakeHttp owner;

    Request(int n, HttpRequest request, byte[] body) {
      super(n);
      this.request = request;
      this.body = body;
    }

    /** Answer it (no-op if the shell cancelled it). */
    boolean answer(int status, byte[] body) {
      return answer(status, body, false);
    }

    /**
     * Answer it; {@code slow}: the body comes a few bytes per read, with a yield before each (a
     * point where another thread may run), and a read after the stream is closed fails, as the JDK's.
     */
    boolean answer(int status, byte[] body, boolean slow) {
      return future.complete(new Response(request, status, body, slow, owner));
    }

    boolean fail(String why) {
      return future.completeExceptionally(new IOException(why));
    }

    @Override
    public String toString() {
      return "#" + n + " " + request.method() + " " + request.uri();
    }
  }

  /** A WebSocket being opened: the network opens it ({@link #open}), fails it, or holds it. */
  final class Open extends Call {
    final URI uri;
    final WebSocket.Listener listener;
    final CompletableFuture<WebSocket> future = new CompletableFuture<>();
    volatile FakeWs ws;

    Open(int n, URI uri, WebSocket.Listener listener) {
      super(n);
      this.uri = uri;
      this.listener = listener;
    }

    /** It opens: the listener's onOpen, then the future (the JDK's order). The socket, or null if cancelled first. */
    FakeWs open() {
      FakeWs w = new FakeWs(this);
      ws = w;
      sockets.add(w);
      listener.onOpen(w);
      future.complete(w);
      return w;
    }

    boolean fail(String why) {
      return future.completeExceptionally(new IOException(why));
    }

    @Override
    public String toString() {
      return "#" + n + " WS " + uri;
    }
  }

  /**
   * One fake WebSocket. The shell's sends wait on {@link #writes}, which the network completes in
   * order ({@link #completeWrite}); the server's side calls the listener ({@link #receive}, {@link
   * #serverClose}, {@link #drop}).
   */
  final class FakeWs implements WebSocket {
    final Open open;
    /** The shell's sends and Close, outstanding, in order: each completed by the network. */
    final java.util.concurrent.ConcurrentLinkedQueue<Write> writes = new java.util.concurrent.ConcurrentLinkedQueue<>();
    final List<byte[]> sent = new CopyOnWriteArrayList<>();
    private Write pendingBinary;
    private boolean closeSent;
    private volatile boolean aborted;
    private volatile boolean peerClosed;
    /** Guards the sends' state (synchronized blocks, not methods: Lincheck hangs on those nested). */
    private final Object state = new Object();
    /** The listener is called one at a time, as by the JDK. */
    private final Object listenerLock = new Object();

    FakeWs(Open open) {
      this.open = open;
    }

    record Write(boolean close, byte[] data, CompletableFuture<WebSocket> done) {}

    @Override
    public CompletableFuture<WebSocket> sendText(CharSequence data, boolean last) {
      violation("a text send: OpAMP sends binary");
      return CompletableFuture.completedFuture(this);
    }

    @Override
    public CompletableFuture<WebSocket> sendBinary(ByteBuffer data, boolean last) {
      synchronized (state) {
        return binary(data);
      }
    }

    private CompletableFuture<WebSocket> binary(ByteBuffer data) {
      starting("a WebSocket send");
      if (aborted) {
        return CompletableFuture.failedFuture(new IOException("aborted"));
      }
      if (closeSent) {
        violation("a send after our Close on " + open);
        return CompletableFuture.failedFuture(new IOException("output closed"));
      }
      if (pendingBinary != null && !pendingBinary.done.isDone()) {
        // The JDK allows one outstanding binary send: this one would fail.
        violation("a send while another is outstanding on " + open);
        return CompletableFuture.failedFuture(new IllegalStateException("send pending"));
      }
      byte[] b = new byte[data.remaining()];
      data.get(b);
      Write w = new Write(false, b, new CompletableFuture<>());
      pendingBinary = w;
      writes.add(w);
      wire.add(this);
      return w.done;
    }

    @Override
    public CompletableFuture<WebSocket> sendClose(int statusCode, String reason) {
      synchronized (state) {
        return close();
      }
    }

    private CompletableFuture<WebSocket> close() {
      starting("a WebSocket Close");
      if (aborted) {
        return CompletableFuture.failedFuture(new IOException("aborted"));
      }
      if (closeSent) {
        violation("a second Close on " + open);
        return CompletableFuture.failedFuture(new IOException("output closed"));
      }
      if (pendingBinary != null && !pendingBinary.done.isDone()) {
        violation("our Close before the send it follows completed, on " + open);
      }
      closeSent = true;
      Write w = new Write(true, null, new CompletableFuture<>());
      writes.add(w);
      wire.add(this);
      return w.done;
    }

    @Override
    public CompletableFuture<WebSocket> sendPing(ByteBuffer message) {
      return CompletableFuture.completedFuture(this);
    }

    @Override
    public CompletableFuture<WebSocket> sendPong(ByteBuffer message) {
      return CompletableFuture.completedFuture(this);
    }

    @Override
    public void request(long n) {}

    @Override
    public String getSubprotocol() {
      return "";
    }

    @Override
    public boolean isOutputClosed() {
      synchronized (state) {
        return closeSent || aborted;
      }
    }

    @Override
    public boolean isInputClosed() {
      return peerClosed || aborted;
    }

    @Override
    public void abort() {
      List<Write> left;
      synchronized (state) {
        if (aborted) {
          return;
        }
        aborted = true;
        left = new ArrayList<>(writes);
        writes.clear();
      }
      for (Write w : left) {
        w.done.completeExceptionally(new IOException("aborted"));
      }
    }

    boolean aborted() {
      return aborted;
    }

    boolean closeSent() {
      synchronized (state) {
        return closeSent;
      }
    }

    /** Over: aborted, or both Close frames gone. */
    boolean ended() {
      return aborted || (closeSent() && peerClosed);
    }

    /**
     * The network completes the oldest write: the data has gone (a send), or our Close has (then
     * the server's Close answers it, if {@code answerClose}). False if there is none or it is aborted.
     */
    boolean completeWrite(boolean answerClose) {
      return completeNext(answerClose) != null;
    }

    /** {@link #completeWrite}, giving the write completed (null if none). */
    Write completeNext(boolean answerClose) {
      Write w = writes.poll();
      if (w == null || aborted) {
        return null;
      }
      if (!w.close) {
        sent.add(w.data);
      }
      w.done.complete(this);
      if (w.close && answerClose) {
        serverClose(NORMAL_CLOSURE);
      }
      return w;
    }

    /** A message from the server, as the listener gets it (unless aborted or closed by the server). */
    void receive(byte[] message) {
      synchronized (listenerLock) {
        if (aborted || peerClosed) {
          return;
        }
        open.listener.onBinary(this, ByteBuffer.wrap(message), true);
      }
    }

    /** The server's Close frame. */
    void serverClose(int code) {
      synchronized (listenerLock) {
        if (aborted || peerClosed) {
          return;
        }
        peerClosed = true;
        open.listener.onClose(this, code, "");
      }
    }

    /** The connection drops. */
    void drop(String why) {
      synchronized (listenerLock) {
        if (aborted || peerClosed) {
          return;
        }
        peerClosed = true;
        open.listener.onError(this, new IOException(why));
      }
      abort();
    }
  }

  // The HttpClient.

  @Override
  public <T> CompletableFuture<HttpResponse<T>> sendAsync(HttpRequest request, HttpResponse.BodyHandler<T> handler) {
    starting("a request to " + request.uri());
    Request r = new Request(next.incrementAndGet(), request, body(request));
    r.owner = this;
    all.add(r);
    calls.add(r);
    atSend.accept(r);
    @SuppressWarnings("unchecked")
    CompletableFuture<HttpResponse<T>> f = (CompletableFuture<HttpResponse<T>>) (CompletableFuture<?>) r.future;
    return f;
  }

  @Override
  public <T> CompletableFuture<HttpResponse<T>> sendAsync(HttpRequest request, HttpResponse.BodyHandler<T> handler,
      HttpResponse.PushPromiseHandler<T> push) {
    return sendAsync(request, handler);
  }

  @Override
  public <T> HttpResponse<T> send(HttpRequest request, HttpResponse.BodyHandler<T> handler) {
    throw new UnsupportedOperationException("the shell sends asynchronously");
  }

  @Override
  public WebSocket.Builder newWebSocketBuilder() {
    return new WebSocket.Builder() {
      @Override
      public WebSocket.Builder header(String name, String value) {
        return this;
      }

      @Override
      public WebSocket.Builder connectTimeout(Duration timeout) {
        return this;
      }

      @Override
      public WebSocket.Builder subprotocols(String mostPreferred, String... lesserPreferred) {
        return this;
      }

      @Override
      public CompletableFuture<WebSocket> buildAsync(URI uri, WebSocket.Listener listener) {
        starting("a WebSocket to " + uri);
        Open o = new Open(next.incrementAndGet(), uri, listener);
        all.add(o);
        calls.add(o);
        return o.future;
      }
    };
  }

  /** The request's body, as the JDK would send it (the publisher of a byte array runs at once). */
  private static byte[] body(HttpRequest request) {
    java.io.ByteArrayOutputStream out = new java.io.ByteArrayOutputStream();
    request.bodyPublisher().ifPresent(p -> p.subscribe(new Flow.Subscriber<ByteBuffer>() {
      @Override
      public void onSubscribe(Flow.Subscription s) {
        s.request(Long.MAX_VALUE);
      }

      @Override
      public void onNext(ByteBuffer item) {
        byte[] b = new byte[item.remaining()];
        item.get(b);
        out.writeBytes(b);
      }

      @Override
      public void onError(Throwable throwable) {}

      @Override
      public void onComplete() {}
    }));
    return out.toByteArray();
  }

  /** A response whose body is a stream over bytes. */
  record Response(HttpRequest request, int statusCode, byte[] bytes, boolean slow, FakeHttp owner)
      implements HttpResponse<InputStream> {
    @Override
    public Optional<HttpResponse<InputStream>> previousResponse() {
      return Optional.empty();
    }

    @Override
    public HttpHeaders headers() {
      return HttpHeaders.of(Map.of("Content-Type", List.of("application/x-protobuf")), (a, b) -> true);
    }

    @Override
    public InputStream body() {
      return slow ? new SlowStream(bytes, owner) : new ByteArrayInputStream(bytes);
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

  /**
   * A body a few bytes at a time, yielding before each read; closed, it fails the next read. A read
   * once the client is sealed is a violation: a transfer still running after stop returned.
   */
  static final class SlowStream extends InputStream {
    private final byte[] bytes;
    private final FakeHttp owner;
    private int at;
    private volatile boolean closed;

    SlowStream(byte[] bytes, FakeHttp owner) {
      this.bytes = bytes;
      this.owner = owner;
    }

    @Override
    public int read() throws IOException {
      byte[] one = new byte[1];
      return read(one, 0, 1) < 0 ? -1 : one[0] & 0xff;
    }

    @Override
    public int read(byte[] b, int off, int len) throws IOException {
      Thread.yield();
      if (owner != null) {
        owner.starting("a body still being read (" + (closed ? "closed" : "open") + ")");
      }
      if (closed) {
        throw new IOException("closed");
      }
      if (at >= bytes.length) {
        return -1;
      }
      int n = Math.min(Math.min(len, 8), bytes.length - at);
      System.arraycopy(bytes, at, b, off, n);
      at += n;
      return n;
    }

    @Override
    public void close() {
      closed = true;
    }
  }

  @Override
  public Optional<CookieHandler> cookieHandler() {
    return Optional.empty();
  }

  @Override
  public Optional<Duration> connectTimeout() {
    return Optional.empty();
  }

  @Override
  public Redirect followRedirects() {
    return Redirect.NEVER;
  }

  @Override
  public Optional<ProxySelector> proxy() {
    return Optional.empty();
  }

  @Override
  public SSLContext sslContext() {
    try {
      return SSLContext.getDefault();
    } catch (java.security.NoSuchAlgorithmException e) {
      throw new IllegalStateException(e);
    }
  }

  @Override
  public SSLParameters sslParameters() {
    return new SSLParameters();
  }

  @Override
  public Optional<Authenticator> authenticator() {
    return Optional.empty();
  }

  @Override
  public HttpClient.Version version() {
    return HttpClient.Version.HTTP_1_1;
  }

  @Override
  public Optional<java.util.concurrent.Executor> executor() {
    return Optional.empty();
  }
}
