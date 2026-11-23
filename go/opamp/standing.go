// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Standing is how the agent stands with its server, from what the client has seen of it since
// Start: whether the server took the agent, and what became of the credentials it offered.
//
// A server can enroll an agent: the agent comes with a credential many agents share, and the
// server answers with an offer of the agent's own (an OpAMP connection settings offer), closes the
// connection, and takes the agent only when it comes with that. An answer that only offers the
// credential takes nothing in, and the server may still refuse the agent on it.
type Standing struct {
	// Accepted: the server has answered (a ServerToAgent that isn't an error response) without
	// offering a credential, or on a credential it issued (one it offered, or one saved before this
	// run). It is taken back when the server closes a connection for policy (1008), until it answers
	// again, and when it closes one (not to stop, or to be come back to) made with the configured
	// credential on which the last it said was an offer of another. (A server that offers one, and answers the
	// connection's next message plainly, serves the agent on the configured one as far as it can be
	// told: it reads as accepted.)
	Accepted bool
	// Takes counts the connections the server took the agent on (the first answer to an HTTP agent):
	// it tells an agent taken once from one taken, dropped and taken again.
	Takes int
	// Dropped is how the server last closed, for policy, a connection it had taken the agent on:
	// until it answers again, or keeps a connection open.
	Dropped string
	// Offers is how many different credentials the server has offered the agent since it last took
	// it, and Rounds how many times it offered one and didn't serve the agent: the connection ended,
	// or over HTTP it offered another at the next request.
	Offers int
	Rounds int
	// ClaimError is how the last try with a credential the server issued ended without an answer:
	// "" if none did. ClaimClosed: the server ended it (a Close, or an HTTP answer), as against a
	// connection that failed or was lost. IdentityTaken: it said that the agent's identity is bound
	// to another installation (identityTaken, over WebSocket). Nothing else it says then is final:
	// servers close for policy, and answer 401, for what passes too. FailedClaims counts them.
	ClaimError    string
	ClaimClosed   bool
	IdentityTaken bool
	FailedClaims  int
	// OfferedAgain: there was a round since ClaimError.
	OfferedAgain bool
	// Unreached is the host (and port) of the endpoint that the server offered a credential for and that
	// couldn't be reached, once the agent makes no further connection for it (Config.KeepUnreached).
	Unreached string
	// Refused is how the server last closed, unanswered, a connection made with the configured
	// credential (one it doesn't take, say): until it answers, or keeps a connection open.
	Refused string
	// Error is the server's last error response (or HTTP 429), until it answers otherwise or keeps
	// a connection open, and RetryAt when it asked the agent to come back, if it did (UNAVAILABLE,
	// 429): the core waits until then.
	Error   string
	RetryAt time.Time
}

// identityTaken is the reason of the Close for policy (1008) a server that enrolls agents sends a
// connection whose agent's identity is another installation's. Its other reasons for 1008 pass, or
// may: only the text tells them apart, so it is matched whole, and the server keeps it.
const identityTaken = "The OpAMP instance UID is already bound to another Fleet installation."

const (
	// maxRetryWait bounds the wait RetryAt tells: the core waits no longer.
	maxRetryWait = time.Hour
	// openFor is how long a connection is open, without a word, before what the server said of the
	// ones before it no longer holds.
	openFor = 10 * time.Second
	// maxOffersKept bounds the offers kept to tell the connections made with them, and
	// maxOfferHeaders and maxOfferText what an offer may hold, as the core's do.
	maxOffersKept   = 64
	maxOfferHeaders = 64
	maxOfferText    = 8000
)

// standing keeps Standing. Its methods do nothing on a nil standing.
type standing struct {
	mu      sync.Mutex
	st      Standing
	changed chan struct{} // holds at most one
	// endpoint and header are the configured ones, set before the client starts. takesOffers: the
	// client takes offers of connection settings.
	endpoint    string
	header      http.Header
	takesOffers bool
	offers      []offered
	// claims is Standing.FailedClaims at the server's last message.
	claims int
	// taken is the credential the server last took the agent on.
	taken digest
	// keepUnreached and keepSaved are Config's, set before the client starts; unreached is the host of the
	// last try with a credential the server issued, if it made no connection.
	keepUnreached, keepSaved bool
	unreached                string
	// settling is, while the server's Close is awaited (awaitClose), closed once it isn't any more.
	settling chan struct{}
}

// offered is an offer the server made: where to, and the headers (a digest of each value, so the
// credentials aren't kept).
type offered struct {
	id       digest
	endpoint string
	header   map[string]digest
}

type digest = [sha256.Size]byte

func newStanding() *standing { return &standing{changed: make(chan struct{}, 1)} }

// configure sets what the agent is configured with.
func (s *standing) configure(endpoint string, header http.Header, takesOffers bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoint, s.header, s.takesOffers = endpoint, http.Header{}, takesOffers
	for k, v := range header { // as written in a config
		k = http.CanonicalHeaderKey(k)
		s.header[k] = append(s.header[k], v...)
	}
}

func (s *standing) now() Standing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// set changes the Standing, and tells of it if it did change. With s.mu held.
func (s *standing) set(change func(*Standing)) {
	was := s.st
	change(&s.st)
	if s.st != was {
		s.tell()
	}
}

// tell says that the Standing changed.
func (s *standing) tell() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// attempt is one connection (ws) or request: where it goes and the headers it carries.
type attempt struct {
	s      *standing
	ws     bool
	url    string
	header http.Header
	// issued: it is made with settings the server issued, not the configured ones.
	issued   bool
	answered bool // the server said something on it
	took     bool // it took the agent on it
	offered  bool // it only offered a credential on it
}

// on is the attempt made to an endpoint with header.
func (s *standing) on(ws bool, to string, header http.Header) *attempt {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &attempt{s: s, ws: ws, url: to, header: header}
	if !s.takesOffers {
		return a // nothing is issued
	}
	// Not to the configured endpoint, or not with the configured headers and no others: what it
	// goes by was issued (offered headers replace the configured ones).
	a.issued = !sameEndpoint(to, s.endpoint) || !carries(header, digests(s.header)) || s.adds(header)
	for _, o := range s.offers {
		if len(o.header) == 0 && sameEndpoint(o.endpoint, s.endpoint) {
			continue // a move back: nothing tells it from the configured settings
		}
		a.issued = a.issued || to == o.endpoint && carries(header, o.header)
	}
	return a
}

// notCredentials are the headers the core sets itself, or leaves to the HTTP stack: a configured
// or offered one of them isn't what is sent.
var notCredentials = regexp.MustCompile(`(?i)^(accept-encoding|connection|content-length|content-type|expect|host|keep-alive|` +
	`opamp-instance-uid|proxy-connection|te|trailer|transfer-encoding|upgrade|user-agent|sec-websocket-.*)$`)

func digestOf(values []string) digest {
	return sha256.Sum256([]byte(strings.TrimSpace(strings.Join(values, ","))))
}

// digests are the values in h, but for those the client sets itself.
func digests(h http.Header) map[string]digest {
	out := make(map[string]digest, len(h))
	for k, v := range h {
		if !notCredentials.MatchString(k) {
			out[k] = digestOf(v)
		}
	}
	return out
}

// carries is whether header has every value of want.
func carries(header http.Header, want map[string]digest) bool {
	for k, d := range want {
		if digestOf(header.Values(k)) != d {
			return false
		}
	}
	return true
}

// adds is whether header has one that isn't configured. (User info in the configured endpoint is
// sent as one.) With s.mu held.
func (s *standing) adds(header http.Header) bool {
	for k := range digests(header) {
		u, err := url.Parse(s.endpoint)
		if s.header.Values(k) == nil && (k != "Authorization" || err != nil || u.User == nil) {
			return true
		}
	}
	return false
}

// sameEndpoint is whether two URLs are the same endpoint, however they are written.
func sameEndpoint(a, b string) bool {
	if a == b {
		return true
	}
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if s := strings.ToLower(u.Scheme); s == "https" || s == "wss" {
			return "443"
		}
		return "80"
	}
	path := func(u *url.URL) string { return max(u.EscapedPath(), "/") }
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Hostname(), ub.Hostname()) && port(ua) == port(ub) &&
		path(ua) == path(ub) && ua.RawQuery == ub.RawQuery
}

// message is what the server sent on the attempt.
func (a *attempt) message(m *protobufs.ServerToAgent) {
	if a == nil {
		return
	}
	if e := m.GetErrorResponse(); e != nil {
		var wait time.Duration
		if e.GetType() == protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable {
			wait = time.Duration(min(e.GetRetryInfo().GetRetryAfterNanoseconds(), uint64(maxRetryWait)))
		}
		what := printable(e.GetErrorMessage())
		if what == "" {
			what = strings.TrimPrefix(e.GetType().String(), "ServerErrorResponseType_")
		}
		a.unavailable(what, wait)
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.answered = true
	a.reached()
	o, isOffer := a.offer(m)
	fresh := isOffer && a.s.keep(o)
	a.offered = isOffer && !a.issued
	a.s.set(func(st *Standing) {
		st.Error, st.RetryAt, st.Refused, st.Dropped = "", time.Time{}, "", ""
		switch {
		case !a.offered:
			takes := st.Takes
			if !a.took && (a.ws || !st.Accepted) {
				takes++
			}
			a.took, a.s.taken = true, a.credential()
			*st = Standing{Accepted: true, Takes: takes}
		case fresh && !st.Accepted:
			st.Offers++
			if !a.ws && (st.Offers > 1 || st.ClaimError != "") {
				st.round() // the request before got one too, or the one it had failed
			}
		case isOffer && !a.ws && !st.Accepted && st.FailedClaims > a.s.claims:
			st.Rounds++ // one it was offered before, which was tried and failed since
		}
		a.s.claims = st.FailedClaims
	})
}

// credential tells the attempt's endpoint and credentials from others.
func (a *attempt) credential() digest {
	h := sha256.New()
	h.Write([]byte(a.url))
	d := digests(a.header)
	for _, k := range slices.Sorted(maps.Keys(d)) {
		v := d[k]
		h.Write([]byte("\n" + k + ":"))
		h.Write(v[:])
	}
	return digest(h.Sum(nil))
}

// round: the server offered a credential and didn't serve the agent.
func (st *Standing) round() {
	st.Rounds++
	st.Offers = max(st.Offers, 1) // one offered while accepted isn't counted
	st.OfferedAgain = st.ClaimError != ""
}

// keep keeps o, and says whether it is new (one past the bounds isn't kept). With s.mu held.
func (s *standing) keep(o offered) bool {
	if o.header == nil {
		return true
	}
	for _, k := range s.offers {
		if k.id == o.id {
			return false
		}
	}
	if len(s.offers) == maxOffersKept {
		s.offers = s.offers[1:]
	}
	s.offers = append(s.offers, o)
	return true
}

// offer is the credential m offers, if it offers one: new headers, or another endpoint. (An offer
// may only set the heartbeat, or name what is in use.) One past the core's bounds, which the core
// refuses, is an offer with no headers kept.
func (a *attempt) offer(m *protobufs.ServerToAgent) (offered, bool) {
	op := m.GetConnectionSettings().GetOpamp()
	if op == nil || !a.s.takesOffers {
		return offered{}, false
	}
	if len(op.GetHeaders().GetHeaders()) > maxOfferHeaders || len(op.GetDestinationEndpoint()) > maxOfferText {
		return offered{}, true
	}
	header := http.Header{}
	for _, h := range op.GetHeaders().GetHeaders() {
		if len(h.GetKey())+len(h.GetValue()) > maxOfferText {
			return offered{}, true
		}
		header.Add(h.GetKey(), h.GetValue())
	}
	o := offered{endpoint: op.GetDestinationEndpoint(), header: digests(header)}
	if o.endpoint == "" {
		o.endpoint = a.url
	}
	if o.endpoint == a.url && carries(a.header, o.header) {
		return offered{}, false
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(op)
	o.id = sha256.Sum256(b)
	return o, true
}

// unavailable: the server told the agent why it doesn't take it now, and how long to stay away
// (0 if it didn't say). That is neither an answer nor a refusal of the credential.
func (a *attempt) unavailable(what string, wait time.Duration) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.answered = true
	a.reached()
	a.s.set(func(st *Standing) {
		st.Error, st.RetryAt, st.Refused = what, time.Time{}, ""
		if wait > 0 {
			st.RetryAt = time.Now().Add(wait)
		}
	})
}

// status: the server's HTTP answer to the attempt wasn't an OpAMP message (a WebSocket upgrade
// refused, say).
func (a *attempt) status(code int, h http.Header) {
	if a != nil {
		a.s.mu.Lock()
		a.reached()
		a.s.mu.Unlock()
	}
	switch {
	case a == nil || code == http.StatusServiceUnavailable || a.ws && code == http.StatusTooManyRequests:
		// The connection reads failed, and will pass.
	case code == http.StatusTooManyRequests:
		a.unavailable(fmt.Sprintf("HTTP %d", code), retryAfter(h))
	default:
		a.ended(true, fmt.Sprintf("HTTP %d", code))
	}
}

// retryAfter is the wait the Retry-After header tells (seconds, or a date), 0 if none.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
		return min(time.Duration(secs)*time.Second, maxRetryWait)
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(0, min(time.Until(at), maxRetryWait))
	}
	return 0
}

// opened: the attempt's connection is open. What the server said before it no longer holds once it
// stays open a while; the function returned is called when it ends.
func (a *attempt) opened() (ended func()) {
	if a == nil {
		return func() {}
	}
	a.s.mu.Lock()
	a.reached()
	a.s.mu.Unlock()
	t := time.AfterFunc(openFor, func() {
		a.s.mu.Lock()
		defer a.s.mu.Unlock()
		// A server may keep the connection it answered (with an error, say) open until the wait it
		// told is over.
		if !a.answered {
			a.s.set(func(st *Standing) {
				*st = Standing{Accepted: st.Accepted, Takes: st.Takes, Offers: st.Offers, Rounds: st.Rounds}
			})
			a.s.tell() // of the connection open in silence, too
		}
	})
	return func() { t.Stop() }
}

// closed: the server closed the connection.
func (a *attempt) closed(code int, reason string) {
	if a == nil {
		return
	}
	taken := code == websocket.ClosePolicyViolation && reason == identityTaken
	detail := fmt.Sprintf("the server closed the connection (%d)", code)
	if reason = printable(reason); reason != "" {
		detail = fmt.Sprintf("the server closed the connection (%d: %s)", code, reason)
	}
	stopping := false // a server that stops, or says to come back
	switch code {
	case websocket.CloseGoingAway, websocket.CloseServiceRestart, websocket.CloseTryAgainLater:
		stopping = true
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.set(func(st *Standing) {
		switch {
		case a.offered:
			if !stopping {
				st.Accepted = false
			}
			a.unserved(st)
		case code == websocket.ClosePolicyViolation && st.Accepted && (a.took || a.credential() == a.s.taken):
			// Taken, and then closed for policy: it isn't now, whatever it is told next. (A server may
			// answer an agent it has just deleted.) A renewal it doesn't take is another credential.
			*st = Standing{Takes: st.Takes, Dropped: detail}
		case a.issued:
			a.failed(st, true, taken, detail)
		case !a.answered && !st.Accepted && !stopping:
			// Unanswered, with the configured credential: one it doesn't take.
			st.Refused = detail
		}
	})
}

// lost: the attempt failed, or its connection did.
func (a *attempt) lost(detail string) { a.ended(false, printable(detail)) }

// reached: the attempt made a connection. With s.mu held.
func (a *attempt) reached() {
	if a.issued {
		a.s.unreached = ""
	}
}

// failedToConnect: the attempt made no connection.
func (a *attempt) failedToConnect(detail string) {
	if a == nil {
		return
	}
	if a.issued {
		host := a.url
		if u, err := url.Parse(a.url); err == nil {
			host = u.Host
		}
		a.s.mu.Lock()
		a.s.unreached = printable(host)
		a.s.mu.Unlock()
	}
	a.lost(detail)
}

// settleFor is how long the server's Close of a connection it offered a credential on is awaited.
const settleFor = time.Second

// settles is whether the server's Close of the attempt's connection is awaited before ours is sent: for an agent
// it doesn't serve yet (Config.KeepUnreached) and only offered a credential on it. Then settled is to be called.
func (a *attempt) settles() bool {
	if a == nil {
		return false
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if !a.s.keepUnreached || !a.offered || a.s.st.Accepted || a.s.settling != nil {
		return false
	}
	a.s.settling = make(chan struct{})
	return true
}

func (a *attempt) settled() {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	close(a.s.settling)
	a.s.settling = nil
}

// heldBack is whether the attempt, with the configured credential, is not to be made (Config.KeepUnreached):
// the server would offer another, and the last was never tried.
func (a *attempt) heldBack() bool {
	if a == nil {
		return false
	}
	s := a.s
	s.mu.Lock()
	settling := s.settling
	s.mu.Unlock()
	if settling != nil && !a.issued {
		<-settling // settleFor at most
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.issued || !s.keepUnreached || s.unreached == "" || s.st.Accepted || s.st.Rounds == 0 && !s.keepSaved {
		return false
	}
	s.set(func(st *Standing) { st.Unreached = s.unreached })
	return true
}

// ended: the attempt is over, by the server's word (closed) or not.
func (a *attempt) ended(closed bool, detail string) {
	if a == nil {
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.set(func(st *Standing) {
		if a.ws && a.offered {
			a.unserved(st)
			return
		}
		a.failed(st, closed, false, detail)
	})
}

// unserved: the attempt ended with the server's last word the offer of a credential: a round, unless
// it serves the agent otherwise (a connection may be lost, or the server stop, at any word).
func (a *attempt) unserved(st *Standing) {
	if !st.Accepted {
		st.round()
	}
}

// failed: the attempt ended. One with a credential the server issued failed, if it wasn't answered.
func (a *attempt) failed(st *Standing, closed, taken bool, detail string) {
	if a.issued && !a.answered && !st.Accepted {
		st.ClaimError, st.ClaimClosed, st.IdentityTaken, st.OfferedAgain = detail, closed, taken, false
		st.FailedClaims++
	}
}

// printable is what the server (or the network) said, fit to show on one line: at most 300
// characters, none of them a control character.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return strings.TrimSpace(s)
}

// httpMessage is the ServerToAgent in an HTTP response, nil if there is none: a success, with the
// protobuf content type the spec has the server send (or none), and a body that is one. A page
// that something on the way serves (a proxy's, a portal's) isn't.
func httpMessage(resp *http.Response, body []byte) *protobufs.ServerToAgent {
	if resp.StatusCode/100 != 2 || len(body) == 0 {
		return nil
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	ct = strings.TrimSpace(ct)
	if ct != "" && !strings.EqualFold(ct, "application/x-protobuf") {
		return nil
	}
	return serverToAgent(body)
}

// wsMessage is the ServerToAgent in a binary WebSocket message, nil if there is none: OpAMP's
// header (a varint, 0), then the message.
func wsMessage(data []byte) *protobufs.ServerToAgent {
	header, n := protowire.ConsumeVarint(data)
	if n <= 0 || header != 0 {
		return nil
	}
	return serverToAgent(data[n:])
}

func serverToAgent(b []byte) *protobufs.ServerToAgent {
	m := &protobufs.ServerToAgent{}
	if proto.Unmarshal(b, m) != nil {
		return nil
	}
	return m
}
