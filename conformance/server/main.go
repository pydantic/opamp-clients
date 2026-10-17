// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Command server is the reference OpAMP server for the conformance suite. It speaks plain HTTP
// and WebSocket on /v1/opamp, applies the faults of a scenario, and writes every frame and
// connection event to a JSON-lines log that the checkers read. A scenario can also have it listen
// with TLS (tls.go) or put a proxy in front of it (proxy.go).
//
// It is written against the protocol directly, not on top of opamp-go's server, so that it can
// do what that server can't: send a WebSocket Close frame and wait for the answer, drop the
// TCP connection, reject a single message, or pretend to restart.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const closeAnswerTimeout = 5 * time.Second

type server struct {
	sc    *Scenario
	start time.Time
	log   *eventLog
	// contact: when the driver's first request (an OpAMP message or a WebSocket upgrade) came, in
	// Unix nanoseconds; 0 before. A scenario's faults are timed from it, not from the server's
	// start: a driver in a container (a JVM, .NET) can start seconds late, and would otherwise
	// meet them late or not at all.
	contact     atomic.Int64
	contactOnce sync.Once

	// closing: the server is shutting down (a held request ends then, not by the client).
	closing atomic.Bool
	// held: requests, upgrades and downloads held so far (scenario "hold").
	held atomic.Int64
	// heldOf: how many of each kind ("messages", "downloads") were held, for Hold.Count.
	heldOf sync.Map

	mu       sync.Mutex
	rng      *mrand.Rand // for windowed faults with a probability; seeded by -seed
	messages int         // AgentToServer messages received so far
	lastSeq  int64       // -1: none since start or restart
	known    bool        // holds a full status report
	conns    int
	open     map[int]*wsConn
	// Credentials (scenario "credentials").
	accepted map[string]bool
	// answered: how many of the offers the agent has answered (APPLIED or FAILED); latest: the
	// token offered last, which the server switches to once the agent uses it.
	answered int
	latest   string
	// movedPort: the second listener's port (scenario credentials.move).
	movedPort string
	// The remote config hash the agent last reported (scenario "remote_config").
	reportedConfig []byte
	// The connection-settings hash the agent last reported, and its capabilities (scenario
	// "own_telemetry").
	reportedSettings []byte
	// reportedPackages: the aggregate packages hash the agent last reported.
	reportedPackages []byte
	// packagesSettled: the agent reported every package of the first offer settled.
	packagesSettled bool
	agentCaps       uint64
	// Hostile (hostile.go): the port that answers malformed endpoints, the settings offer being
	// made, and since when.
	hostilePort     string
	hostileNext     int
	hostileSince    time.Time
	hostileAnswered bool
	// The listeners of redirecting downloads (redirect.go).
	ports downloadPorts
}

type wsConn struct {
	id          int
	c           *websocket.Conn
	wmu         sync.Mutex
	closeSent   bool
	closeAnswer chan struct{}
}

type eventLog struct {
	mu    sync.Mutex
	w     io.Writer
	start time.Time
}

func (l *eventLog) emit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = event
	fields["t"] = time.Since(l.start).Seconds()
	fields["wall"] = float64(time.Now().UnixNano()) / 1e9
	line, _ := json.Marshal(fields)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(append(line, '\n'))
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	scenarioPath := flag.String("scenario", "", "scenario file (JSON)")
	logPath := flag.String("log", "", "event log (JSON lines); stderr if empty")
	seed := flag.Uint64("seed", 1, "seed for windowed faults that fire with a probability")
	certs := flag.String("certs", "", "directory for the PEM files of a TLS scenario (default: the log's)")
	flag.Parse()

	sc, err := loadScenario(*scenarioPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	out := io.Writer(os.Stderr)
	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer f.Close()
		out = f
	}
	start := time.Now()
	s := &server{sc: sc, start: start, log: &eventLog{w: out, start: start}, lastSeq: -1, open: map[int]*wsConn{},
		rng: mrand.New(mrand.NewPCG(*seed, 0x6f70616d70))}
	if c := sc.Credentials; c != nil {
		s.accepted = map[string]bool{c.Bootstrap: true}
	}
	if sc.Hostile != nil {
		if s.hostilePort, err = s.startHostile(*listen); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Where the server is reached, for the runner: TLS files, and the proxy in front of it.
	ready := map[string]any{"event": "listening", "addr": ln.Addr().String()}
	_, s.ports.main, _ = net.SplitHostPort(ln.Addr().String())
	dir := *certs
	if dir == "" && *logPath != "" {
		dir = filepath.Dir(*logPath)
	} else if dir == "" {
		dir = os.TempDir()
	}
	if sc.TLS != nil || sc.Proxy != nil {
		var cfg *tls.Config
		if sc.TLS != nil {
			var files tlsFiles
			if cfg, files, err = setupTLS(sc.TLS, dir); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			ready["tls"] = files
		}
		ln = newLoggingListener(ln, s, cfg)
	}
	if sc.TLS == nil {
		ln = rawListener{ln}
	}
	if sc.Proxy != nil {
		host, port, _ := net.SplitHostPort(ln.Addr().String())
		p, err := startProxy(s, sc.Proxy, host, port)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer p.Close()
		ready["proxy"] = p.ln.Addr().String()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/opamp", s.handle)
	// An OTLP/HTTP receiver, for agents told to send their own telemetry here: it takes anything
	// and records what came, and with which credentials.
	for _, signal := range []string{"traces", "metrics", "logs"} {
		mux.HandleFunc("/v1/"+signal, func(w http.ResponseWriter, r *http.Request) {
			var in io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				if z, err := gzip.NewReader(r.Body); err == nil {
					in = z
				}
			}
			body, _ := io.ReadAll(in)
			services, err := otlpServices(signal, body)
			fields := map[string]any{"signal": signal, "bytes": len(body), "services": services,
				"decode_error": errString(err), "authorization": r.Header.Get("Authorization"),
				"content_type": r.Header.Get("Content-Type")}
			if signal == "traces" {
				fields["spans"] = otlpSpans(body)
				fields["ends"] = otlpSpanEnds(body, s.start)
			}
			s.log.emit("otlp", fields)
			w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusOK)
		})
	}
	// Package files (scenario packages), for the credentials the offer gives, and the redirects
	// to them (redirect.go).
	mux.HandleFunc("/packages/", s.serveDownload("main"))
	mux.HandleFunc("/redirect/", s.serveDownload("main"))
	if sc.Packages != nil {
		caFile, err := s.startRedirects(*listen, dir, s.downloadMux)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if caFile != "" {
			ready["download_ca_file"] = caFile
		}
	}
	// Anything else: a client that followed a redirect, say.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.emit("stray_request", map[string]any{"path": r.URL.Path, "method": r.Method,
			"upgrade": websocket.IsWebSocketUpgrade(r), "headers": rawHeaders(r)})
		w.WriteHeader(http.StatusNotFound)
	})
	srv := &http.Server{Handler: s.withRawHead(s.flagHostile(mux)), ConnContext: connContext}
	go srv.Serve(ln)
	// The endpoint a move offers (scenario credentials.move): the same server on another port.
	if c := sc.Credentials; c != nil && c.Move {
		host, _, _ := net.SplitHostPort(ln.Addr().String())
		moved, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_, s.movedPort, _ = net.SplitHostPort(moved.Addr().String())
		ready["moved"] = moved.Addr().String()
		go srv.Serve(rawListener{moved})
	}
	// Also on the IPv6 loopback, for a client told "localhost" that tries ::1 first (scenario
	// "host"). Optional: without it such a client falls back to 127.0.0.1.
	if host, port, _ := net.SplitHostPort(ln.Addr().String()); host == "127.0.0.1" && sc.TLS == nil && sc.Proxy == nil {
		if ln6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
			go srv.Serve(rawListener{ln6})
		}
	}
	// The runner waits for this line on stdout.
	line, _ := json.Marshal(ready)
	s.log.emit("listening", ready)
	fmt.Println(string(line))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	s.log.emit("server_stop", nil)
	s.closing.Store(true)
	l, logging := ln.(*loggingListener)
	if logging {
		l.stopping.Store(true)
	}
	srv.Close()
	if logging {
		l.closeLive()
	}
}

// elapsed: seconds since the driver's first request, the clock a scenario's faults are timed by
// (0 before it).
func (s *server) elapsed() float64 {
	at := s.contact.Load()
	if at == 0 {
		return 0
	}
	return time.Since(time.Unix(0, at)).Seconds()
}

// contacted starts the faults' clock at the driver's first request, once: the log says when
// ("first_contact"), and the faults timed by it are scheduled from now.
func (s *server) contacted() {
	s.contactOnce.Do(func() {
		s.contact.Store(time.Now().UnixNano())
		s.log.emit("first_contact", nil)
		s.scheduleTimedFaults()
	})
}

// packageAt is the package served at "<name>/<version>", if any.
func (s *server) packageAt(path string) *Package {
	p := s.sc.Packages
	if p == nil {
		return nil
	}
	for _, list := range [][]Package{p.Offer, p.Then} {
		for i := range list {
			if list[i].Name+"/"+list[i].Version == path {
				return &list[i]
			}
		}
	}
	return nil
}

// packagesInfo describes the packages offered now, for the checks: each one's version, the
// SHA-256 of its file, and whether the offer's content hash is wrong; for a hostile package, what
// was malformed (its URL as offered at `opamp`, and its headers).
func packagesInfo(list []Package, opamp, hport string) map[string]any {
	out := map[string]any{}
	for _, pkg := range list {
		sum := sha256.Sum256(pkg.wire())
		p := map[string]any{"version": pkg.Version, "sha256": hex.EncodeToString(sum[:]), "corrupt": pkg.Corrupt,
			"gzip": pkg.Gzip}
		if pkg.hostile != "" {
			p["hostile"], p["url"], p["headers"] = pkg.hostile, shown(expand(pkg.url, opamp, hport)), shownHeaders(pkg.headers)
		}
		if r := pkg.Redirect; r != nil {
			p["redirect"] = map[string]any{"from": r.from(), "hops": r.Hops, "refused": r.refused()}
		}
		out[pkg.Name] = p
	}
	return out
}

// wire is the package's file as served: its content, gzipped if Gzip (deterministically: no
// name, no time).
func (p *Package) wire() []byte {
	if !p.Gzip {
		return []byte(p.Content)
	}
	var b bytes.Buffer
	z, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	z.Write([]byte(p.Content))
	z.Close()
	return b.Bytes()
}

// packagesNow: the packages the server offers now (under s.mu).
func (s *server) packagesNow() []Package {
	p := s.sc.Packages
	if p.Then != nil && s.packagesSettled {
		return p.Then
	}
	return p.Offer
}

// settles: statuses that report every package of `offer` INSTALLED or INSTALL_FAILED, under its
// aggregate hash.
func settles(ps *protobufs.PackageStatuses, offer *protobufs.PackagesAvailable) bool {
	if ps == nil || !bytes.Equal(ps.ServerProvidedAllPackagesHash, offer.AllPackagesHash) {
		return false
	}
	for name := range offer.Packages {
		st := ps.Packages[name].GetStatus()
		if ps.Packages[name] == nil || (st != protobufs.PackageStatusEnum_PackageStatusEnum_Installed &&
			st != protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed) {
			return false
		}
	}
	return true
}

// packagesOffer offers `list`, as reached at `opamp` (the URL the agent's OpAMP request came to);
// hport is the hostile listener's port, for hostile packages, and tport the tls download
// listener's, for packages whose file is there (redirect.go).
func packagesOffer(list []Package, opamp, hport, tport string) *protobufs.PackagesAvailable {
	u, err := neturl.Parse(opamp)
	if err != nil {
		return nil
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	} else if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	offer := &protobufs.PackagesAvailable{Packages: map[string]*protobufs.PackageAvailable{}}
	all := sha256.New()
	for _, pkg := range list {
		content := sha256.Sum256(pkg.wire())
		if pkg.Corrupt {
			content = sha256.Sum256([]byte("not " + pkg.Content))
		}
		hash := sha256.Sum256([]byte(pkg.Name + "\x00" + pkg.Version + "\x00" + string(content[:])))
		u.Path = "/packages/" + pkg.Name + "/" + pkg.Version
		fileURL := u.String()
		if pkg.Redirect.from() == "tls" {
			at := *u
			at.Scheme, at.Host = "https", net.JoinHostPort(u.Hostname(), tport)
			fileURL = at.String()
		}
		file := &protobufs.DownloadableFile{
			DownloadUrl: fileURL,
			ContentHash: content[:],
			Headers:     &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer packages"}}},
		}
		if pkg.hostile != "" {
			file.DownloadUrl = expand(pkg.url, u.String(), hport)
			file.Headers = &protobufs.Headers{}
			for _, h := range pkg.headers {
				file.Headers.Headers = append(file.Headers.Headers, &protobufs.Header{Key: h[0], Value: h[1]})
			}
		}
		offer.Packages[pkg.Name] = &protobufs.PackageAvailable{Version: pkg.Version, Hash: hash[:], File: file}
		all.Write(hash[:])
	}
	offer.AllPackagesHash = all.Sum(nil)
	return offer
}

// acceptingThenDropping: an accept_then_drop fault's window contains now.
func (s *server) acceptingThenDropping() bool {
	now := s.elapsed()
	for _, f := range s.sc.Faults {
		if f.Action == "accept_then_drop" && len(f.WindowS) == 2 && now >= f.WindowS[0] && now < f.WindowS[1] {
			return true
		}
	}
	return false
}

// windowFault is an http_status fault whose window contains now.
func (s *server) windowFault() *Fault {
	now := s.elapsed()
	for i := range s.sc.Faults {
		f := &s.sc.Faults[i]
		if f.Action == "http_status" && len(f.WindowS) == 2 && now >= f.WindowS[0] && now < f.WindowS[1] {
			return f
		}
	}
	return nil
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	s.contacted()
	upgrade := websocket.IsWebSocketUpgrade(r)
	if f := s.windowFault(); f != nil {
		if !upgrade {
			// Log the refused message too, or its sequence_num looks like a gap.
			s.logRefused(r, f.Status)
		}
		if f.Location != "" {
			w.Header().Set("Location", f.Location)
		}
		if f.RetryAfter != "" {
			w.Header().Set("Retry-After", f.RetryAfter)
		}
		w.WriteHeader(f.Status)
		s.log.emit("request_rejected", map[string]any{"status": f.Status, "retry_after": f.RetryAfter,
			"upgrade": upgrade, "user_agent": r.UserAgent(), "headers": rawHeaders(r)})
		return
	}
	if s.holding("messages") {
		s.hold(r, "messages", upgrade)
		return
	}
	if !s.authorized(r, upgrade) {
		if !upgrade {
			s.logRefused(r, http.StatusUnauthorized)
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if upgrade {
		s.handleWS(w, r)
		return
	}
	s.handleHTTP(w, r)
}

// authorized checks the request's bearer token, when the scenario has credentials. Once the agent
// uses the offered token, the server accepts only that.
func (s *server) authorized(r *http.Request, upgrade bool) bool {
	c := s.sc.Credentials
	if c == nil {
		return true
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	ok := s.accepted[token]
	switched := ok && token == s.latest && len(s.accepted) > 1
	if switched {
		s.accepted = map[string]bool{token: true}
	}
	s.mu.Unlock()
	s.log.emit("auth", map[string]any{"token": token, "ok": ok, "upgrade": upgrade, "switched": switched,
		"moved": s.movedPort != "" && strings.HasSuffix(r.Host, ":"+s.movedPort)})
	return ok
}

// otlpSpans names each span in a trace export: its URL (url.full, or the older http.url) when it
// has one, else its name. What an application's requests were, told apart from an agent's own.
// Never nil: an export that doesn't decode names no spans, logged as [] (decode_error says why).
func otlpSpans(body []byte) []string {
	out := []string{}
	var req coltracepb.ExportTraceServiceRequest
	if proto.Unmarshal(body, &req) != nil {
		return out
	}
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				name := sp.Name
				for _, kv := range sp.Attributes {
					if kv.Key == "url.full" || kv.Key == "http.url" {
						name = kv.GetValue().GetStringValue()
					}
				}
				out = append(out, name)
			}
		}
	}
	return out
}

// otlpSpanEnds is when each span in a trace export ended, on the event log's clock (seconds since
// the server started): when the span ended says what a switch should have stopped, where when its
// export arrived depends on batching and a busy machine. The application runs on this host.
func otlpSpanEnds(body []byte, start time.Time) []float64 {
	out := []float64{}
	var req coltracepb.ExportTraceServiceRequest
	if proto.Unmarshal(body, &req) != nil {
		return out
	}
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				out = append(out, float64(int64(sp.EndTimeUnixNano)-start.UnixNano())/1e9)
			}
		}
	}
	return out
}

// otlpServices is the service.name of each resource in an OTLP/HTTP export (protobuf).
func otlpServices(signal string, body []byte) ([]string, error) {
	var resources []*resourcepb.Resource
	switch signal {
	case "traces":
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for _, rs := range req.ResourceSpans {
			resources = append(resources, rs.Resource)
		}
	case "metrics":
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for _, rm := range req.ResourceMetrics {
			resources = append(resources, rm.Resource)
		}
	default:
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for _, rl := range req.ResourceLogs {
			resources = append(resources, rl.Resource)
		}
	}
	services := []string{}
	for _, res := range resources {
		name := ""
		for _, kv := range res.GetAttributes() {
			if kv.Key == "service.name" {
				name = kv.GetValue().GetStringValue()
			}
		}
		services = append(services, name)
	}
	return services, nil
}

// ownCapability is the capability that asks for each signal's settings.
var ownCapability = map[string]protobufs.AgentCapabilities{
	"metrics": protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnMetrics,
	"traces":  protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnTraces,
	"logs":    protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnLogs,
}

// otlpEndpoint is this server's OTLP/HTTP receiver for `signal`, as reached at `opamp` (the URL
// the agent's OpAMP request came to).
func otlpEndpoint(opamp, signal string) string {
	u, err := neturl.Parse(opamp)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = "/v1/" + signal
	return u.String()
}

// endpoint is the URL the request came to, as an agent would be told to use it.
func endpoint(r *http.Request, upgrade bool) string {
	scheme := "http"
	if upgrade {
		scheme = "ws"
	}
	if connTLS(r) != nil {
		scheme += "s"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

// withPort is url with its port replaced.
func withPort(url, port string) string {
	scheme, rest, _ := strings.Cut(url, "://")
	host, path, _ := strings.Cut(rest, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/" + path
}

// messageFaults registers one more AgentToServer and returns the faults triggered by it.
func (s *server) messageFaults() (int, []Fault) {
	now := s.elapsed()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages++
	n := s.messages
	var out []Fault
	for _, f := range s.sc.Faults {
		switch {
		case f.OnMessage != nil && *f.OnMessage == n:
			out = append(out, f)
		case f.Action != "http_status" && f.Action != "accept_then_drop" && len(f.WindowS) == 2 &&
			now >= f.WindowS[0] && now < f.WindowS[1]:
			// Drawn for every message in the window, so the schedule replays from the seed
			// given the same messages.
			if f.P == 0 || s.rng.Float64() < f.P {
				out = append(out, f)
			}
		}
	}
	return n, out
}

func has(faults []Fault, action string) *Fault {
	for i := range faults {
		if faults[i].Action == action {
			return &faults[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Plain HTTP

// transportInfo is how a request reached the server, for scenarios about the connection (TLS or a
// proxy): the TLS version and client certificate, and the peer address, which is a proxy's for a
// request that came through one. Nothing otherwise, so other scenarios' logs stay as they were.
func (s *server) transportInfo(r *http.Request) map[string]any {
	if s.sc.TLS == nil && s.sc.Proxy == nil {
		return nil
	}
	info := tlsInfo(connTLS(r))
	info["remote"] = r.RemoteAddr
	return info
}

// readHTTP decodes a plain HTTP request's AgentToServer and the request details the checks look at.
// headerNames lists the request's header names as sent (Go's canonical case; Host is r.Host),
// one per field line: which headers a client's HTTP stack added. Values stay out: they may be
// credentials.
func headerNames(r *http.Request) []string {
	var names []string
	for name, values := range r.Header {
		for range values {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *server) readHTTP(r *http.Request) (int, *protobufs.AgentToServer, map[string]any, error) {
	s.mu.Lock()
	s.conns++
	req := s.conns
	s.mu.Unlock()
	body, err := io.ReadAll(r.Body)
	if err == nil && r.Header.Get("Content-Encoding") == "gzip" {
		var zr *gzip.Reader
		if zr, err = gzip.NewReader(bytes.NewReader(body)); err == nil {
			body, err = io.ReadAll(zr)
		}
	}
	info := map[string]any{
		"content_type":     r.Header.Get("Content-Type"),
		"content_encoding": r.Header.Get("Content-Encoding"),
		"uid_header":       r.Header.Get("OpAMP-Instance-UID"),
		"user_agent":       r.UserAgent(),
		"accept_encoding":  r.Header.Get("Accept-Encoding"),
		"header_names":     headerNames(r),
	}
	if h := rawHeaders(r); h != nil {
		info["headers"] = h
	}
	for k, v := range s.transportInfo(r) {
		info[k] = v
	}
	msg := &protobufs.AgentToServer{}
	if err == nil {
		err = proto.Unmarshal(body, msg)
	}
	if err != nil {
		s.log.emit("decode_error", map[string]any{"conn": req, "transport": "http", "error": err.Error()})
	}
	return req, msg, info, err
}

// logRefused records a request refused by an http_status window. It counts as a message.
func (s *server) logRefused(r *http.Request, status int) {
	req, msg, info, err := s.readHTTP(r)
	if err != nil {
		return
	}
	n, _ := s.messageFaults()
	info["refused_status"] = status
	s.logA2S(msg, n, req, "http", info)
}

func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	req, msg, httpInfo, err := s.readHTTP(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	n, faults := s.messageFaults()
	s.logA2S(msg, n, req, "http", httpInfo)

	if has(faults, "tcp_drop") != nil {
		// The server has the message; the connection goes before its reply (a reply lost on the
		// way back). Nothing is answered.
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
			}
		}
		s.log.emit("tcp_drop", map[string]any{"conn": req, "msg": n})
		return
	}
	if f := has(faults, "reject"); f != nil && f.Status == http.StatusRequestEntityTooLarge {
		w.WriteHeader(f.Status)
		s.log.emit("s2a", map[string]any{"conn": req, "msg": n, "http_status": f.Status})
		return
	}
	if f := has(faults, "unavailable"); f != nil {
		w.Header().Set("Retry-After", strconv.Itoa(int(f.RetryAfterS)))
		w.WriteHeader(http.StatusServiceUnavailable)
		s.log.emit("s2a", map[string]any{"conn": req, "msg": n, "http_status": 503, "retry_after": int(f.RetryAfterS)})
		return
	}
	resp, info := s.respond(msg, faults, n, endpoint(r, false))
	if has(faults, "reject") != nil {
		// Any status other than 413: a malformed message, answered with BAD_REQUEST.
		resp = &protobufs.ServerToAgent{InstanceUid: msg.InstanceUid, ErrorResponse: &protobufs.ServerErrorResponse{
			Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest, ErrorMessage: "rejected by scenario"}}
		info = map[string]any{"error": "BAD_REQUEST"}
	}
	info["conn"], info["msg"], info["http_status"] = req, n, 200
	s.log.emit("s2a", info)
	out, _ := proto.Marshal(resp)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(out)
}

// ---------------------------------------------------------------------------------------------
// WebSocket

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	var answer http.Header
	if s.sc.Cookie {
		answer = http.Header{"Set-Cookie": {cookie}}
	}
	c, err := upgrader.Upgrade(w, r, answer)
	haltRaw(r)
	if err != nil {
		s.log.emit("upgrade_error", map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.conns++
	wc := &wsConn{id: s.conns, c: c, closeAnswer: make(chan struct{})}
	s.open[wc.id] = wc
	s.mu.Unlock()
	via := s.transportInfo(r)
	opened := map[string]any{"conn": wc.id, "user_agent": r.UserAgent(),
		"extensions": r.Header.Get("Sec-WebSocket-Extensions"), "header_names": headerNames(r)}
	if h := rawHeaders(r); h != nil {
		opened["headers"] = h
	}
	for k, v := range via {
		opened[k] = v
	}
	s.log.emit("ws_open", opened)
	if s.acceptingThenDropping() {
		// A proxy or load balancer that takes the upgrade and loses its backend: the connection
		// goes before the server reads or answers anything, without a Close frame.
		s.mu.Lock()
		delete(s.open, wc.id)
		s.mu.Unlock()
		s.log.emit("accept_then_drop", map[string]any{"conn": wc.id})
		c.UnderlyingConn().Close()
		s.log.emit("ws_closed", map[string]any{"conn": wc.id, "reason": "accept_then_drop"})
		return
	}
	url := endpoint(r, true)

	c.SetCloseHandler(func(code int, text string) error {
		wc.wmu.Lock()
		answered := wc.closeSent
		if !answered {
			// The client closed first: answer, as RFC 6455 requires.
			c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
		}
		wc.wmu.Unlock()
		if answered {
			close(wc.closeAnswer)
		}
		s.log.emit("close_frame_received", map[string]any{"conn": wc.id, "code": code, "answers_server_close": answered})
		return nil
	})

	reason := "eof"
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			if _, ok := err.(*websocket.CloseError); ok {
				reason = "close_handshake"
			} else {
				reason = err.Error()
			}
			break
		}
		if mt != websocket.BinaryMessage {
			s.log.emit("non_binary_message", map[string]any{"conn": wc.id, "type": mt})
			continue
		}
		header, size := binary.Uvarint(data)
		if size <= 0 || header != 0 {
			s.log.emit("bad_header", map[string]any{"conn": wc.id, "header": header})
			continue
		}
		msg := &protobufs.AgentToServer{}
		if err := proto.Unmarshal(data[size:], msg); err != nil {
			s.log.emit("decode_error", map[string]any{"conn": wc.id, "transport": "ws", "error": err.Error()})
			continue
		}
		n, faults := s.messageFaults()
		s.logA2S(msg, n, wc.id, "ws", via)
		if has(faults, "tcp_drop") != nil {
			s.dropTCP(wc)
			continue
		}
		var resp *protobufs.ServerToAgent
		var info map[string]any
		if f := has(faults, "unavailable"); f != nil {
			// Turned away, as a 503 over HTTP: the server didn't take the message in, so it
			// doesn't move the sequence on (respond is not called). The next message is then a
			// gap, and the server asks for the full state: a client that doesn't resend what
			// it sent (opamp-go over WebSocket ignores the error) would otherwise leave the
			// server without that status until the next change.
			resp = &protobufs.ServerToAgent{InstanceUid: msg.InstanceUid, ErrorResponse: &protobufs.ServerErrorResponse{
				Type:         protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable,
				ErrorMessage: "unavailable by scenario",
				Details: &protobufs.ServerErrorResponse_RetryInfo{RetryInfo: &protobufs.RetryInfo{
					RetryAfterNanoseconds: uint64(f.RetryAfterS * 1e9)}},
			}}
			info = map[string]any{"error": "UNAVAILABLE", "retry_after": f.RetryAfterS}
		} else {
			resp, info = s.respond(msg, faults, n, url)
			if has(faults, "reject") != nil {
				resp = &protobufs.ServerToAgent{InstanceUid: msg.InstanceUid, ErrorResponse: &protobufs.ServerErrorResponse{
					Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest, ErrorMessage: "rejected by scenario"}}
				info = map[string]any{"error": "BAD_REQUEST"}
			}
		}
		info["conn"], info["msg"] = wc.id, n
		out, _ := proto.Marshal(resp)
		wc.wmu.Lock()
		err = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, out...))
		wc.wmu.Unlock()
		if err == nil {
			s.log.emit("s2a", info)
		}
		if f := has(faults, "close_frame"); f != nil {
			s.sendClose(wc, f.Code)
		}
	}
	s.mu.Lock()
	delete(s.open, wc.id)
	s.mu.Unlock()
	c.Close()
	s.log.emit("ws_closed", map[string]any{"conn": wc.id, "reason": reason})
}

func (s *server) sendClose(wc *wsConn, code int) {
	if code == 0 {
		code = websocket.CloseNormalClosure
	}
	wc.wmu.Lock()
	if wc.closeSent {
		wc.wmu.Unlock()
		return
	}
	wc.closeSent = true
	err := wc.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "conformance"), time.Now().Add(time.Second))
	wc.wmu.Unlock()
	s.log.emit("close_frame_sent", map[string]any{"conn": wc.id, "code": code, "error": errString(err)})
	go func() {
		select {
		case <-wc.closeAnswer:
		case <-time.After(closeAnswerTimeout):
			s.log.emit("close_not_answered", map[string]any{"conn": wc.id, "after_s": closeAnswerTimeout.Seconds()})
			wc.c.UnderlyingConn().Close()
		}
	}()
}

func (s *server) dropTCP(wc *wsConn) {
	s.log.emit("tcp_drop", map[string]any{"conn": wc.id})
	wc.c.UnderlyingConn().Close()
}

// ---------------------------------------------------------------------------------------------
// Faults at a point in time (after the driver's first request: contacted)

func (s *server) scheduleTimedFaults() {
	for _, f := range s.sc.Faults {
		if f.Action == "accept_then_drop" {
			// The window opens on a client already connected: its connection goes too, so the
			// first connection the window sees has not been answered either.
			time.AfterFunc(time.Duration(f.WindowS[0]*float64(time.Second)), func() {
				s.applyTimed(Fault{Action: "tcp_drop"})
			})
			continue
		}
		if f.AtS == nil {
			continue
		}
		f := f
		time.AfterFunc(time.Duration(*f.AtS*float64(time.Second)), func() { s.applyTimed(f) })
	}
}

func (s *server) applyTimed(f Fault) {
	switch f.Action {
	case "restart":
		s.mu.Lock()
		s.lastSeq, s.known = -1, false
		s.mu.Unlock()
		s.log.emit("restart", nil)
	case "close_frame", "tcp_drop":
		s.mu.Lock()
		var conns []*wsConn
		for _, wc := range s.open {
			conns = append(conns, wc)
		}
		s.mu.Unlock()
		if len(conns) == 0 {
			s.log.emit("fault_skipped", map[string]any{"action": f.Action, "reason": "no open WebSocket"})
		}
		for _, wc := range conns {
			if f.Action == "close_frame" {
				s.sendClose(wc, f.Code)
			} else {
				s.dropTCP(wc)
			}
		}
	default:
		s.log.emit("fault_skipped", map[string]any{"action": f.Action, "reason": "not a timed action"})
	}
}

// ---------------------------------------------------------------------------------------------
// Processing

const serverCapabilities = uint64(protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
	protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig |
	protobufs.ServerCapabilities_ServerCapabilities_AcceptsEffectiveConfig |
	protobufs.ServerCapabilities_ServerCapabilities_OffersConnectionSettings)

// isFull: the message carries the agent description, the part of the status every
// client has. Checkers look at the logged field list for anything finer.
func isFull(msg *protobufs.AgentToServer) bool { return msg.AgentDescription != nil }

func (s *server) respond(msg *protobufs.AgentToServer, faults []Fault, n int, url string) (*protobufs.ServerToAgent, map[string]any) {
	info := map[string]any{}
	s.mu.Lock()
	seq := int64(msg.SequenceNum)
	gap := s.lastSeq >= 0 && seq != s.lastSeq+1
	full := isFull(msg)
	unknown := !s.known && !full
	s.lastSeq = seq
	if full {
		s.known = true
	}
	s.mu.Unlock()

	caps := serverCapabilities
	if s.sc.Packages != nil {
		caps |= uint64(protobufs.ServerCapabilities_ServerCapabilities_OffersPackages |
			protobufs.ServerCapabilities_ServerCapabilities_AcceptsPackagesStatus)
	}
	if s.sc.Capabilities != nil {
		caps = *s.sc.Capabilities
	}
	resp := &protobufs.ServerToAgent{InstanceUid: msg.InstanceUid, Capabilities: caps}
	askFull := false
	if !s.sc.Lax && (gap || unknown) && !full && msg.AgentDisconnect == nil {
		askFull = true
		info["full_state_reason"] = map[bool]string{true: "gap", false: "unknown"}[gap]
	}
	if has(faults, "report_full_state") != nil {
		askFull = true
		info["full_state_reason"] = "scenario"
	}
	if askFull {
		resp.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	}
	info["report_full_state"] = askFull
	if caps := s.sc.CustomCapabilities; caps != nil {
		resp.CustomCapabilities = &protobufs.CustomCapabilities{Capabilities: caps}
	}
	if f := has(faults, "custom_ping"); f != nil {
		resp.CustomMessage = &protobufs.CustomMessage{Capability: "io.opentelemetry.echo", Type: "ping", Data: []byte(f.Body)}
		info["custom_ping"] = hex.EncodeToString([]byte(f.Body))
	}
	if has(faults, "custom_other") != nil {
		resp.CustomMessage = &protobufs.CustomMessage{Capability: "io.opentelemetry.other", Type: "ping"}
		info["custom_other"] = true
	}
	if has(faults, "ask_components") != nil {
		resp.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
		info["ask_components"] = true
	}
	requested := msg.Flags&uint64(protobufs.AgentToServerFlags_AgentToServerFlags_RequestInstanceUid) != 0
	if f := has(faults, "assign_uid"); f != nil {
		uid := newUID(f.UID)
		resp.AgentIdentification = &protobufs.AgentIdentification{NewInstanceUid: uid}
		info["assign_uid"] = hex.EncodeToString(uid)
	} else if s.sc.HonourUIDRequest && requested {
		// A new UID for every message that asks: a client asking twice gets two.
		uid := newUID("v7")
		resp.AgentIdentification = &protobufs.AgentIdentification{NewInstanceUid: uid}
		info["assign_uid"] = hex.EncodeToString(uid)
		info["uid_requested"] = true
		time.Sleep(time.Duration(s.sc.UIDRequestDelayS * float64(time.Second)))
	}
	if f := has(faults, "offer_config"); f != nil {
		sum := sha256.Sum256([]byte(f.Body))
		ct := f.ContentType
		if ct == "" {
			ct = "text/yaml"
		}
		resp.RemoteConfig = &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{f.ConfigKey: {Body: []byte(f.Body), ContentType: ct}}},
			ConfigHash: sum[:],
		}
		info["offer_config"] = hex.EncodeToString(sum[:])
	}
	if s.sc.HeartbeatIntervalS != nil {
		hb := *s.sc.HeartbeatIntervalS
		resp.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
			Hash:  []byte("heartbeat-" + strconv.FormatUint(hb, 10)),
			Opamp: &protobufs.OpAMPConnectionSettings{HeartbeatIntervalSeconds: hb},
		}
		info["heartbeat_offer"] = hb
	}
	if rc := s.sc.RemoteConfig; rc != nil {
		sum := sha256.Sum256([]byte(rc.Body))
		s.mu.Lock()
		if st := msg.RemoteConfigStatus; st != nil {
			s.reportedConfig = st.LastRemoteConfigHash
		}
		offer := (rc.Always || !bytes.Equal(s.reportedConfig, sum[:])) && msg.AgentDisconnect == nil
		s.mu.Unlock()
		if offer {
			resp.RemoteConfig = &protobufs.AgentRemoteConfig{
				Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
					"": {Body: []byte(rc.Body), ContentType: "text/yaml"}}},
				ConfigHash: sum[:],
			}
			info["offer_config"] = hex.EncodeToString(sum[:])
		}
	}
	if ot := s.sc.OwnTelemetry; ot != nil {
		dest := ot.Endpoint
		if dest == "" {
			dest = otlpEndpoint(url, ot.Signal)
		}
		sum := sha256.Sum256([]byte(ot.Signal + " " + dest + " " + ot.Token))
		s.mu.Lock()
		if st := msg.ConnectionSettingsStatus; st != nil {
			s.reportedSettings = st.LastConnectionSettingsHash
		}
		if msg.Capabilities != 0 {
			s.agentCaps = msg.Capabilities
		}
		asked := s.agentCaps&uint64(ownCapability[ot.Signal]) != 0
		offer := asked && !bytes.Equal(s.reportedSettings, sum[:]) && msg.AgentDisconnect == nil
		s.mu.Unlock()
		if offer {
			settings := &protobufs.TelemetryConnectionSettings{DestinationEndpoint: dest}
			if ot.Token != "" {
				settings.Headers = &protobufs.Headers{Headers: []*protobufs.Header{
					{Key: "Authorization", Value: "Bearer " + ot.Token}}}
			}
			cs := &protobufs.ConnectionSettingsOffers{Hash: sum[:]}
			switch ot.Signal {
			case "traces":
				cs.OwnTraces = settings
			case "logs":
				cs.OwnLogs = settings
			default:
				cs.OwnMetrics = settings
			}
			resp.ConnectionSettings = cs
			info["telemetry_offer"] = hex.EncodeToString(sum[:])
			info["telemetry_endpoint"] = dest
		}
	}
	if s.sc.Packages != nil {
		s.mu.Lock()
		if ps := msg.PackageStatuses; ps != nil {
			s.reportedPackages = ps.ServerProvidedAllPackagesHash
			if first := packagesOffer(s.sc.Packages.Offer, url, s.hostilePort, s.ports.tls); first != nil && settles(ps, first) {
				s.packagesSettled = true
			}
		}
		list := s.packagesNow()
		offer := packagesOffer(list, url, s.hostilePort, s.ports.tls)
		if msg.Capabilities != 0 {
			s.agentCaps = msg.Capabilities
		}
		accepts := s.agentCaps&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsPackages) != 0
		send := offer != nil && accepts && !bytes.Equal(s.reportedPackages, offer.AllPackagesHash) && msg.AgentDisconnect == nil
		s.mu.Unlock()
		if send {
			resp.PackagesAvailable = offer
			info["packages_offer"] = hex.EncodeToString(offer.AllPackagesHash)
			fileURL := url
			if strings.HasPrefix(url, "ws") {
				fileURL = "http" + url[2:] // as packagesOffer offers files: over HTTP
			}
			info["packages"] = packagesInfo(list, fileURL, s.hostilePort)
		}
	}
	if c := s.sc.Credentials; c != nil {
		s.mu.Lock()
		offers, step := c.offers(), s.answered
		s.mu.Unlock()
		if step < len(offers) {
			token := offers[step]
			sum := sha256.Sum256([]byte(token))
			if st := msg.ConnectionSettingsStatus; st != nil && bytes.Equal(st.LastConnectionSettingsHash, sum[:]) &&
				(st.Status == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED ||
					st.Status == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED) {
				s.mu.Lock()
				s.answered++
				s.mu.Unlock()
				step++
			}
		}
		if step < len(offers) && n >= c.OfferOn && msg.AgentDisconnect == nil {
			token := offers[step]
			sum := sha256.Sum256([]byte(token))
			if !c.Broken {
				s.mu.Lock()
				s.accepted[token] = true
				s.latest = token
				s.mu.Unlock()
			}
			dest := url
			if c.Move && step == 0 {
				dest = withPort(url, s.movedPort)
			}
			resp.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
				Hash: sum[:],
				Opamp: &protobufs.OpAMPConnectionSettings{
					DestinationEndpoint: dest,
					Headers: &protobufs.Headers{Headers: []*protobufs.Header{
						{Key: "Authorization", Value: "Bearer " + token}}},
				},
			}
			info["credentials_offer"] = hex.EncodeToString(sum[:])
			info["credentials_offer_step"] = step
			info["credentials_offer_endpoint"] = dest
		}
	}
	if cs := s.hostileSettings(msg, n, url, info); cs != nil {
		resp.ConnectionSettings = cs
	}
	return resp, info
}

func (s *server) logA2S(msg *protobufs.AgentToServer, n, conn int, transport string, extra map[string]any) {
	fields := []string{}
	add := func(name string, present bool) {
		if present {
			fields = append(fields, name)
		}
	}
	add("agent_description", msg.AgentDescription != nil)
	add("health", msg.Health != nil)
	add("effective_config", msg.EffectiveConfig != nil)
	add("remote_config_status", msg.RemoteConfigStatus != nil)
	add("package_statuses", msg.PackageStatuses != nil)
	add("agent_disconnect", msg.AgentDisconnect != nil)
	add("custom_capabilities", msg.CustomCapabilities != nil)
	add("custom_message", msg.CustomMessage != nil)
	add("available_components", msg.AvailableComponents != nil)
	add("connection_settings_status", msg.ConnectionSettingsStatus != nil)

	// The same request resent: identical apart from sequence_num and instance_uid.
	body := proto.Clone(msg).(*protobufs.AgentToServer)
	body.SequenceNum, body.InstanceUid = 0, nil
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	sum := sha256.Sum256(raw)

	ev := map[string]any{
		"conn": conn, "msg": n, "transport": transport,
		"seq": msg.SequenceNum, "uid": hex.EncodeToString(msg.InstanceUid), "caps": msg.Capabilities,
		"flags": msg.Flags, "fields": fields, "full": isFull(msg), "disconnect": msg.AgentDisconnect != nil,
		"payload_hash": hex.EncodeToString(sum[:8]), "bytes": proto.Size(msg),
	}
	if d := msg.AgentDescription; d != nil {
		// Drivers put their status-change counter here (DRIVERS.md): the value the server now holds.
		for _, kv := range d.NonIdentifyingAttributes {
			if kv.Key == "driver.change" {
				if n, err := strconv.Atoi(kv.GetValue().GetStringValue()); err == nil {
					ev["driver_change"] = n
				}
			}
		}
	}
	if cs := msg.ConnectionSettingsStatus; cs != nil {
		ev["connection_settings_status"] = map[string]any{"status": cs.Status.String(),
			"hash": hex.EncodeToString(cs.LastConnectionSettingsHash), "error": cs.ErrorMessage}
	}
	if rcs := msg.RemoteConfigStatus; rcs != nil {
		ev["remote_config_status"] = map[string]any{"status": rcs.Status.String(),
			"hash": hex.EncodeToString(rcs.LastRemoteConfigHash), "error": rcs.ErrorMessage}
	}
	if cm := msg.CustomMessage; cm != nil {
		ev["custom_message"] = map[string]any{"capability": cm.Capability, "type": cm.Type,
			"data": hex.EncodeToString(cm.Data)}
	}
	if cc := msg.CustomCapabilities; cc != nil {
		ev["custom_capabilities"] = cc.Capabilities
	}
	if ps := msg.PackageStatuses; ps != nil {
		packages := map[string]any{}
		for name, p := range ps.Packages {
			packages[name] = map[string]any{"status": p.Status.String(), "has_version": p.AgentHasVersion,
				"has_hash": hex.EncodeToString(p.AgentHasHash), "offered_version": p.ServerOfferedVersion,
				"offered_hash": hex.EncodeToString(p.ServerOfferedHash), "error": p.ErrorMessage}
		}
		ev["package_statuses"] = map[string]any{"all": hex.EncodeToString(ps.ServerProvidedAllPackagesHash),
			"packages": packages, "error": ps.ErrorMessage}
	}
	if ac := msg.AvailableComponents; ac != nil {
		ev["available_components"] = map[string]any{"hash": hex.EncodeToString(ac.Hash),
			"components": len(ac.Components)}
	}
	for k, v := range extra {
		ev[k] = v
	}
	s.log.emit("a2s", ev)
}

func newUID(kind string) []byte {
	uid := make([]byte, 16)
	switch kind {
	case "", "v7":
		ms := uint64(time.Now().UnixMilli())
		binary.BigEndian.PutUint64(uid[0:8], ms<<16)
		rand.Read(uid[6:])
		uid[6] = 0x70 | (uid[6] & 0x0f)
		uid[8] = 0x80 | (uid[8] & 0x3f)
	case "not-rfc4122":
		rand.Read(uid)
		uid[6] &= 0x0f // version nibble 0
	default:
		if b, err := hex.DecodeString(kind); err == nil && len(b) == 16 {
			copy(uid, b)
		}
	}
	return uid
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
