// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// A hostile server sends values that are malformed but look plausible: connection-settings offers
// with an endpoint no client should use or headers no client can send, packages whose download
// URL or headers are the same, and heartbeat intervals at the edges of what the protocol allows.
// A client must refuse the malformed ones, report them failed, and carry on.
//
// Every malformed endpoint and URL that could be reached points at the hostile listener, a port of
// its own that answers nothing: any connection to it means a client used a value it should have
// refused. A relative download URL, resolved against the server, lands on /packages/h0st1le/,
// which the server also logs as hostile.
type Hostile struct {
	// Seed picks the kind of every "any" entry and each kind's variant (which control character,
	// which bad scheme, and so on): a scenario offers the same values every run.
	Seed uint64 `json:"seed"`
	// Settings: connection-settings offers, one at a time, each in every response until the agent
	// answers it (APPLIED or FAILED) or AnswerWithinS passes.
	Settings []string `json:"settings"`
	// Packages: offered together, with a well-formed package after them (by name, the order the
	// packages are worked through).
	Packages []string `json:"packages"`
	// OfferOn: the message whose response carries the first settings offer (default 2).
	OfferOn int `json:"offer_on"`
	// AnswerWithinS: how long an offer waits for an answer before the next one (default 15).
	AnswerWithinS float64 `json:"answer_within_s"`
	// HoldS: an answered offer stays the current one until this long after it was first made,
	// so that it takes effect for a while (a heartbeat interval, say).
	HoldS float64 `json:"hold_s"`

	offers []hostileOffer
}

// hostileMarker is in every hostile path: a request for one was made with a value that should
// have been refused.
const hostileMarker = "h0st1le"

// hostileOffer is a connection-settings offer. Endpoint is a template (expand).
type hostileOffer struct {
	kind      string
	endpoint  string
	headers   [][2]string
	heartbeat uint64
	malformed bool
	hash      []byte
}

var settingsKinds = []string{"endpoint_space", "endpoint_bad_host", "endpoint_no_host", "endpoint_scheme",
	"endpoint_huge", "header_ctl", "header_name", "heartbeat_zero", "heartbeat_huge"}

var packageKinds = []string{"url_scheme", "url_relative", "url_space", "url_no_host", "header_ctl", "header_name"}

// Control characters no header value may hold (RFC 9110 section 5.5), each followed by what an
// injection would add.
var controls = []string{"\r\n", "\n", "\r", "\x00"}

// Header names that aren't tokens (RFC 9110 section 5.1).
var badNames = []string{"Bad Name", "Bad:Name", "Bad(Name)", ""}

// variants picks one of `from` for each value a scenario asks for: at random from the seed, but
// not one already used for the same kind while others remain.
type variants struct {
	rng  *mrand.Rand
	used map[string]map[string]bool
}

func pick[T any](v *variants, kind string, from []T) T {
	if v.used[kind] == nil {
		v.used[kind] = map[string]bool{}
	}
	var fresh []T
	for _, x := range from {
		if !v.used[kind][fmt.Sprint(x)] {
			fresh = append(fresh, x)
		}
	}
	if len(fresh) == 0 {
		fresh = from
	}
	x := fresh[v.rng.IntN(len(fresh))]
	v.used[kind][fmt.Sprint(x)] = true
	return x
}

// resolve picks the kinds and variants, and adds the hostile packages to the scenario's.
func (h *Hostile) resolve(sc *Scenario) error {
	rng := &variants{rng: mrand.New(mrand.NewPCG(h.Seed, 0x686f7374696c65)), used: map[string]map[string]bool{}}
	if h.OfferOn == 0 {
		h.OfferOn = 2
	}
	if h.AnswerWithinS == 0 {
		h.AnswerWithinS = 15
	}
	for i, kind := range h.Settings {
		if kind == "any" {
			kind = pick(rng, "any settings", settingsKinds)
		}
		n := i + 1
		path := fmt.Sprintf("/v1/%s/%d", hostileMarker, n)
		o := hostileOffer{kind: kind, malformed: true, endpoint: "{scheme}://{host}:{hport}" + path}
		switch kind {
		case "endpoint_space":
			o.endpoint += pick(rng, kind, []string{" x", "\tx", " "})
		case "endpoint_bad_host":
			o.endpoint = "{scheme}://" + pick(rng, kind, []string{"bad host", "bad_host!", "[not-an-ip]", "exa mple.com"}) +
				":{hport}" + path
		case "endpoint_no_host":
			o.endpoint = pick(rng, kind, []string{"{scheme}://:{hport}", "{scheme}://"}) + path
		case "endpoint_scheme":
			o.endpoint = pick(rng, kind, []string{"ftp", "file", "gopher"}) + "://{host}:{hport}" + path
		case "endpoint_huge":
			// Past the 8000 octets RFC 9110 (section 4.1) asks everyone to support.
			o.endpoint += "/" + strings.Repeat("a", pick(rng, kind, []int{70_000, 100_000, 65_543}))
		case "header_ctl":
			o.headers = [][2]string{{"X-Hostile", "v" + pick(rng, kind, controls) + "X-Injected: " + hostileMarker}}
		case "header_name":
			o.headers = [][2]string{{pick(rng, kind, badNames), hostileMarker}}
		case "heartbeat_zero":
			o = hostileOffer{kind: kind}
		case "heartbeat_huge":
			// The largest there is; one whose milliseconds overflow 64 bits; one past 2^53 (a
			// double's integers); one whose milliseconds overflow a signed 32-bit timer.
			o = hostileOffer{kind: kind, heartbeat: pick(rng, kind, []uint64{math.MaxUint64, math.MaxUint64/1000 + 1,
				1<<53 + 1, math.MaxInt32/1000 + 1})}
		default:
			return fmt.Errorf("settings: unknown kind %q", kind)
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d %s %q %q %d", n, o.kind, o.endpoint, o.headers, o.heartbeat)))
		o.hash = sum[:]
		h.offers = append(h.offers, o)
	}
	if len(h.Packages) == 0 {
		return nil
	}
	if sc.Packages == nil {
		sc.Packages = &Packages{}
	}
	for i, kind := range h.Packages {
		if kind == "any" {
			kind = pick(rng, "any package", packageKinds)
		}
		n := i + 1
		path := fmt.Sprintf("/packages/%s/%d", hostileMarker, n)
		p := Package{Name: fmt.Sprintf("h%d-%s", n, kind), Version: "1.0.0", Content: fmt.Sprintf("hostile %d", n),
			hostile: kind, url: "{scheme}://{host}:{hport}" + path,
			headers: [][2]string{{"Authorization", "Bearer packages"}}}
		switch kind {
		case "url_scheme":
			p.url = pick(rng, kind, []string{"ftp", "file", "gopher"}) + "://{host}:{hport}" + path
		case "url_relative":
			p.url = pick(rng, kind, []string{path, strings.TrimPrefix(path, "/"), "//{host}:{hport}" + path})
		case "url_space":
			p.url = pick(rng, kind, []string{p.url + " x", p.url + "\tx", "{scheme}://bad host:{hport}" + path})
		case "url_no_host":
			p.url = pick(rng, kind, []string{"{scheme}://:{hport}", "{scheme}://"}) + path
		case "header_ctl":
			p.headers = [][2]string{{"Authorization", "Bearer packages" + pick(rng, kind, controls) + "X-Injected: " + hostileMarker}}
		case "header_name":
			p.headers = append(p.headers, [2]string{pick(rng, kind, badNames), hostileMarker})
		default:
			return fmt.Errorf("packages: unknown kind %q", kind)
		}
		sc.Packages.Offer = append(sc.Packages.Offer, p)
	}
	// Well formed, and worked on after the others: a flow they wedge never gets to it.
	sc.Packages.Offer = append(sc.Packages.Offer, Package{Name: "zz-good", Version: "1.0.0", Content: "a good package"})
	return nil
}

// expand fills in a template: {scheme} and {host} as the agent reached the server at `reached`
// (a URL), {port} its port, and {hport} the hostile listener's.
func expand(tmpl, reached, hport string) string {
	scheme, rest, _ := strings.Cut(reached, "://")
	hostport, _, _ := strings.Cut(rest, "/")
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return strings.NewReplacer("{scheme}", scheme, "{host}", host, "{port}", port, "{hport}", hport).Replace(tmpl)
}

// shown is a value as logged: quoted (control characters visible) and cut short.
func shown(v string) string {
	const max = 160
	if len(v) > max {
		return strconv.Quote(v[:max]) + fmt.Sprintf("... (%d bytes)", len(v))
	}
	return strconv.Quote(v)
}

func shownHeaders(hs [][2]string) [][2]string {
	out := [][2]string{}
	for _, h := range hs {
		out = append(out, [2]string{shown(h[0]), shown(h[1])})
	}
	return out
}

// startHostile listens for connections to malformed endpoints, on the host the server listens
// on, and logs each (hostile_connect) with the first line it sends. It answers 404 and closes.
func (s *server) startHostile(listen string) (string, error) {
	return s.startTrap(listen, "hostile_connect")
}

// startTrap listens on a port of its own, on the host the server listens on, for connections no
// client should make, and logs each as `event` with the first line it sends. It answers 404 and
// closes.
func (s *server) startTrap(listen, event string) (string, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 512)
				n, _ := c.Read(buf)
				first, _, _ := strings.Cut(string(buf[:n]), "\r\n")
				// A header line a malformed value injected, if the request got that far.
				injected := strings.Contains(string(buf[:n]), "\nX-Injected: "+hostileMarker)
				s.log.emit(event, map[string]any{"remote": c.RemoteAddr().String(), "first_line": shown(first),
					"bytes": n, "injected": injected})
				c.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, nil
}

// hostileSettings: the connection-settings offer due in the response to message n (under s.mu),
// having taken the agent's answer to the one before from msg.
func (s *server) hostileSettings(msg *protobufs.AgentToServer, n int, url string, info map[string]any) *protobufs.ConnectionSettingsOffers {
	h := s.sc.Hostile
	if h == nil || len(h.offers) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostileNext < len(h.offers) {
		cur := h.offers[s.hostileNext]
		st := msg.ConnectionSettingsStatus
		if st != nil && !s.hostileAnswered && string(st.LastConnectionSettingsHash) == string(cur.hash) &&
			(st.Status == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED ||
				st.Status == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED) {
			s.log.emit("hostile_answered", map[string]any{"kind": cur.kind, "hash": fmt.Sprintf("%x", cur.hash),
				"status": st.Status.String(), "error": st.ErrorMessage})
			s.hostileAnswered = true
		}
		held := !s.hostileSince.IsZero() && time.Since(s.hostileSince).Seconds() >= h.HoldS
		if s.hostileAnswered && held {
			s.hostileNext++
			s.hostileSince, s.hostileAnswered = time.Time{}, false
		} else if !s.hostileAnswered && !s.hostileSince.IsZero() && time.Since(s.hostileSince).Seconds() > h.AnswerWithinS {
			s.log.emit("hostile_unanswered", map[string]any{"kind": cur.kind, "hash": fmt.Sprintf("%x", cur.hash)})
			s.hostileNext++
			s.hostileSince = time.Time{}
		}
	}
	if n < h.OfferOn || s.hostileNext >= len(h.offers) || msg.AgentDisconnect != nil {
		return nil
	}
	o := h.offers[s.hostileNext]
	if s.hostileSince.IsZero() {
		s.hostileSince = time.Now()
	}
	settings := &protobufs.OpAMPConnectionSettings{HeartbeatIntervalSeconds: o.heartbeat}
	endpoint := ""
	if o.endpoint != "" {
		endpoint = expand(o.endpoint, url, s.hostilePort)
		settings.DestinationEndpoint = endpoint
	}
	if o.headers != nil {
		settings.Headers = &protobufs.Headers{}
		for _, hd := range o.headers {
			settings.Headers.Headers = append(settings.Headers.Headers, &protobufs.Header{Key: hd[0], Value: hd[1]})
		}
	}
	info["hostile_offer"] = map[string]any{"kind": o.kind, "hash": fmt.Sprintf("%x", o.hash), "malformed": o.malformed,
		"endpoint": shown(endpoint), "headers": shownHeaders(o.headers), "heartbeat": o.heartbeat}
	return &protobufs.ConnectionSettingsOffers{Hash: o.hash, Opamp: settings}
}

// flagHostile logs every request made with a hostile value (hostile_request): one for a hostile
// path (a relative download URL, resolved against this server), or with a header that holds the
// marker (a header value that smuggled one in).
func (s *server) flagHostile(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := strings.Contains(r.URL.Path, hostileMarker) || strings.Contains(r.RequestURI, hostileMarker)
		for name, values := range r.Header {
			for _, v := range values {
				hit = hit || strings.Contains(name, hostileMarker) || strings.Contains(v, hostileMarker)
			}
		}
		if hit {
			s.log.emit("hostile_request", map[string]any{"method": r.Method, "path": shown(r.RequestURI),
				"headers": rawHeaders(r)})
		}
		next.ServeHTTP(w, r)
	})
}
