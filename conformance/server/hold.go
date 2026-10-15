// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

import (
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
)

// Holding (scenario "hold"): a request, an upgrade or a download the server answers nothing more
// of, until the client lets go. The log says when it was taken ("held") and when the client closed
// it ("held_closed", by_client false if the server was shutting down then): a client that stops
// mid-connect or mid-download must let go by the time it says it has stopped (RULE-CLIENT-16).

// holding says whether the scenario holds `what` ("messages" or "downloads") now.
func (s *server) holding(what string) bool {
	h := s.sc.Hold
	if h == nil || s.elapsed() < h.FromS || (h.UntilS > 0 && s.elapsed() >= h.UntilS) {
		return false
	}
	if !((what == "messages" && h.Messages) || (what == "downloads" && h.Downloads)) {
		return false
	}
	if h.Count > 0 {
		n, _ := s.heldOf.LoadOrStore(what, new(atomic.Int64))
		return n.(*atomic.Int64).Add(1) <= h.Count
	}
	return true
}

// hold answers nothing more until the client closes the connection (or the server shuts down).
func (s *server) hold(r *http.Request, what string, upgrade bool) {
	n := s.held.Add(1)
	// Read the body first: only then does the server watch the connection for the client going
	// away (and cancel the request's context when it does).
	io.Copy(io.Discard, r.Body)
	s.log.emit("held", map[string]any{"hold": n, "what": what, "path": r.URL.Path, "upgrade": upgrade})
	<-r.Context().Done()
	s.log.emit("held_closed", map[string]any{"hold": n, "what": what, "by_client": !s.closing.Load()})
}

// holdDownload sends half of `body` (announcing all of it), then holds.
func (s *server) holdDownload(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body[:len(body)/2])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.hold(r, "downloads", false)
}
