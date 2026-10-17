// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.Set;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.logging.Logger;
import dev.pydantic.opamp.AppSettings;

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
    for (long n = 0; ; n++) {
      URI to = n % 3 == 2 ? health : ping;
      try {
        http.send(HttpRequest.newBuilder(to).build(), HttpResponse.BodyHandlers.discarding());
      } catch (java.io.IOException e) {
        // The server may be gone at the end of a run.
      }
      LOG.info("requested " + to);
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

  private static void get(HttpClient http, URI to) throws InterruptedException {
    try {
      http.send(HttpRequest.newBuilder(to).build(), HttpResponse.BodyHandlers.discarding());
    } catch (java.io.IOException e) {
      // The server may be gone at the end of a run.
    }
  }
}
