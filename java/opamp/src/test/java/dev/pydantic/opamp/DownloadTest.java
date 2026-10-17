// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.sun.net.httpserver.HttpServer;
import dev.pydantic.opamp.core.CoreProto;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.PosixFilePermissions;
import java.security.MessageDigest;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Downloads: whatever the status, the body goes to the file and the response
 * to the core, a redirect with its Location resolved and that URL's origin; no redirect is followed.
 */
class DownloadTest {
  @Test
  void everyStatusIsReportedAndNoRedirectFollowed(@TempDir Path dir) throws Exception {
    byte[] body = "package".repeat(100).getBytes();
    byte[] moved = "moved".getBytes();
    List<String> asked = new CopyOnWriteArrayList<>();
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.createContext("/", ex -> {
      String path = ex.getRequestURI().getPath();
      asked.add(path + " " + ex.getRequestHeaders().getFirst("Authorization"));
      switch (path) {
        case "/a/c/moved" -> {
          ex.getResponseHeaders().add("Location", "../b/./file?x=1");
          ex.sendResponseHeaders(302, moved.length);
          ex.getResponseBody().write(moved);
        }
        case "/away" -> {
          ex.getResponseHeaders().add("Location", "https://CDN.example:443/f");
          ex.sendResponseHeaders(301, -1);
        }
        case "/nowhere" -> ex.sendResponseHeaders(307, -1);
        case "/file" -> {
          ex.sendResponseHeaders(200, body.length);
          ex.getResponseBody().write(body);
        }
        default -> {
          ex.sendResponseHeaders(404, 3);
          ex.getResponseBody().write("no!".getBytes());
        }
      }
      ex.close();
    });
    server.start();
    String base = "http://127.0.0.1:" + server.getAddress().getPort();
    Path state = dir.resolve("state");
    LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    Executor x = new Executor(Transport.of(HttpClient.newHttpClient()), got::add, state);
    try {
      CoreProto.DownloadResponse ok = get(x, got, 1, base + "/file", 1 << 20).getDownloadResponse();
      assertEquals(200, ok.getStatus());
      assertArrayEquals(sha(body), ok.getSha256().toByteArray());
      assertEquals("", ok.getLocation());
      assertFalse(ok.hasLocationOrigin());
      assertArrayEquals(body, Files.readAllBytes(x.downloadPath(1)));
      if (dir.getFileSystem().supportedFileAttributeViews().contains("posix")) {
        assertEquals("rwx------", PosixFilePermissions.toString(Files.getPosixFilePermissions(Executor.downloadsDir(state))));
        assertEquals("rw-------", PosixFilePermissions.toString(Files.getPosixFilePermissions(x.downloadPath(1))));
      }
      x.run(CoreProto.Effect.newBuilder().setDiscard(CoreProto.Id.newBuilder().setId(1)).build());
      assertFalse(Files.exists(x.downloadPath(1)));

      // A relative Location, with dot segments: resolved against the URL asked, not followed.
      CoreProto.DownloadResponse r = get(x, got, 2, base + "/a/c/moved", 1 << 20).getDownloadResponse();
      assertEquals(302, r.getStatus());
      assertArrayEquals(sha(moved), r.getSha256().toByteArray(), "the body of the redirect, as it came");
      assertEquals(base + "/a/b/file?x=1", r.getLocation());
      assertEquals(CoreProto.Origin.newBuilder().setScheme("http").setHost("127.0.0.1").setPort(server.getAddress().getPort()).build(),
          r.getLocationOrigin());
      // An absolute one elsewhere: the default port is 0.
      CoreProto.DownloadResponse away = get(x, got, 3, base + "/away", 1 << 20).getDownloadResponse();
      assertEquals(301, away.getStatus());
      assertEquals("https://CDN.example:443/f", away.getLocation());
      assertEquals(CoreProto.Origin.newBuilder().setScheme("https").setHost("CDN.example").setPort(0).build(), away.getLocationOrigin());
      // A redirect without a Location: nothing to resolve.
      CoreProto.DownloadResponse nowhere = get(x, got, 4, base + "/nowhere", 1 << 20).getDownloadResponse();
      assertEquals(307, nowhere.getStatus());
      assertEquals("", nowhere.getLocation());
      assertFalse(nowhere.hasLocationOrigin());
      // Any other status: the body to the file, the status to the core.
      CoreProto.DownloadResponse missing = get(x, got, 5, base + "/missing", 1 << 20).getDownloadResponse();
      assertEquals(404, missing.getStatus());
      assertArrayEquals("no!".getBytes(), Files.readAllBytes(x.downloadPath(5)));
      assertEquals(List.of("/file t", "/a/c/moved t", "/away t", "/nowhere t", "/missing t"), asked, "nothing followed");

      // Too large: failed, and nothing is left.
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_FAILED, get(x, got, 6, base + "/file", 10).getKindCase());
      assertFalse(Files.exists(x.downloadPath(6)));
      // No server: failed.
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_FAILED, get(x, got, 7, "http://127.0.0.1:1/file", 10).getKindCase());
      // What was downloaded before a restart goes.
      Files.write(x.downloadPath(8), new byte[] {1});
      Executor.deleteDownloads(state);
      assertFalse(Files.exists(Executor.downloadsDir(state)));
    } finally {
      server.stop(0);
    }
  }

  /** Locations as java.net.URI resolves them, and their origins as it reads them. */
  @Test
  void locationsAreResolvedByUri() {
    URI asked = URI.create("https://files.example/pkgs/v1/agent.tar.gz");
    assertLocation(asked, "agent-2.tar.gz", "https://files.example/pkgs/v1/agent-2.tar.gz", "https", "files.example", 0);
    assertLocation(asked, "../../other/./x", "https://files.example/other/x", "https", "files.example", 0);
    assertLocation(asked, "/abs", "https://files.example/abs", "https", "files.example", 0);
    assertLocation(asked, "//cdn.example:8443/p", "https://cdn.example:8443/p", "https", "cdn.example", 8443);
    assertLocation(asked, "http://cdn.example:80/p", "http://cdn.example:80/p", "http", "cdn.example", 0);
    assertLocation(asked, "http://[::1]:8080/p", "http://[::1]:8080/p", "http", "[::1]", 8080);
    // A base with no path merges as "/".
    assertLocation(URI.create("http://h.example"), "x", "http://h.example/x", "http", "h.example", 0);
    // Doesn't resolve: as given, no origin.
    CoreProto.DownloadResponse.Builder bad = CoreProto.DownloadResponse.newBuilder();
    Executor.location(bad, asked, "http://bad host/x");
    assertEquals("http://bad host/x", bad.getLocation());
    assertFalse(bad.hasLocationOrigin());
  }

  private static void assertLocation(URI asked, String given, String url, String scheme, String host, int port) {
    CoreProto.DownloadResponse.Builder r = CoreProto.DownloadResponse.newBuilder();
    Executor.location(r, asked, given);
    assertEquals(url, r.getLocation(), given);
    assertTrue(r.hasLocationOrigin(), given);
    assertEquals(CoreProto.Origin.newBuilder().setScheme(scheme).setHost(host).setPort(port).build(), r.getLocationOrigin(), given);
  }

  static byte[] sha(byte[] b) throws Exception {
    return MessageDigest.getInstance("SHA-256").digest(b);
  }

  private static CoreProto.Input get(Executor x, LinkedBlockingQueue<CoreProto.Input> got, long id, String url, long max)
      throws InterruptedException {
    x.run(CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
        .setId(id).setUrl(url).setMaxBytes(max).setTimeoutMs(5000)
        .addHeaders(CoreProto.Header.newBuilder().setName("Authorization").setValue("t"))).build());
    CoreProto.Input in = got.poll(10, TimeUnit.SECONDS);
    assertTrue(x.admit(in), "the download's one result");
    return in;
  }
}
