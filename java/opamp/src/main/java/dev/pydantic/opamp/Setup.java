// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import dev.pydantic.opamp.core.CoreProto;

/**
 * The core's Setup call: an OpenTelemetry integration's start-up decisions (where the server is,
 * the headers, the heartbeat interval, the state directory, the instance UID, what is safe to log),
 * made by the core, the same in every language. The integration keeps only the I/O: reading its
 * settings, making and locking a state directory's slot, reading and writing its instance-uid
 * file, and logging what it is told to. It needs no client and keeps no state: the same call
 * always gives the same answer. Thread-safe.
 */
public final class Setup {
  /** The file in a state directory's slot that a process holds a lock on while it runs. */
  public static final String LOCK_FILE = "lock";
  /** The file in a state directory that holds the instance UID. */
  public static final String UID_FILE = "instance-uid";

  private Setup() {}

  /** The core could not answer: a call it can't use (a bug in the integration) or a trap. */
  public static final class Failed extends RuntimeException {
    Failed(String message, Throwable cause) {
      super(message, cause);
    }
  }

  /**
   * Where the server is and how to reach it, from the environment and what the code gives;
   * {@code off} without an endpoint.
   *
   * @throws Failed on a failure of the core
   */
  public static CoreProto.SetupStarted start(CoreProto.SetupStart start) {
    return call(CoreProto.SetupCall.newBuilder().setStart(start).build()).getStarted();
  }

  /**
   * The instance UID, once the state directory is known (and its slot locked).
   *
   * @throws Failed on a failure of the core, or a call it can't use
   */
  public static CoreProto.SetupIdentified identify(CoreProto.SetupIdentity identity) {
    return call(CoreProto.SetupCall.newBuilder().setIdentity(identity).build()).getIdentified();
  }

  private static CoreProto.SetupReply call(CoreProto.SetupCall call) {
    CoreProto.SetupReply reply;
    try {
      reply = Core.setup(call);
    } catch (Core.CoreException e) {
      throw new Failed(e.getMessage(), e);
    }
    if (reply.getResultCase() == CoreProto.SetupReply.ResultCase.ERROR) {
      throw new Failed(reply.getError(), null);
    }
    return reply;
  }
}
