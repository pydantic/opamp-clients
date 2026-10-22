// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.Set;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.ThreadLocalRandom;
import java.util.logging.Logger;
import dev.pydantic.opamp.AppSettings;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.logs.Severity;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;
import io.opentelemetry.api.trace.SpanContext;

/**
 * An application with no OpenTelemetry code at all: it makes an HTTP request every 200 ms, every
 * third one a health check, and logs a line for each with java.util.logging, plus one from a noisy
 * library's logger. The upstream Java agent's instrumentation traces (and measures) the requests
 * and exports the log lines; the extension puts the agent under the OpAMP server. Run it as
 * {@code java -javaagent:... App <base>/ping}: the health checks go to {@code <base>/health}.
 *
 * <p>It also reads a setting of its own the server sends (docs/app-settings.md), with the
 * extension's {@code AppSettings} (its app-settings jar on the class path): a feature flag,
 * {@code example.feature}. While the server says "on", each round also requests
 * {@code <base>/feature}; and each time its settings change it requests {@code <base>/changed},
 * once. Without the agent, or until the server says, the flag is its default, "off".
 *
 * <p>With EXAMPLE_PROBE=1 (integrations.py's run 3) each round also probes redaction, through the
 * OpenTelemetry API the agent bridges (its jar on the class path): a span of its own,
 * {@code example.probe}, with the URL it requests (url.full) on an event and on a link to the
 * round before's probe, and a log record {@code example.probe} with that URL; each with a note
 * ({@code example.note}) that says password, for scrubbing to replace. It probes before or
 * after the request, at random: under a rate limit of one a second, the first record after the
 * token comes takes it, and in a fixed order the probe might never be that one.
 */
public final class App {
  private static final Logger LOG = Logger.getLogger("app");
  private static final Logger NOISY = Logger.getLogger("app.noisy");

  public static void main(String[] args) throws Exception {
    HttpClient http = HttpClient.newHttpClient();
    URI ping = URI.create(args[0]);
    URI health = ping.resolve("health");
    URI feature = ping.resolve("feature");
    URI changedUri = ping.resolve("changed");
    // Told on the extension's thread: keep it quick, and do the work in the application's own
    // loop. The subscription lasts as long as the program.
    BlockingQueue<Set<String>> changes = new LinkedBlockingQueue<>();
    AppSettings.onChange((settings, changed) -> changes.add(changed));
    boolean probing = "1".equals(System.getenv("EXAMPLE_PROBE"));
    SpanContext lastProbe = null;
    for (long n = 0; ; n++) {
      URI to = n % 3 == 2 ? health : ping;
      boolean early = ThreadLocalRandom.current().nextBoolean();
      if (probing && early) {
        lastProbe = probe(ping.toString(), lastProbe);
      }
      try {
        http.send(HttpRequest.newBuilder(to).build(), HttpResponse.BodyHandlers.discarding());
      } catch (java.io.IOException e) {
        // The server may be gone at the end of a run.
      }
      LOG.info("requested " + to);
      if (probing && !early) {
        lastProbe = probe(ping.toString(), lastProbe);
      }
      if (AppSettings.get("example.feature", "off").equals("on")) {
        get(http, feature);
      }
      for (Set<String> changed; (changed = changes.poll()) != null; ) {
        LOG.info("settings changed: " + String.join(", ", changed));
        get(http, changedUri);
      }
      NOISY.info("connection pool: 3 idle");
      Thread.sleep(200);
    }
  }

  /** What the probe carries beside its URL (example.note): it says password, so scrubbing replaces it. */
  private static final String NOTE = "the password is p4ss";

  /**
   * A span of its own with {@code url} on an event and on a link to {@code last} (none while it is
   * null), and a log record with {@code url}: what redaction may drop or mask. Returns the span's
   * context, for the next probe to link to. Each carries {@link #NOTE} too, which scrubbing replaces.
   */
  private static SpanContext probe(String url, SpanContext last) {
    Attributes attributes = Attributes.of(
        AttributeKey.stringKey("url.full"), url, AttributeKey.stringKey("example.note"), NOTE);
    SpanBuilder builder = GlobalOpenTelemetry.getTracer("app").spanBuilder("example.probe").setNoParent();
    if (last != null) {
      builder.addLink(last, attributes);
    }
    Span span = builder.startSpan();
    span.addEvent("example.probe", attributes);
    span.end();
    GlobalOpenTelemetry.get().getLogsBridge().get("app").logRecordBuilder()
        .setSeverity(Severity.INFO).setSeverityText("INFO").setBody("example.probe")
        .setAllAttributes(attributes).emit();
    return span.getSpanContext();
  }

  private static void get(HttpClient http, URI to) throws InterruptedException {
    try {
      http.send(HttpRequest.newBuilder(to).build(), HttpResponse.BodyHandlers.discarding());
    } catch (java.io.IOException e) {
      // The server may be gone at the end of a run.
    }
  }
}
