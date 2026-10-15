// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

// Package downloads answered with redirects (a package's "redirect"; docs/packages.md,
// "Redirects"). The file is offered at /packages/<name>/<version> on its listener, which answers
// with the first redirect; each hop n is at /redirect/<name>/<version>/<n>, and the last one
// serves the file. Three more listeners, on the host the server listens on:
//
//   - "other": plain HTTP on a port of its own, another origin. It serves whatever is asked of it,
//     credentials or not (a storage host with signed URLs).
//   - "tls": HTTPS with a certificate from a CA made at start (download-ca.pem beside the log,
//     handed to the driver as DRIVER_CA_FILE).
//   - the trap: every redirect a client must refuse points at a port that answers nothing. Any
//     connection to it is logged (redirect_trap_connect): the client followed.
//
// Every request is logged (package_download) with the listener it came to, the listener of the
// file's own origin, its hop, the headers it carried, and what it was answered.

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// Redirect: how a package's download is redirected.
type Redirect struct {
	// From: the listener the offer's URL names, the file's own origin: "main" (default) or "tls".
	From string `json:"from"`
	// Hops: where each redirect points, in order. Followed: "same" (an absolute URL on the same
	// listener), "relative" (a path-absolute Location: /redirect/...), "relative_dots" (a
	// relative path: ../../redirect/...), "main", "other" and "tls" (an absolute URL on that
	// listener). Refused, and the last hop: "downgrade" (from "tls" to the trap over http),
	// "trap" (the sixth hop or later: one too many), and malformed Locations to the trap:
	// "bad_host" (a space in the host), "bad_scheme" (ftp) and "userinfo".
	Hops []string `json:"hops"`
}

// maxRedirects: the most a download follows (docs/packages.md, as the core follows them).
const maxRedirects = 5

// redirectStatuses: the statuses a redirect answers with, taken in turn, so a chain uses them all.
var redirectStatuses = []int{302, 301, 307, 308, 303}

var followedHops = map[string]bool{"same": true, "relative": true, "relative_dots": true, "main": true,
	"other": true, "tls": true}
var refusedHops = map[string]bool{"downgrade": true, "trap": true, "bad_host": true, "bad_scheme": true,
	"userinfo": true}

// refused: the download must be refused at its last hop.
func (r *Redirect) refused() bool {
	return len(r.Hops) > 0 && refusedHops[r.Hops[len(r.Hops)-1]]
}

// from: the listener of the file's own origin.
func (r *Redirect) from() string {
	if r == nil || r.From == "" {
		return "main"
	}
	return r.From
}

// check: the chain is one the scenario can judge: refused hops come last, "downgrade" leaves an
// https listener, "trap" is past the limit, and nothing else is.
func (r *Redirect) check() error {
	if f := r.from(); f != "main" && f != "tls" {
		return fmt.Errorf("from must be main or tls, not %q", f)
	}
	at := r.from()
	for i, h := range r.Hops {
		last := i == len(r.Hops)-1
		switch {
		case refusedHops[h] && !last:
			return fmt.Errorf("hop %d (%s) is refused: it must be the last", i+1, h)
		case h == "downgrade" && at != "tls":
			return fmt.Errorf("hop %d: a downgrade must come from the tls listener", i+1)
		case h == "trap" && i < maxRedirects:
			return fmt.Errorf("hop %d: trap is for the hop past %d", i+1, maxRedirects)
		case !refusedHops[h] && i >= maxRedirects:
			return fmt.Errorf("hop %d (%s): past %d redirects, only trap", i+1, h, maxRedirects)
		case !refusedHops[h] && !followedHops[h]:
			return fmt.Errorf("hop %d: unknown kind %q", i+1, h)
		}
		if h == "main" || h == "other" || h == "tls" {
			at = h
		}
	}
	return nil
}

// usesTLS: the chain needs the tls listener.
func (r *Redirect) usesTLS() bool {
	if r.from() == "tls" {
		return true
	}
	for _, h := range r.Hops {
		if h == "tls" {
			return true
		}
	}
	return false
}

// downloadPorts: the listeners of redirecting downloads, by name.
type downloadPorts struct {
	main, other, tls, trap string
}

// startRedirects starts the listeners the scenario's redirects need (their ports go to s.ports),
// and returns the CA file the driver is to trust if one of them is the tls listener.
func (s *server) startRedirects(listen, dir string, handler func(listener string) http.Handler) (string, error) {
	var redirects, tlsNeeded bool
	for _, list := range [][]Package{s.sc.Packages.Offer, s.sc.Packages.Then} {
		for _, p := range list {
			if p.Redirect != nil {
				redirects = true
				tlsNeeded = tlsNeeded || p.Redirect.usesTLS()
			}
		}
	}
	if !redirects {
		return "", nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if s.ports.trap, err = s.startTrap(listen, "redirect_trap_connect"); err != nil {
		return "", err
	}
	other, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	_, s.ports.other, _ = net.SplitHostPort(other.Addr().String())
	go (&http.Server{Handler: s.withRawHead(handler("other")), ConnContext: connContext}).Serve(rawListener{other})
	if !tlsNeeded {
		return "", nil
	}
	ca := newCA("opamp-clients conformance download CA")
	cert, _ := ca.sign("conformance-downloads", serverHosts, x509.ExtKeyUsageServerAuth)
	caFile := filepath.Join(dir, "download-ca.pem")
	if err := pemFile(caFile, "CERTIFICATE", ca.der); err != nil {
		return "", err
	}
	raw, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	_, s.ports.tls, _ = net.SplitHostPort(raw.Addr().String())
	cfg := tlsConfig(cert)
	go (&http.Server{Handler: handler("tls")}).Serve(newLoggingListener(raw, s, cfg))
	return caFile, nil
}

// downloadPath reads a download's path: /packages/<name>/<version> (hop 0) or
// /redirect/<name>/<version>/<n> (hop n).
func downloadPath(path string) (key string, hop int, ok bool) {
	if rest, found := strings.CutPrefix(path, "/packages/"); found {
		return rest, 0, true
	}
	rest, found := strings.CutPrefix(path, "/redirect/")
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n < 1 {
		return "", 0, false
	}
	return rest[:i], n, true
}

// location: where hop n+1 of p's chain points, from a request to `listener` for `path` that
// reached the server as `reqHost`.
func (s *server) location(p *Package, n int, listener, path, reqHost string) string {
	host, _, err := net.SplitHostPort(reqHost)
	if err != nil {
		host = reqHost
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	target := fmt.Sprintf("/redirect/%s/%s/%d", p.Name, p.Version, n+1)
	at := func(scheme, port string) string { return scheme + "://" + host + ":" + port + target }
	ports := map[string]string{"main": s.ports.main, "other": s.ports.other, "tls": s.ports.tls}
	scheme := func(l string) string {
		if l == "tls" {
			return "https"
		}
		return "http"
	}
	switch kind := p.Redirect.Hops[n]; kind {
	case "same":
		return at(scheme(listener), ports[listener])
	case "relative":
		return target
	case "relative_dots":
		// Up from the directory of the path requested to the root, then down to the target.
		depth := strings.Count(strings.Trim(path, "/"), "/")
		return strings.Repeat("../", depth) + strings.TrimPrefix(target, "/")
	case "main", "other", "tls":
		return at(scheme(kind), ports[kind])
	case "downgrade", "trap":
		return at("http", s.ports.trap)
	case "bad_host":
		return "http://bad host:" + s.ports.trap + target
	case "bad_scheme":
		return "ftp://" + host + ":" + s.ports.trap + target
	case "userinfo":
		return "http://agent:secret@" + host + ":" + s.ports.trap + target
	}
	return target
}

// serveDownload answers package downloads on `listener`: the file, or the next redirect of its
// chain. The file's own origin wants the offer's credentials; another origin serves anyone.
func (s *server) serveDownload(listener string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, hop, ok := downloadPath(r.URL.Path)
		var named *Package // the package the path names
		if ok {
			named = s.packageAt(key)
		}
		p := named // and the one served: a hop the chain has
		if p == nil || (hop > 0 && p.Redirect == nil) || (p.Redirect != nil && hop > len(p.Redirect.Hops)) {
			p = nil
		}
		auth := r.Header.Get("Authorization")
		home := named == nil || named.Redirect.from() == listener
		offered := []string{"Authorization"}
		if !home {
			offered = []string{}
		}
		ev := map[string]any{"path": r.URL.Path, "authorization": auth, "found": p != nil,
			"headers": rawHeaders(r), "offered_headers": offered}
		if p != nil {
			ev["gzip"] = p.Gzip
		}
		if named != nil && named.Redirect != nil {
			ev["package"], ev["version"], ev["hop"] = named.Name, named.Version, hop
			ev["listener"], ev["file_listener"] = listener, named.Redirect.from()
		}
		status := http.StatusOK
		switch {
		case p == nil || (home && auth != "Bearer packages"):
			status = http.StatusNotFound
		case p.Redirect != nil && hop < len(p.Redirect.Hops):
			status = redirectStatuses[hop%len(redirectStatuses)]
			loc := s.location(p, hop, listener, r.URL.Path, r.Host)
			ev["location"] = shown(loc)
			w.Header().Set("Location", loc)
		}
		ev["status"] = status
		s.log.emit("package_download", ev)
		switch {
		case status == http.StatusOK && s.holding("downloads"):
			s.holdDownload(w, r, p.wire())
		case status == http.StatusOK:
			if p.Gzip {
				w.Header().Set("Content-Encoding", "gzip")
			}
			w.Write(p.wire())
		case status == http.StatusNotFound:
			w.WriteHeader(status)
		default:
			// A body, as servers send with a redirect: the client must not take it for the file.
			w.WriteHeader(status)
			w.Write([]byte("redirecting\n"))
		}
	}
}

// downloadMux serves downloads on a listener other than the main one.
func (s *server) downloadMux(listener string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/packages/", s.serveDownload(listener))
	mux.HandleFunc("/redirect/", s.serveDownload(listener))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.emit("stray_request", map[string]any{"path": r.URL.Path, "method": r.Method, "listener": listener,
			"headers": rawHeaders(r)})
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}
