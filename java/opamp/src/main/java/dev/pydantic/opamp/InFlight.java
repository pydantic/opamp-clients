// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import dev.pydantic.opamp.core.CoreProto;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CompletableFuture;

/**
 * The effects in flight, by effect id: a request, a WebSocket being opened or open, a download, a
 * save. The one place that decides what reaches the core, what a cancel aborts and what stop
 * releases; the {@link Executor} only does the I/O. Thread-safe: every decision is taken under its
 * lock.
 *
 * <ol>
 *   <li>An input about an effect reaches the core only while its entry is present, and its last
 *       result removes the entry, in one step: a result that comes late (after a cancel, after the
 *       last result, after close) is dropped.
 *   <li>A cancel removes the entry and aborts its work: nothing more about it reaches the core.
 *   <li>A resource that becomes ready after its entry is gone (a WebSocket that opens after its
 *       cancel, or after stop) is closed at once.
 *   <li>After {@link #close}, nothing registers: work added then is aborted at once. Close aborts
 *       every entry, and its future completes when their tasks (and those of cancelled entries
 *       still finishing) are over.
 * </ol>
 */
final class InFlight {
  /** An effect's work: how to abort it. */
  interface Work {
    /** Stop it at once: close its connection, stop its transfer, delete its partial file. */
    void abort();
  }

  private static final class Entry {
    final Work work;
    /** Completes when its task is over (null: nothing to wait for once aborted). */
    final CompletableFuture<?> task;
    /** Closes the resource that became ready for it (an open WebSocket), if any. */
    Runnable resource;
    /** The core closed it (ws_close): its next ws_closed is its last word... */
    boolean closing;
    /**
     * ...but for the server's Close frames reported (ws_closed) before the core closed it and not
     * yet admitted: each is the server's, never the end of the core's close, however late it is
     * admitted (the core may close it in between, at stop say).
     */
    int serverCloses;

    Entry(Work work, CompletableFuture<?> task) {
      this.work = work;
      this.task = task;
    }
  }

  private final Map<Long, Entry> entries = new HashMap<>();
  /** Tasks of entries cancelled that may still be finishing (a save writing). */
  private final Set<CompletableFuture<?>> finishing = new HashSet<>();
  private boolean closed;

  /**
   * Effect {@code id} is under way: {@code work} aborts it, {@code task} completes when it is over.
   * After close it is aborted at once; false then.
   */
  boolean add(long id, Work work, CompletableFuture<?> task) {
    synchronized (this) {
      if (!closed) {
        entries.put(id, new Entry(work, task));
        return true;
      }
    }
    work.abort();
    return false;
  }

  /** The work of effect {@code id}, if it is still in flight. */
  synchronized Work get(long id) {
    Entry e = entries.get(id);
    return e == null ? null : e.work;
  }

  /** The core closes connection {@code id} (ws_close): its work, if it is still in flight. */
  synchronized Work closing(long id) {
    Entry e = entries.get(id);
    if (e == null) {
      return null;
    }
    e.closing = true;
    return e.work;
  }

  /**
   * The server's Close of connection {@code id} is reported before the core closed it: that
   * ws_closed is not its end. Before it is reported.
   */
  synchronized void serverClosed(long id) {
    Entry e = entries.get(id);
    if (e != null) {
      e.serverCloses++;
    }
  }

  /**
   * The resource of effect {@code id} is ready (a WebSocket opened): kept while the entry is there,
   * and closed with it; if the entry is gone, {@code close} runs at once and this is false.
   */
  boolean attach(long id, Runnable close) {
    synchronized (this) {
      Entry e = entries.get(id);
      if (e != null) {
        e.resource = close;
        return true;
      }
    }
    close.run();
    return false;
  }

  /**
   * Whether {@code input} goes to the core. Anything but an effect's result goes. A result goes
   * only while its effect's entry is present, and its last result removes the entry.
   */
  boolean admit(CoreProto.Input input) {
    long id;
    Boolean last = Boolean.TRUE;
    switch (input.getKindCase()) {
      case HTTP_RESPONSE -> id = input.getHttpResponse().getId();
      case HTTP_FAILED -> id = input.getHttpFailed().getId();
      case WS_OPENED -> {
        id = input.getWsOpened().getId();
        last = false;
      }
      case WS_FAILED -> id = input.getWsFailed().getId();
      case WS_MESSAGE -> {
        id = input.getWsMessage().getId();
        last = false;
      }
      // The server's Close frame, or, once the core has closed it, the end of the handshake.
      case WS_CLOSED -> {
        id = input.getWsClosed().getId();
        last = null;
      }
      case WS_LOST -> id = input.getWsLost().getId();
      case STORED -> id = input.getStored().getId();
      case STORE_FAILED -> id = input.getStoreFailed().getId();
      case DOWNLOADED -> id = input.getDownloaded().getId();
      case DOWNLOAD_RESPONSE -> id = input.getDownloadResponse().getId();
      case DOWNLOAD_FAILED -> id = input.getDownloadFailed().getId();
      default -> {
        return true;
      }
    }
    synchronized (this) {
      Entry e = entries.get(id);
      if (e == null) {
        return false;
      }
      boolean end;
      if (last != null) {
        end = last;
      } else if (e.serverCloses > 0) {
        e.serverCloses--; // the server's Close: the core's close, if any, ends later
        end = false;
      } else {
        end = e.closing;
      }
      if (end) {
        entries.remove(id);
      }
      return true;
    }
  }

  /** The core gives up on effect {@code id}: its entry goes and its work is aborted. */
  void cancel(long id) {
    Entry e;
    synchronized (this) {
      e = entries.remove(id);
      if (e == null) {
        return;
      }
      finishing(e);
    }
    abort(e);
  }

  /**
   * The client has ended: nothing registers from now on, and everything in flight is aborted. The
   * future completes when their tasks are over (the caller bounds the wait).
   */
  CompletableFuture<Void> close() {
    List<Entry> all;
    List<CompletableFuture<?>> tasks;
    synchronized (this) {
      closed = true;
      all = new ArrayList<>(entries.values());
      entries.clear();
      all.forEach(this::finishing);
      tasks = new ArrayList<>(finishing);
    }
    all.forEach(InFlight::abort);
    return CompletableFuture.allOf(tasks.stream()
        .map(t -> t.handle((v, x) -> null))
        .toArray(CompletableFuture[]::new));
  }

  /**
   * Run {@code act} if effect {@code id} is still in flight: false, without running it, if the
   * effect is gone. A save's rename goes through here: a save cancelled before it lands doesn't
   * land. The rename runs outside the lock (a slow disk would hold up every input waiting for
   * it): one cancelled while it runs lands, as if it had landed just before, and its answer is
   * dropped. Saves run one at a time, so it still lands before any later save.
   */
  boolean whileInFlight(long id, StateFile.Rename act) throws java.io.IOException {
    synchronized (this) {
      if (!entries.containsKey(id)) {
        return false;
      }
    }
    act.run();
    return true;
  }

  /** Whether effect {@code id} is in flight. */
  synchronized boolean has(long id) {
    return entries.containsKey(id);
  }

  /** Keep {@code e}'s task until it is over. Under the lock. */
  private void finishing(Entry e) {
    CompletableFuture<?> t = e.task;
    if (t != null && !t.isDone()) {
      finishing.add(t);
      t.whenComplete((v, x) -> {
        synchronized (this) {
          finishing.remove(t);
        }
      });
    }
  }

  private static void abort(Entry e) {
    e.work.abort();
    if (e.resource != null) {
      e.resource.run();
    }
  }
}
