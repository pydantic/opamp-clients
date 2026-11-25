// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// acceptedWithin is whether c's Standing is accepted within d.
func acceptedWithin(c *Client, d time.Duration) bool {
	late := time.After(d)
	for {
		if c.Standing().Accepted {
			return true
		}
		select {
		case <-c.StandingChanged():
		case <-late:
			return false
		}
	}
}

func startStanding(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := startFull(context.Background(), Config{Mode: ModePlatform, Endpoint: endpoint, HeartbeatInterval: time.Hour}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return c
}

// A server that offers no credential takes the agent with its first answer, and only then: a ServerToAgent over HTTP (with
// the protobuf content type, or none) or WebSocket; not an error response, a page something on
// the way serves, a binary frame that isn't OpAMP, nor a WebSocket the server takes and closes
// unanswered, as one may for credentials it refuses.
func TestStandingAcceptedAtTheServersAnswer(t *testing.T) {
	answer, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: make([]byte, 16)})
	refusal, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: make([]byte, 16), ErrorResponse: &protobufs.ServerErrorResponse{
		Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest, ErrorMessage: "unknown key"}})
	wsSends := func(frame []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			_, _, _ = conn.ReadMessage()
		}
	}
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		ws       bool
		answered bool
	}{
		{"http protobuf", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(answer)
		}, false, true},
		{"http no content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write(answer)
		}, false, true},
		{"http page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
		}, false, false},
		{"http refused", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, false, false},
		{"http error response", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(refusal)
		}, false, false},
		{"ws answered", wsSends(append([]byte{0}, answer...)), true, true},
		{"ws error response", wsSends(append([]byte{0}, refusal...)), true, false},
		{"ws not opamp", wsSends([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}), true, false},
		{"ws closed unanswered", func(w http.ResponseWriter, r *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""), time.Now().Add(time.Second))
			_ = conn.Close()
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			endpoint := ts.URL
			if tc.ws {
				endpoint = "ws" + strings.TrimPrefix(ts.URL, "http")
			}
			c := startStanding(t, endpoint)
			wait := 3 * time.Second
			if got := acceptedWithin(c, wait); got != tc.answered {
				t.Fatalf("accepted within %s: %v, want %v", wait, got, tc.answered)
			}
		})
	}
}

// In core mode the core reads the server itself: the Standing is the zero one.
func TestStandingIsZeroInCoreMode(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, err := startFull(context.Background(), Config{Endpoint: ts.URL, HeartbeatInterval: time.Hour}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	if c.StandingChanged() != nil || c.Standing() != (Standing{}) {
		t.Fatal("a Standing in core mode")
	}
}

const standingURL = "wss://opamp.example.com/v1/opamp"

// bearer is the header a connection with the credential carries, with what the core adds.
func bearer(credential string) http.Header {
	return http.Header{"Authorization": {"Bearer " + credential}, "User-Agent": {"core"}, "Opamp-Instance-Uid": {"u"}}
}

// offerOf is the server's answer that offers credential at url.
func offerOf(url, credential string) *protobufs.ServerToAgent {
	op := &protobufs.OpAMPConnectionSettings{DestinationEndpoint: url}
	if credential != "" {
		op.Headers = &protobufs.Headers{Headers: []*protobufs.Header{{Key: "authorization", Value: "Bearer " + credential}}}
	}
	return &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{Opamp: op}}
}

func unavailableFor(d time.Duration) *protobufs.ServerToAgent {
	return &protobufs.ServerToAgent{ErrorResponse: &protobufs.ServerErrorResponse{
		Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable, ErrorMessage: "too many",
		Details: &protobufs.ServerErrorResponse_RetryInfo{RetryInfo: &protobufs.RetryInfo{RetryAfterNanoseconds: uint64(d)}}}}
}

// Standing, step by step: what the server says on connections with the configured credential (t)
// and those it offered (k1, k2).
func TestStanding(t *testing.T) {
	answer := &protobufs.ServerToAgent{}
	type step func(s *standing)
	on := func(credential string, f func(a *attempt)) step {
		return func(s *standing) { f(s.on(true, standingURL, bearer(credential))) }
	}
	says := func(credential string, m *protobufs.ServerToAgent) step {
		return on(credential, func(a *attempt) { a.message(m) })
	}
	closes := func(credential string, code int, why string) step {
		return on(credential, func(a *attempt) { a.closed(code, why) })
	}
	web := func(credential string, f func(a *attempt)) step {
		return func(s *standing) { f(s.on(false, standingURL, bearer(credential))) }
	}
	round := func(with, credential string) step {
		return on(with, func(a *attempt) { a.message(offerOf(standingURL, credential)); a.closed(1000, "") })
	}
	offer := func(on, credential string) step { return says(on, offerOf(standingURL, credential)) }
	refusal := "the server closed the connection (1008: no)"
	const bound = "The OpAMP instance UID is already bound to another Fleet installation."
	for name, tc := range map[string]struct {
		steps []step
		want  Standing
	}{
		"answered":               {[]step{says("t", answer)}, Standing{Accepted: true}},
		"offered":                {[]step{offer("t", "k1")}, Standing{Offers: 1}},
		"offered, then closed":   {[]step{round("t", "k1")}, Standing{Offers: 1, Rounds: 1}},
		"offered the same again": {[]step{round("t", "k1"), round("t", "k1")}, Standing{Offers: 1, Rounds: 2}},
		"offered, then served":   {[]step{offer("t", "k1"), says("t", answer)}, Standing{Accepted: true}},
		"claimed":                {[]step{round("t", "k1"), says("k1", answer)}, Standing{Accepted: true}},
		"claimed with the first": {[]step{round("t", "k1"), round("t", "k2"), says("k1", answer)}, Standing{Accepted: true}},
		"saved credential":       {[]step{says("k1", answer)}, Standing{Accepted: true}},
		"renewed on its own":     {[]step{round("t", "k1"), says("k1", answer), offer("k1", "k2")}, Standing{Accepted: true}},
		"renewal refused":        {[]step{says("k1", answer), offer("k1", "k2"), closes("k2", 1008, "no")}, Standing{Accepted: true}},
		"renewal refused, then its own": {[]step{says("k1", answer), offer("k1", "k2"), closes("k2", 1008, "no"), closes("k1", 1008, "principal revoked")},
			Standing{Dropped: "the server closed the connection (1008: principal revoked)"}},
		"dropped, answered":               {[]step{says("k1", answer), closes("k1", 1008, "x"), says("k1", answer)}, Standing{Accepted: true}},
		"dropped, offered":                {[]step{says("t", answer), closes("t", 1008, "x"), round("t", "k1")}, Standing{Offers: 1, Rounds: 1}},
		"served, offered, a server stops": {[]step{says("t", answer), on("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")); a.closed(1012, "") })}, Standing{Accepted: true}},
		"offered and lost, the claim refused": {[]step{on("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")); a.lost("EOF") }), closes("k1", 1008, "no"), on("t", func(a *attempt) { a.message(offerOf(standingURL, "k2")); a.lost("EOF") })},
			Standing{Offers: 2, Rounds: 2, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"renewed, then closed":       {[]step{says("k1", answer), round("k1", "k2")}, Standing{Accepted: true}},
		"offered once served":        {[]step{says("t", answer), offer("t", "k1")}, Standing{Accepted: true}},
		"enrolled again, not served": {[]step{says("t", answer), round("t", "k1")}, Standing{Offers: 1, Rounds: 1}},
		"served on one connection": {[]step{on("t", func(a *attempt) {
			a.message(offerOf(standingURL, "k1"))
			a.message(answer)
			a.closed(1000, "")
		})}, Standing{Accepted: true}},
		"closed, offered again": {[]step{round("t", "k1"), closes("k1", 1008, "no"), round("t", "k2")},
			Standing{Offers: 2, Rounds: 2, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"closed, offered, not closed": {[]step{round("t", "k1"), closes("k1", 1008, "no"), offer("t", "k2")},
			Standing{Offers: 2, Rounds: 1, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1}},
		"closed, offered the same": {[]step{round("t", "k1"), closes("k1", 1008, "no"), round("t", "k1")},
			Standing{Offers: 1, Rounds: 2, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"a saved one closed, offered again": {[]step{closes("k1", 1008, "no"), round("t", "k2")},
			Standing{Offers: 1, Rounds: 1, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"closed, then taken":  {[]step{round("t", "k1"), closes("k1", 1008, "no"), round("t", "k2"), says("k2", answer)}, Standing{Accepted: true}},
		"closed, then served": {[]step{round("t", "k1"), closes("k1", 1008, "no"), says("t", answer)}, Standing{Accepted: true}},
		"closed twice": {[]step{round("t", "k1"), closes("k1", 1008, "no"), offer("t", "k2"), closes("k2", 1008, "no")},
			Standing{Offers: 2, Rounds: 1, ClaimError: refusal, ClaimClosed: true, FailedClaims: 2}},
		"lost":            {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.lost("EOF") })}, Standing{Offers: 1, ClaimError: "EOF", FailedClaims: 1}},
		"closed normally": {[]step{offer("t", "k1"), closes("k1", 1000, "")}, Standing{Offers: 1, ClaimError: "the server closed the connection (1000)", ClaimClosed: true, FailedClaims: 1}},
		"its own close code": {[]step{offer("t", "k1"), closes("k1", 4001, "\x1b[2Jno")},
			Standing{Offers: 1, ClaimError: "the server closed the connection (4001: [2Jno)", ClaimClosed: true, FailedClaims: 1}},
		"come back later": {[]step{offer("t", "k1"), closes("k1", 1013, "")}, Standing{Offers: 1, ClaimError: "the server closed the connection (1013)", ClaimClosed: true, FailedClaims: 1}},
		"upgrade refused": {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.status(403, nil) })}, Standing{Offers: 1, ClaimError: "HTTP 403", ClaimClosed: true, FailedClaims: 1}},
		"HTTP 409":        {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.status(409, nil) })}, Standing{Offers: 1, ClaimError: "HTTP 409", ClaimClosed: true, FailedClaims: 1}},
		"HTTP 503":        {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.status(503, nil) })}, Standing{Offers: 1}},
		"identity taken": {[]step{offer("t", "k1"), closes("k1", 1008, bound)},
			Standing{Offers: 1, ClaimError: "the server closed the connection (1008: " + bound + ")", ClaimClosed: true, IdentityTaken: true, FailedClaims: 1}},
		"not for policy": {[]step{offer("t", "k1"), closes("k1", 1011, bound)},
			Standing{Offers: 1, ClaimError: "the server closed the connection (1011: " + bound + ")", ClaimClosed: true, FailedClaims: 1}},
		"taken, then closed otherwise":   {[]step{offer("t", "k1"), closes("k1", 1008, bound), closes("k1", 1008, "no")}, Standing{Offers: 1, ClaimError: refusal, ClaimClosed: true, FailedClaims: 2}},
		"taken, then lost":               {[]step{offer("t", "k1"), closes("k1", 1008, bound), on("k1", func(a *attempt) { a.lost("EOF") })}, Standing{Offers: 1, ClaimError: "EOF", FailedClaims: 2}},
		"taken, once answered":           {[]step{says("k1", answer), closes("k1", 1008, bound)}, Standing{Dropped: "the server closed the connection (1008: " + bound + ")"}},
		"taken, on the token":            {[]step{closes("t", 1008, bound)}, Standing{Refused: "the server closed the connection (1008: " + bound + ")"}},
		"the token refused":              {[]step{closes("t", 1008, "no")}, Standing{Refused: refusal}},
		"the token refused, then taken":  {[]step{closes("t", 1008, "no"), says("t", answer)}, Standing{Accepted: true}},
		"the token refused, offered":     {[]step{closes("t", 1008, "no"), offer("t", "k1")}, Standing{Offers: 1}},
		"the token refused, unavailable": {[]step{closes("t", 1008, "no"), says("t", unavailableFor(0))}, Standing{Error: "too many"}},
		"unavailable, the token refused": {[]step{says("t", unavailableFor(0)), closes("t", 1008, "no")}, Standing{Error: "too many", Refused: refusal}},
		"unavailable, then closed":       {[]step{on("t", func(a *attempt) { a.message(unavailableFor(0)); a.closed(1013, "later") })}, Standing{Error: "too many"}},
		"the token, going away":          {[]step{closes("t", 1001, "server draining")}, Standing{}},
		"taken, the token refused":       {[]step{says("t", answer), closes("t", 1008, "credentials revoked")}, Standing{Dropped: "the server closed the connection (1008: credentials revoked)"}},
		"taken again":                    {[]step{says("t", answer), closes("t", 1008, "credentials revoked"), says("t", answer)}, Standing{Accepted: true}},
		"taken, closed otherwise":        {[]step{says("k1", answer), closes("k1", 1001, "server draining")}, Standing{Accepted: true}},
		"taken, lost":                    {[]step{says("k1", answer), on("k1", func(a *attempt) { a.lost("EOF") })}, Standing{Accepted: true}},
		"the token refused over HTTP":    {[]step{on("t", func(a *attempt) { a.status(401, nil) })}, Standing{}},
		"closed once answered":           {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.message(answer); a.closed(1008, "principal revoked") })}, Standing{Dropped: "the server closed the connection (1008: principal revoked)"}},
		"unavailable, closed for policy": {[]step{on("t", func(a *attempt) { a.message(unavailableFor(0)); a.closed(1008, "x") })}, Standing{Error: "too many"}},
		"the key unavailable":            {[]step{offer("t", "k1"), on("k1", func(a *attempt) { a.message(unavailableFor(0)); a.closed(1013, "later") })}, Standing{Offers: 1, Error: "too many"}},
		"a heartbeat offer":              {[]step{says("t", &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{Opamp: &protobufs.OpAMPConnectionSettings{HeartbeatIntervalSeconds: 5}}})}, Standing{Accepted: true}},
		"offered what is in use":         {[]step{offer("t", "t")}, Standing{Accepted: true}},
		"offered past the bounds":        {[]step{round("t", strings.Repeat("x", maxOfferText))}, Standing{Offers: 1, Rounds: 1}},
		"past the bounds, served":        {[]step{offer("t", strings.Repeat("x", maxOfferText)), says("t", answer)}, Standing{Accepted: true}},
		"offered, a server stops":        {[]step{on("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")); a.closed(1001, "server draining") })}, Standing{Offers: 1, Rounds: 1}},
		"offered, lost":                  {[]step{on("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")); a.lost("EOF") })}, Standing{Offers: 1, Rounds: 1}},
		"served, offered, lost":          {[]step{says("t", answer), on("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")); a.lost("EOF") })}, Standing{Accepted: true}},
		"served, then the same offered": {[]step{says("t", answer), round("t", "k1"), closes("k1", 1008, "no"), round("t", "k1")},
			Standing{Offers: 1, Rounds: 2, ClaimError: refusal, ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"moved, and back": {[]step{says("t", offerOf("wss://other.example.com/v1/opamp", "")), func(s *standing) {
			s.on(true, "wss://other.example.com/v1/opamp", bearer("t")).message(offerOf(standingURL, ""))
		}, offer("t", "k1")}, Standing{Accepted: true}},
		"moved, and back, then enrolled": {[]step{says("t", offerOf("wss://other.example.com/v1/opamp", "")), func(s *standing) {
			s.on(true, "wss://other.example.com/v1/opamp", bearer("t")).message(offerOf(standingURL, ""))
		}, round("t", "k1")}, Standing{Offers: 1, Rounds: 1}},
		"another endpoint": {[]step{says("t", offerOf("wss://other.example.com/v1/opamp", ""))}, Standing{Offers: 1}},
		"another endpoint, taken there": {[]step{says("t", offerOf("wss://other.example.com/v1/opamp", "")), func(s *standing) {
			s.on(true, "wss://other.example.com/v1/opamp", bearer("t")).message(answer)
		}}, Standing{Accepted: true}},
		"error":             {[]step{says("t", unavailableFor(0))}, Standing{Error: "too many"}},
		"a blank error":     {[]step{says("t", &protobufs.ServerToAgent{ErrorResponse: &protobufs.ServerErrorResponse{Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable, ErrorMessage: "\x01"}})}, Standing{Error: "Unavailable"}},
		"error, then taken": {[]step{says("t", unavailableFor(time.Hour)), says("t", answer)}, Standing{Accepted: true}},
		"error, then offer": {[]step{says("t", unavailableFor(0)), offer("t", "k1")}, Standing{Offers: 1}},
		"upgrade told 429":  {[]step{on("t", func(a *attempt) { a.status(429, http.Header{}) })}, Standing{}},
		"HTTP: 429":         {[]step{web("t", func(a *attempt) { a.status(429, http.Header{}) })}, Standing{Error: "HTTP 429"}},
		"HTTP: 503":         {[]step{web("t", func(a *attempt) { a.status(503, http.Header{}) })}, Standing{}},
		"HTTP: offered":     {[]step{web("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")) })}, Standing{Offers: 1}},
		"HTTP: offered twice": {[]step{web("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")) }), web("t", func(a *attempt) { a.message(offerOf(standingURL, "k2")) })},
			Standing{Offers: 2, Rounds: 1}},
		"HTTP: refused, offered again": {[]step{web("t", func(a *attempt) { a.message(offerOf(standingURL, "k1")) }), web("k1", func(a *attempt) { a.status(409, nil) }), web("t", func(a *attempt) { a.message(offerOf(standingURL, "k2")) })},
			Standing{Offers: 2, Rounds: 1, ClaimError: "HTTP 409", ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
		"HTTP: a saved one refused, offered": {[]step{web("k1", func(a *attempt) { a.status(409, nil) }), web("t", func(a *attempt) { a.message(offerOf(standingURL, "k2")) })},
			Standing{Offers: 1, Rounds: 1, ClaimError: "HTTP 409", ClaimClosed: true, FailedClaims: 1, OfferedAgain: true}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStanding()
			s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}, "User-Agent": {"configured"}}, true)
			for _, step := range tc.steps {
				step(s)
			}
			got := s.now()
			got.RetryAt, got.Takes = time.Time{}, 0 // TestStandingRetryAt, TestStandingTakes
			if got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// Every reason a server that enrolls agents closes an offered credential's connection for with 1008: only the
// identity bound to another installation, to the letter, is final. The others pass, or may (a feed
// of revocations that reset closes every connection), or are the core's to act on (a credential
// that expired).
func TestStandingEveryPolicyClose(t *testing.T) {
	for reason, taken := range map[string]bool{
		"The OpAMP instance UID is already bound to another Fleet installation.":  true,
		"The OpAMP instance UID is already bound to another Fleet installation":   false,
		"the OpAMP instance UID is already bound to another Fleet installation.":  false,
		"The OpAMP instance UID is already bound to another Fleet installation. ": false,
		"This runtime credential cannot claim the Fleet installation.":            false,
		"The claim is malformed.":                   false,
		"unauthorized":                              false,
		"principal revoked":                         false,
		"scope revoked":                             false,
		"credentials revoked":                       false,
		"principal expired":                         false,
		"message not finished in time":              false,
		"the Fleet issuer refused the request: 422": false,
		"": false,
	} {
		s := newStanding()
		s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, true)
		s.on(true, standingURL, bearer("t")).message(offerOf(standingURL, "k1"))
		s.on(true, standingURL, bearer("k1")).closed(1008, reason)
		got := s.now()
		if got.IdentityTaken != taken || !got.ClaimClosed || got.ClaimError == "" || got.Accepted {
			t.Errorf("%q: %+v", reason, got)
		}
		// On the token's connection it isn't about the credential offered.
		s.on(true, standingURL, bearer("t")).closed(1008, reason)
		if got := s.now(); got.IdentityTaken != taken || got.Refused == "" {
			t.Errorf("%q on the token: %+v", reason, got)
		}
	}
}

// The wait the server tells, over WebSocket and over HTTP; one past an hour is an hour, as the
// core waits.
func TestStandingRetryAt(t *testing.T) {
	for name, tc := range map[string]struct {
		say  func(a *attempt)
		want time.Duration
	}{
		"UNAVAILABLE": {func(a *attempt) { a.message(unavailableFor(time.Hour)) }, time.Hour},
		"a Retry-After date": {func(a *attempt) {
			a.status(429, http.Header{"Retry-After": {time.Now().Add(30 * time.Minute).UTC().Format(http.TimeFormat)}})
		}, 30 * time.Minute},
		"too long": {func(a *attempt) {
			m := unavailableFor(0)
			m.ErrorResponse.GetRetryInfo().RetryAfterNanoseconds = 1<<64 - 1
			a.message(m)
		}, maxRetryWait},
		"HTTP 429":       {func(a *attempt) { a.status(429, http.Header{"Retry-After": {"120"}}) }, 2 * time.Minute},
		"HTTP, too long": {func(a *attempt) { a.status(429, http.Header{"Retry-After": {"4294967295"}}) }, maxRetryWait},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStanding()
			s.configure(standingURL, nil, true)
			tc.say(s.on(false, standingURL, bearer("t")))
			if got := time.Until(s.now().RetryAt); got < tc.want-time.Minute || got > tc.want {
				t.Fatalf("in %s, want %s", got, tc.want)
			}
		})
	}
}

// A client that takes no offers is accepted by an answer that makes one; a host with no
// configured headers tells its issued credential by the offer.
func TestStandingWithoutOffersOrHeaders(t *testing.T) {
	s := newStanding()
	s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, false)
	s.on(true, standingURL, bearer("t")).message(offerOf(standingURL, "k1"))
	if !s.now().Accepted {
		t.Fatalf("%+v", s.now())
	}
	s = newStanding()
	s.configure(standingURL, nil, true)
	s.on(true, standingURL, http.Header{}).message(offerOf(standingURL, "k1"))
	if want := (Standing{Offers: 1}); s.now() != want {
		t.Fatalf("%+v", s.now())
	}
	s.on(true, standingURL, bearer("k1")).message(&protobufs.ServerToAgent{})
	if !s.now().Accepted {
		t.Fatalf("%+v", s.now())
	}
}

// An error that isn't UNAVAILABLE tells no wait: the core doesn't wait for it.
func TestStandingOnlyUnavailableWaits(t *testing.T) {
	s := newStanding()
	s.configure(standingURL, nil, true)
	m := unavailableFor(time.Hour)
	m.ErrorResponse.Type = protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest
	s.on(true, standingURL, bearer("t")).message(m)
	if got := s.now(); got.Error != "too many" || !got.RetryAt.IsZero() {
		t.Fatalf("%+v", got)
	}
}

// What the server said of earlier connections no longer holds once one stays open in silence; on
// one it answered with an error it does.
func TestStandingOfAQuietConnection(t *testing.T) {
	for _, answered := range []bool{false, true} {
		s := newStanding()
		s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, true)
		s.on(true, standingURL, bearer("t")).closed(1011, "")
		if s.now().Refused == "" {
			t.Fatal("not refused")
		}
		a := s.on(true, standingURL, bearer("t"))
		end := a.opened()
		if answered {
			a.message(unavailableFor(0))
		}
		time.Sleep(openFor + time.Second)
		end()
		if got := s.now(); (got == Standing{}) == answered {
			t.Fatalf("answered=%v: %+v", answered, got)
		}
	}
}

// The offers kept to tell the connections made with them hold no credential, and are bounded.
func TestStandingKeepsNoCredentials(t *testing.T) {
	s := newStanding()
	s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, true)
	for i := range 3 * maxOffersKept {
		s.on(true, standingURL, bearer("t")).message(offerOf(standingURL, fmt.Sprint("secret-", i)))
	}
	if len(s.offers) != maxOffersKept || strings.Contains(fmt.Sprintf("%+v", s.offers), "secret") {
		t.Fatalf("%d kept", len(s.offers))
	}
	if n := s.now().Offers; n != 3*maxOffersKept {
		t.Fatalf("%d offers", n)
	}
	if got := printable(strings.Repeat("é", 1000)); len([]rune(got)) != 303 {
		t.Fatalf("%d characters", len([]rune(got)))
	}
}

// An endpoint written another way is the same endpoint; another host, port, path or query isn't.
func TestSameEndpoint(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"wss://h/v1", "wss://h/v1", true},
		{"WSS://H:443/v1", "wss://h/v1", true},
		{"ws://h", "ws://h:80/", true},
		{"wss://u:p@h/v1", "wss://h/v1", true},
		{"wss://h/v1", "ws://h/v1", false},
		{"wss://h/v1", "wss://h:444/v1", false},
		{"wss://h/v1", "wss://g/v1", false},
		{"wss://h/v1", "wss://h/v2", false},
		{"wss://h/v1?a", "wss://h/v1", false},
		{"wss://h/%zz", "wss://h/", false},
	} {
		if got := sameEndpoint(tc.a, tc.b); got != tc.same {
			t.Errorf("%s and %s: %v", tc.a, tc.b, got)
		}
	}
}

// User info in the configured endpoint, sent as a header, is configured; another header isn't.
func TestStandingUserInfoIsConfigured(t *testing.T) {
	s := newStanding()
	s.configure("wss://u:p@opamp.example.com/v1/opamp", nil, true)
	if s.on(true, standingURL, bearer("x")).issued {
		t.Fatal("user info read as issued")
	}
	if !s.on(true, standingURL, http.Header{"X-Key": {"k"}}).issued {
		t.Fatal("another header read as configured")
	}
}

// A configured header as a config writes it: a connection with it is the configured one.
func TestStandingConfiguredHeaderInAnyCase(t *testing.T) {
	for _, key := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		s := newStanding()
		s.configure(standingURL, http.Header{key: {"Bearer t"}}, true)
		if s.on(true, standingURL, bearer("t")).issued || !s.on(true, standingURL, bearer("k1")).issued {
			t.Errorf("%s", key)
		}
	}
}

// A failed try no longer holds either, once a connection stays open in silence; and that it does is
// told, though nothing changes by it.
func TestStandingOfAQuietConnectionAfterAFailedTry(t *testing.T) {
	s := newStanding()
	s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, true)
	s.on(true, standingURL, bearer("t")).message(offerOf(standingURL, "k1"))
	s.on(true, standingURL, bearer("k1")).closed(1008, identityTaken)
	if !s.now().IdentityTaken {
		t.Fatal("not taken")
	}
	defer s.on(true, standingURL, bearer("k1")).opened()()
	time.Sleep(openFor + time.Second)
	if got, want := s.now(), (Standing{Offers: 1}); got != want {
		t.Fatalf("%+v", got)
	}
	fresh := newStanding()
	fresh.configure(standingURL, nil, true)
	defer fresh.on(true, standingURL, nil).opened()()
	select {
	case <-fresh.changed:
	case <-time.After(openFor + time.Second):
		t.Fatal("not told")
	}
}

// Takes counts the connections the server took the agent on, once each; over HTTP, once until it is
// taken back.
func TestStandingTakes(t *testing.T) {
	answer := &protobufs.ServerToAgent{}
	s := newStanding()
	s.configure(standingURL, http.Header{"Authorization": {"Bearer t"}}, true)
	takes := func(want int) {
		t.Helper()
		if got := s.now().Takes; got != want {
			t.Fatalf("%d, want %d", got, want)
		}
	}
	a := s.on(true, standingURL, bearer("t"))
	a.message(answer)
	a.message(answer)
	takes(1)
	a.lost("EOF")
	s.on(true, standingURL, bearer("t")).message(answer)
	takes(2)
	for range 3 {
		s.on(false, standingURL, bearer("t")).message(answer)
	}
	takes(2)
	b := s.on(true, standingURL, bearer("t"))
	b.message(answer)
	b.closed(1008, "x")
	takes(3)
	s.on(false, standingURL, bearer("t")).message(answer)
	takes(4)
}
