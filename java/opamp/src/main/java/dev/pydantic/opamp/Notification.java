// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.time.Duration;
import opamp.proto.v1.Opamp;

/** What the application is told. */
public sealed interface Notification {
  /** Connected (WebSocket), or an HTTP request succeeded: the first, or the first after failures. */
  record Connected() implements Notification {}

  /** Connecting or a request failed; {@code status} is the HTTP status, if there was one. */
  record ConnectFailed(Integer status, String detail) implements Notification {}

  /** The server sent an error_response ({@code type} is a ServerErrorResponseType value). */
  record ErrorResponse(int type, String message) implements Notification {}

  /** A remote config to apply; answer with {@link OpampClient#setRemoteConfigStatus}. */
  record RemoteConfig(Opamp.AgentRemoteConfig config) implements Notification {}

  /**
   * Own telemetry and other connection settings, all of them; answer with {@link
   * OpampClient#setConnectionSettingsStatus}.
   */
  record ConnectionSettings(Opamp.ConnectionSettingsOffers settings) implements Notification {
    /** The offers, but no header's value, private key or URL's password. */
    @Override
    public String toString() {
      return "ConnectionSettings[settings=" + Redacted.offers(settings) + "]";
    }
  }

  /** A custom message from the server, for a custom capability the agent declared. */
  record CustomMessage(Opamp.CustomMessage message) implements Notification {}

  /** The custom capabilities the server supports changed. */
  record ServerCustomCapabilities(java.util.List<String> capabilities) implements Notification {}

  /** A custom message the application sent won't be, and why. */
  record CustomMessageNotSent(Opamp.CustomMessage message, String reason) implements Notification {}

  /**
   * Install package {@code name} ({@code pkg}: version, hash, type, the file's signature) from the
   * file at {@code path}; checking the signature is the application's job. Answer with {@link
   * OpampClient#packageInstalled}; the client deletes the file after the answer.
   */
  record InstallPackage(String name, Opamp.PackageAvailable pkg, java.nio.file.Path path) implements Notification {
    /** Every member, but the download's headers without their values and its URL without a password. */
    @Override
    public String toString() {
      return "InstallPackage[name=" + name + ", pkg=" + Redacted.pkg(pkg) + ", path=" + path + "]";
    }
  }

  /** Remove package {@code name}, no longer offered; answer with {@link OpampClient#packageRemoved}. */
  record RemovePackage(String name) implements Notification {}

  /** A command from the server. */
  record Command(Opamp.ServerToAgentCommand command) implements Notification {}

  /** The server assigned a new instance UID; persist it if you persist the UID. */
  record InstanceUidChanged(byte[] instanceUid) implements Notification {}

  /** The heartbeat or polling interval changed ({@link Duration#ZERO}: heartbeats off). */
  record HeartbeatInterval(Duration interval) implements Notification {}

  /**
   * A status field was left out of a message because it was too large; it isn't tried again until
   * it changes or the server asks for the full state. {@code field} is its name in AgentToServer, as
   * the core gives it ({@code agent_description}, {@code health}, {@code effective_config}, ...).
   */
  record StatusTooLarge(String field, long bytes) implements Notification {}

  /** The server sent something that isn't a ServerToAgent message. */
  record BadMessage(String detail) implements Notification {}

  /**
   * The save under way has run for its stall time without ending ({@code stalled}), and the client
   * waits on it still, saving nothing else meanwhile; or it has ended at last (not {@code stalled}).
   * {@code waited}: how long it had run. A disk that hangs is the application's to see to.
   */
  record SaveStalled(boolean stalled, Duration waited) implements Notification {}

  /**
   * The client has stopped: after {@link OpampClient#stop}, or on its own because of {@code error}
   * (which {@code stop()} completes with too). {@code error} is null after {@code stop()}.
   */
  record Stopped(Throwable error) implements Notification {}
}
