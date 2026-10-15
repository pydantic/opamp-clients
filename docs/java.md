# Java

Use the upstream client, `io.opentelemetry.contrib:opentelemetry-opamp-client`, unchanged.
Checked against 1.60.0-alpha.

## Use the HTTP transport

Build the client with `HttpRequestService`, not `WebSocketRequestService`. The WebSocket
transport stops for good when the server closes the connection with a close frame: it never
reconnects and no callback fires. It also drops its first message and sends nothing after a
reconnect: in a torture run, status set while it was disconnected didn't reach the server
until the application changed it again 70 s later, and the server's ReportFullState went
unanswered. The first three are reported upstream in
[open-telemetry/opentelemetry-java-contrib#3125](https://github.com/open-telemetry/opentelemetry-java-contrib/issues/3125).

```java
OkHttpClient http = new OkHttpClient.Builder()
    .addInterceptor(chain -> chain.proceed(chain.request().newBuilder()
        .header("Authorization", "Bearer " + token)
        .build()))
    .build();

OpampClient client = OpampClient.builder()
    .putIdentifyingAttribute("service.name", "my-service")
    .enableRemoteConfig()
    .setRequestService(HttpRequestService.create(
        OkHttpSender.create(endpoint, http),
        PeriodicDelay.ofFixedDuration(Duration.ofSeconds(30)),
        RetryPeriodicDelay.create(Duration.ofSeconds(30))))
    .build(callbacks);
```

## Maven

The published POM depends on `com.squareup.wire:wire-runtime`, whose jar has no classes when
Maven resolves it (the classes are in `wire-runtime-jvm`, which only Gradle finds). Add both of
these next to the client:

```xml
<dependency>
  <groupId>com.squareup.wire</groupId>
  <artifactId>wire-runtime-jvm</artifactId>
  <version>6.4.5</version>
</dependency>
<!-- wire-runtime brings okio 3.17.0; OkHttp 5 needs 3.18.1 or later -->
<dependency>
  <groupId>com.squareup.okio</groupId>
  <artifactId>okio-jvm</artifactId>
  <version>3.18.1</version>
</dependency>
```

Gradle builds need neither. With either build tool, add OkHttp itself as a compile dependency
(`com.squareup.okhttp3:okhttp-jvm:5.5.0` for Maven): the client only depends on it at runtime,
but building the request service above needs `OkHttpClient`.

## Known gaps over HTTP

- The client never declares `ReportsStatus`, which the spec requires. Servers that insist on it
  will treat the agent as non-compliant.
- A 401 or 413 does not reach `Callbacks`, so the application can't see that its token was
  rejected. Watch the client's logs instead.
- `close()` does not send `agent_disconnect`, so the server only notices the agent is gone when
  it stops polling.

All of these are in the same upstream issue.

## This repository's client

`java/opamp` (the shared core behind `java.net.http.HttpClient`) takes TLS settings, a client
certificate and an HTTP proxy as options; see its [README](../java/opamp/README.md). Without a
proxy it uses the JVM's default `ProxySelector` (`http.proxyHost` and the other proxy
properties); `directConnection()` goes straight to the server instead. Two limits come from the
JDK:

- HTTP proxies only. `HttpClient` has no SOCKS and no `https://` proxies, so the client refuses
  `socks5://` and `https://` proxy URLs.
- The JDK sends no Basic proxy credentials through a CONNECT tunnel (`https://`, `ws://`, `wss://`)
  unless the JVM runs with `-Djdk.http.auth.tunneling.disabledSchemes=`. It is the JVM's setting,
  so the client leaves it to the application.
