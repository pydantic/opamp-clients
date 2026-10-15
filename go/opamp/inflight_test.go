// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// The register's invariants, in a synctest bubble (fake time; synctest.Wait runs every goroutine
// until all are blocked): a late result is dropped, a late resource is closed, nothing registers
// after close, and close waits for the effects' goroutines for its bound only.
func TestInFlightInvariants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newInFlight()

		// A result, queued before its cancel, is dropped after it.
		e1 := f.add(1)
		late := report{e: e1, last: true}
		if f.cancel(1) != e1 || e1.ctx.Err() == nil {
			t.Fatal("cancel didn't abort the effect")
		}
		if f.deliver(late) {
			t.Fatal("a result reached the core after its cancel")
		}
		if f.cancel(1) != nil {
			t.Fatal("cancelled twice")
		}

		// Reports go until the last, which ends the effect; nothing after it.
		e2 := f.add(2)
		if !f.deliver(report{e: e2}) || !f.deliver(report{e: e2, last: true}) {
			t.Fatal("a report of an effect under way was dropped")
		}
		if f.deliver(report{e: e2}) || f.get(2) != nil || e2.ctx.Err() == nil {
			t.Fatal("the effect lives on after its last report")
		}
		// An effect with the same id later is another effect: an old one's report is dropped.
		e2b := f.add(2)
		if f.deliver(report{e: e2}) || !f.deliver(report{e: e2b}) {
			t.Fatal("reports confused between effects of one id")
		}

		// A resource ready after its cancel (a dial that completes late) is closed at once.
		e3 := f.add(3)
		var closed atomic.Bool
		f.spawn(func() {
			time.Sleep(time.Second) // the dial
			context.AfterFunc(e3.ctx, func() { closed.Store(true) })
		})
		f.cancel(3)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !closed.Load() {
			t.Fatal("a connection that opened after its cancel is still open")
		}

		// close: everything cancelled, nothing registers after, and the wait is bounded.
		e4 := f.add(4)
		f.spawn(func() {
			<-e4.ctx.Done()
			time.Sleep(10 * time.Second) // slow to end
		})
		began := time.Now()
		if f.close(time.Second) {
			t.Fatal("close says a goroutine still running has ended")
		}
		if took := time.Since(began); took != time.Second {
			t.Fatalf("close waited %v, its bound is 1 s", took)
		}
		if e4.ctx.Err() == nil || e2b.ctx.Err() == nil {
			t.Fatal("close left an effect running")
		}
		e5 := f.add(5)
		if e5.ctx.Err() == nil || f.get(5) != nil || f.deliver(report{e: e5}) {
			t.Fatal("an effect registered after close")
		}
		// A closed register whose goroutines have ended says so.
		time.Sleep(10 * time.Second)
		if !f.close(time.Second) {
			t.Fatal("close says goroutines that ended are running")
		}
	})
}

// Once the core has asked to close a connection (ws_close), the first end reported is its last:
// the server's Close crossing the core's close is the handshake done, not a second end (found by
// TestSchedules, -rapid.checks=10000, base seed 8046487757770476665: ws_closed fed twice).
func TestAfterTheCoresCloseTheFirstEndIsTheLast(t *testing.T) {
	for _, end := range []*corepb.Input{wsClosed(7, 1000), wsLost(7, errors.New("gone"))} {
		x := newExecutor(&connection{})
		e := x.fl.add(7)
		e.cmds = newMailbox[*corepb.Effect]()
		x.command(7, &corepb.Effect{Kind: &corepb.Effect_WsClose{WsClose: &corepb.Id{Id: 7}}})
		// The connection's goroutine reported the server's Close (or the loss) before it took the
		// core's close, then its own end.
		if !x.deliver(report{e: e, in: end, last: false}) {
			t.Fatalf("%v: the end was dropped", end)
		}
		if x.deliver(report{e: e, in: wsClosed(7, 1000), last: true}) {
			t.Fatalf("%v: the connection ended twice", end)
		}
		if e.ctx.Err() == nil {
			t.Fatalf("%v: the connection's work goes on after its end", end)
		}
	}
}

// A connection gone without a Close frame is lost, not closed: gorilla reports it as a CloseError
// with 1006 (abnormal closure), a code never sent on the wire.
func TestAConnectionGoneWithoutACloseFrameIsLost(t *testing.T) {
	if _, ok := closeCode(&websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"}); ok {
		t.Fatal("1006 taken for the server's Close frame")
	}
	if code, ok := closeCode(&websocket.CloseError{Code: websocket.CloseGoingAway}); !ok || code != websocket.CloseGoingAway {
		t.Fatalf("the server's Close (1001) read as %d, %v", code, ok)
	}
}
