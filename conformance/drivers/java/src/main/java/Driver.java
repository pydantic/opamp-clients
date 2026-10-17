// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Runs io.opentelemetry.contrib:opentelemetry-opamp-client under the conformance driver contract
// (conformance/DRIVERS.md), over HTTP or WebSocket depending on OPAMP_ENDPOINT's scheme.

import io.opentelemetry.opamp.client.OpampClient;
import io.opentelemetry.opamp.client.OpampClientBuilder;
import io.opentelemetry.opamp.client.internal.connectivity.http.OkHttpSender;
import io.opentelemetry.opamp.client.internal.connectivity.websocket.OkHttpWebSocket;
import io.opentelemetry.opamp.client.internal.request.delay.PeriodicDelay;
import io.opentelemetry.opamp.client.internal.request.delay.RetryPeriodicDelay;
import io.opentelemetry.opamp.client.internal.request.service.HttpRequestService;
import io.opentelemetry.opamp.client.internal.request.service.WebSocketRequestService;
import io.opentelemetry.opamp.client.internal.response.MessageData;
import io.opentelemetry.opamp.client.request.service.RequestService;
import java.time.Duration;
import java.util.HexFormat;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import okhttp3.OkHttpClient;
import opamp.proto.AgentDescription;
import opamp.proto.AgentRemoteConfig;
import opamp.proto.AnyValue;
import opamp.proto.KeyValue;
import opamp.proto.RemoteConfigStatus;
import opamp.proto.RemoteConfigStatuses;
import opamp.proto.ServerErrorResponse;

public final class Driver {
  private static synchronized void emit(String event, String... kv) {
    StringBuilder sb = new StringBuilder("{\"event\":").append(quote(event));
    sb.append(",\"wall\":").append(System.currentTimeMillis() / 1000.0);
    for (int i = 0; i + 1 < kv.length; i += 2) {
      sb.append(',').append(quote(kv[i])).append(':').append(quote(kv[i + 1]));
    }
    System.out.println(sb.append('}'));
    System.out.flush();
  }

  private static String quote(String s) {
    StringBuilder sb = new StringBuilder("\"");
    for (char c : String.valueOf(s).toCharArray()) {
      if (c == '"' || c == '\\') {
        sb.append('\\').append(c);
      } else if (c < 0x20) {
        sb.append(String.format("\\u%04x", (int) c));
      } else {
        sb.append(c);
      }
    }
    return sb.append('"').toString();
  }

  private static double envDouble(String key, double def) {
    String v = System.getenv(key);
    return v == null || v.isEmpty() ? def : Double.parseDouble(v);
  }

  private static KeyValue attr(String key, String value) {
    return new KeyValue.Builder().key(key).value(new AnyValue.Builder().string_value(value).build()).build();
  }

  private static AgentDescription description(int change) {
    return new AgentDescription.Builder()
        .identifying_attributes(List.of(attr("service.name", "conformance-java")))
        .non_identifying_attributes(List.of(attr("driver.change", Integer.toString(change))))
        .build();
  }

  public static void main(String[] args) throws Exception {
    String endpoint = System.getenv("OPAMP_ENDPOINT");
    double runS = envDouble("DRIVER_RUN_S", 30);
    Duration poll = Duration.ofMillis((long) (envDouble("DRIVER_POLL_S", 2) * 1000));

    OkHttpClient http = new OkHttpClient();
    RequestService service = endpoint.startsWith("ws")
        ? WebSocketRequestService.create(OkHttpWebSocket.create(endpoint, http))
        : HttpRequestService.create(OkHttpSender.create(endpoint, http),
            PeriodicDelay.ofFixedDuration(poll), RetryPeriodicDelay.create(poll));

    OpampClientBuilder builder = OpampClient.builder()
        .putIdentifyingAttribute("service.name", "conformance-java")
        .putNonIdentifyingAttribute("driver.change", "0")
        .enableRemoteConfig()
        .setRequestService(service);
    String uid = System.getenv().getOrDefault("DRIVER_UID", "random");
    if (uid.equals("zero")) {
      builder.setInstanceUid(new byte[16]);
    } else if (!uid.equals("random") && !uid.isEmpty()) {
      builder.setInstanceUid(HexFormat.of().parseHex(uid));
    }

    OpampClient client = builder.build(new OpampClient.Callbacks() {
      @Override
      public void onConnect(OpampClient c) {
        emit("connect");
      }

      @Override
      public void onConnectFailed(OpampClient c, Throwable t) {
        emit("connect_failed", "error", String.valueOf(t));
      }

      @Override
      public void onErrorResponse(OpampClient c, ServerErrorResponse e) {
        emit("error_response", "type", String.valueOf(e.type), "message", e.error_message);
      }

      @Override
      public void onMessage(OpampClient c, MessageData data) {
        AgentRemoteConfig rc = data.getRemoteConfig();
        if (rc == null) {
          emit("message");
          return;
        }
        emit("message", "remote_config_hash", HexFormat.of().formatHex(rc.config_hash.toByteArray()));
        c.setRemoteConfigStatus(new RemoteConfigStatus.Builder()
            .last_remote_config_hash(rc.config_hash)
            .status(RemoteConfigStatuses.RemoteConfigStatuses_APPLIED)
            .build());
      }
    });
    emit("started", "client", "opentelemetry-opamp-client");

    ScheduledExecutorService timers = Executors.newSingleThreadScheduledExecutor();
    String changes = System.getenv().getOrDefault("DRIVER_CHANGES_AT_S", "");
    int n = 0;
    for (String at : changes.split(",")) {
      if (at.isBlank()) {
        continue;
      }
      int change = ++n;
      timers.schedule(() -> {
        emit("status_change", "change", Integer.toString(change));
        client.setAgentDescription(description(change));
      }, (long) (Double.parseDouble(at.trim()) * 1000), TimeUnit.MILLISECONDS);
    }

    Thread.sleep((long) (runS * 1000));
    timers.shutdownNow();
    emit("stopping");
    client.close();
    emit("stopped");
    System.exit(0);
  }
}
