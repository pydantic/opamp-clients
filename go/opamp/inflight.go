// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"sync"
	"time"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// inFlight is the register of the effects under way: requests, connections from their dial to
// their end, downloads and saves. It alone decides what reaches the core, what is cancelled, and
// when the client may let go. Its invariants:
//
//   - Every effect under way has an entry, keyed by its id, holding the cancel of its context.
//   - cancel removes the entry and cancels the context: the work aborts at once.
//   - A report reaches the core only if its entry is present when the core is about to take it
//     (deliver); a report that is an effect's last removes the entry. So nothing about an effect
//     reaches the core after its cancel, even a result already queued before it.
//   - A resource that becomes ready after its entry is gone (a dial that completes after its
//     cancel, or after close) is closed at once: its owner ties it to the entry's context with
//     context.AfterFunc, which runs at once on a context already done.
//   - After close, nothing registers: add returns an entry whose context is already done.
//   - close cancels everything and waits for the effects' goroutines, for at most a bound.
type inFlight struct {
	mu      sync.Mutex
	entries map[uint64]*effect
	closed  bool
	tasks   sync.WaitGroup
}

// effect is one effect under way.
type effect struct {
	owner  *inFlight // the register it is in (the executor's, or core mode's: streams.go)
	id     uint64
	ctx    context.Context // done once cancelled, or the register closed
	cancel context.CancelFunc
	cmds   *mailbox[*corepb.Effect] // a connection's sends and its close, in order
	file   string                   // a download's file
	// closeAsked: the core has asked to close this connection (ws_close; the loop's alone).
	closeAsked bool

	// stream: core mode's stream, for an entry of its register (nil for a resolution).
	stream *stream

	fileMu     sync.Mutex
	fileHanded bool // the download's goroutine is done with its file: it is the core's
}

// handOver is the download's goroutine giving its file to the core, done with it: false if the
// download was cancelled, and the file is still the goroutine's to delete.
func (e *effect) handOver() bool {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	if e.ctx.Err() != nil {
		return false
	}
	e.fileHanded = true
	return true
}

// handedOver says, once e is cancelled, whether its goroutine had handed its file over (and so
// won't delete it).
func (e *effect) handedOver() bool {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	return e.fileHanded
}

// report is what an effect's goroutine tells the core: an input, and whether it is the effect's
// last.
type report struct {
	e    *effect // nil: not an effect's (the application's calls, timers)
	in   *corepb.Input
	last bool
	// ack, if set, is closed once the loop has stepped the core with it and carried out the
	// step's effects, or dropped it (core mode's streams read in step with the core).
	ack chan struct{}
	// src: the stream whose bytes these are (core mode): the download_data the core makes of
	// them came from it.
	src *stream
}

// settled says r's sender the loop is done with it.
func (r report) settled() {
	if r.ack != nil {
		close(r.ack)
	}
}

func newInFlight() *inFlight { return &inFlight{entries: map[uint64]*effect{}} }

// add registers effect id. After close its context is done already, and it isn't registered.
func (f *inFlight) add(id uint64) *effect {
	ctx, cancel := context.WithCancel(context.Background())
	e := &effect{owner: f, id: id, ctx: ctx, cancel: cancel}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		cancel()
		return e
	}
	f.entries[id] = e
	return e
}

// spawn runs work on a goroutine of its own, which close waits for.
func (f *inFlight) spawn(work func()) {
	f.tasks.Add(1)
	go func() {
		defer f.tasks.Done()
		work()
	}()
}

// get is effect id, if it is under way.
func (f *inFlight) get(id uint64) *effect {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[id]
}

// cancel gives up on effect id: its entry goes and its work aborts. The entry, if there was one.
func (f *inFlight) cancel(id uint64) *effect {
	f.mu.Lock()
	e := f.entries[id]
	delete(f.entries, id)
	f.mu.Unlock()
	if e != nil {
		e.cancel()
	}
	return e
}

// deliver says whether r goes to the core: only while its effect is under way. An effect's last
// report ends it; so does a connection's end (ws_closed, ws_lost) once the core has asked to close
// it: the server's Close that crossed the core's close is the handshake done, and the connection's
// own end after it would be a second (RULE-SHELL-04). Called on the loop only.
func (f *inFlight) deliver(r report) bool {
	if r.e == nil {
		return true
	}
	if r.e.closeAsked && (r.in.GetWsClosed() != nil || r.in.GetWsLost() != nil) {
		r.last = true
	}
	f.mu.Lock()
	present := f.entries[r.e.id] == r.e
	if present && r.last {
		delete(f.entries, r.e.id)
	}
	f.mu.Unlock()
	if present && r.last {
		r.e.cancel() // releases its context
	}
	return present
}

// cancelAll registers nothing more, and cancels everything under way.
func (f *inFlight) cancelAll() {
	f.mu.Lock()
	f.closed = true
	all := f.entries
	f.entries = map[uint64]*effect{}
	f.mu.Unlock()
	for _, e := range all {
		e.cancel()
	}
}

// close registers nothing more, cancels everything under way, and waits for the effects'
// goroutines, for at most bound: whether they all ended.
func (f *inFlight) close(bound time.Duration) bool {
	f.cancelAll()
	ended := make(chan struct{})
	go func() {
		f.tasks.Wait()
		close(ended)
	}()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-ended:
		return true
	case <-t.C:
		return false
	}
}
