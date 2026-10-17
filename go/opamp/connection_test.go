// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// The TLS and proxy options, over HTTP, WebSocket and package downloads alike: servers with TLS
// (trusted by the CA given), with mutual TLS, and HTTP, HTTPS and SOCKS5 proxies that need
// credentials, all in the test.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

func testConnection(t *testing.T, tl TLS, p Proxy) *connection {
	t.Helper()
	c, err := newConnection(tl, p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ---------------------------------------------------------------------------------------------
// Certificates

// testCA is a CA made up for a test.
type testCA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	pem   []byte
	pool  *x509.CertPool
	child int64
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool,
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue makes a certificate for 127.0.0.1 and localhost (server) or named cn (client), and its
// PKCS #8 key.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (certPEM, keyPEM []byte, pair tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca.child++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(ca.child + 1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := x509.MarshalPKCS8PrivateKey(key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})
	pair, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM, pair
}

// ---------------------------------------------------------------------------------------------
// The server

// recorder is an OpAMP server (propServer) that also serves a package file at /file, and records
// where each request came from, whether it came over TLS, and the client certificate's name.
type recorder struct {
	opamp propServer
	file  []byte

	mu   sync.Mutex
	seen []request
}

type request struct {
	path   string
	remote string
	tls    bool
	peer   string // the client certificate's common name
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s := request{path: req.URL.Path, remote: req.RemoteAddr, tls: req.TLS != nil}
	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		s.peer = req.TLS.PeerCertificates[0].Subject.CommonName
	}
	r.mu.Lock()
	r.seen = append(r.seen, s)
	r.mu.Unlock()
	if req.URL.Path == "/file" {
		_, _ = w.Write(r.file)
		return
	}
	r.opamp.ServeHTTP(w, req)
}

func (r *recorder) requests() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// startServer starts one without TLS (mode ""), with TLS ("tls"; "tls12": TLS 1.2 at most), or
// with mutual TLS ("mtls": a client certificate signed by ca is required).
func startServer(t *testing.T, ca *testCA, mode string) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{file: bytes.Repeat([]byte("package "), 1000)}
	ts := httptest.NewUnstartedServer(rec)
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are expected
	if mode == "" {
		ts.Start()
	} else {
		_, _, pair := ca.issue(t, "server", true)
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
		if mode == "tls12" {
			ts.TLS.MaxVersion = tls.VersionTLS12
		}
		if mode == "mtls" {
			ts.TLS.ClientAuth = tls.RequireAndVerifyClientCert
			ts.TLS.ClientCAs = ca.pool
		}
		ts.StartTLS()
	}
	t.Cleanup(ts.Close)
	return rec, ts
}

// ---------------------------------------------------------------------------------------------
// Proxies

const proxyUser, proxyPass = "agent", "proxy-secret"

// testProxy is an HTTP proxy (CONNECT, and requests in absolute form), the same over TLS, or a
// SOCKS5 proxy (socks5 or socks5h: the same proxy, under either scheme), needing proxyUser and
// proxyPass. It records every tunnel.
type testProxy struct {
	kind string // http, https, socks5 or socks5h
	ln   net.Listener

	mu      sync.Mutex
	tunnels []tunnel
	refused []string
}

type tunnel struct {
	via      string // connect, forward or socks5
	user     string
	header   string // the X-Proxy-Test header of a CONNECT
	upstream string // the address the server sees the tunnel's requests come from
	atyp     byte   // SOCKS5: the address type asked for (1 IPv4, 3 a name, 4 IPv6)
	target   string // SOCKS5: the host asked for, a name or an address
}

func startProxy(t *testing.T, kind string, ca *testCA) *testProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if kind == "https" {
		_, _, pair := ca.issue(t, "proxy", true)
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}})
	}
	p := &testProxy{kind: kind, ln: ln}
	var conns sync.WaitGroup
	var mu sync.Mutex
	open := map[net.Conn]bool{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open[c] = true
			mu.Unlock()
			conns.Go(func() {
				defer func() { mu.Lock(); delete(open, c); mu.Unlock() }()
				p.serve(c)
			})
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for c := range open {
			c.Close()
		}
		mu.Unlock()
		conns.Wait()
	})
	return p
}

func (p *testProxy) url(user, pass string) string {
	scheme := p.kind
	return scheme + "://" + user + ":" + pass + "@" + p.ln.Addr().String()
}

func (p *testProxy) record(tn tunnel) {
	p.mu.Lock()
	p.tunnels = append(p.tunnels, tn)
	p.mu.Unlock()
}

func (p *testProxy) refuse(why string) {
	p.mu.Lock()
	p.refused = append(p.refused, why)
	p.mu.Unlock()
}

func (p *testProxy) seen() ([]tunnel, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.tunnels), slices.Clone(p.refused)
}

func (p *testProxy) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	if strings.HasPrefix(p.kind, "socks5") {
		p.serveSOCKS5(c, br)
	} else {
		p.serveHTTP(c, br)
	}
}

func relay(c net.Conn, br *bufio.Reader, up net.Conn) {
	c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, br); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	c.Close()
	up.Close()
	<-done
}

func (p *testProxy) serveHTTP(c net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		p.refuse("protocol: " + err.Error())
		return
	}
	user, pass := "", ""
	if h := req.Header.Get("Proxy-Authorization"); strings.HasPrefix(h, "Basic ") {
		raw, _ := base64.StdEncoding.DecodeString(h[6:])
		user, pass, _ = strings.Cut(string(raw), ":")
	}
	if user != proxyUser || pass != proxyPass {
		p.refuse("auth")
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return
	}
	target := req.Host
	if req.Method != http.MethodConnect {
		if !req.URL.IsAbs() {
			p.refuse("not in absolute form: " + req.RequestURI)
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
			return
		}
		target = req.URL.Host
	}
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		p.refuse("target: " + err.Error())
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	tn := tunnel{via: "connect", user: user, header: req.Header.Get("X-Proxy-Test"), upstream: up.LocalAddr().String()}
	if req.Method == http.MethodConnect {
		p.record(tn)
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	} else {
		// The rest of the connection goes to the same server; it takes absolute form too.
		tn.via = "forward"
		p.record(tn)
		req.Header.Del("Proxy-Authorization")
		if err := req.Write(up); err != nil {
			up.Close()
			return
		}
	}
	relay(c, br, up)
}

func (p *testProxy) serveSOCKS5(c net.Conn, br *bufio.Reader) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || hdr[0] != 5 {
		p.refuse("not SOCKS5")
		return
	}
	methods := make([]byte, hdr[1])
	io.ReadFull(br, methods)
	if !slices.Contains(methods, 2) {
		p.refuse("no username/password offered")
		c.Write([]byte{5, 0xff})
		return
	}
	c.Write([]byte{5, 2})
	field := func() string {
		n, _ := br.ReadByte()
		b := make([]byte, n)
		io.ReadFull(br, b)
		return string(b)
	}
	if v, _ := br.ReadByte(); v != 1 {
		p.refuse("auth version")
		return
	}
	user, pass := field(), field()
	if user != proxyUser || pass != proxyPass {
		p.refuse("auth")
		c.Write([]byte{1, 1})
		return
	}
	c.Write([]byte{1, 0})
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != 5 || req[1] != 1 {
		p.refuse("request")
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		io.ReadFull(br, ip)
		host = net.IP(ip).String()
	case 3:
		host = field()
	case 4:
		ip := make([]byte, 16)
		io.ReadFull(br, ip)
		host = net.IP(ip).String()
	default:
		p.refuse("address type " + strconv.Itoa(int(req[3])))
		return
	}
	var port [2]byte
	io.ReadFull(br, port[:])
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), 5*time.Second)
	if err != nil {
		p.refuse("target: " + err.Error())
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	p.record(tunnel{via: "socks5", user: user, upstream: up.LocalAddr().String(), atyp: req[3], target: host})
	c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	relay(c, br, up)
}

// ---------------------------------------------------------------------------------------------
// Running a client

// run starts a client, waits until the server has three messages (or the client has failed to
// connect twice), and stops it. It returns what the server got and why connecting failed.
func run(t *testing.T, cfg Config, srv *recorder) (got []*protobufs.AgentToServer, failures []string) {
	t.Helper()
	var mu sync.Mutex
	c, err := Start(context.Background(), cfg, Status{Description: propDescription(0)}, func(_ *Client, n Notification) {
		if n.Kind == ConnectFailed {
			mu.Lock()
			failures = append(failures, n.Detail)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(srv.opamp.messages()) >= 3 || len(failures) >= 2
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return srv.opamp.messages(), slices.Clone(failures)
}

func endpoint(ts *httptest.Server, ws bool) string {
	if ws {
		return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/opamp"
	}
	return ts.URL + "/v1/opamp"
}

// clientConfig is a Go mode client's settings (checkCombination sets the mode it tests).
func clientConfig(ts *httptest.Server, ws bool) Config {
	return Config{Mode: ModeGo, Endpoint: endpoint(ts, ws), InstanceUID: [16]byte{0x42}, HeartbeatInterval: 50 * time.Millisecond}
}

// ---------------------------------------------------------------------------------------------
// Tests

// Every combination: HTTP or WebSocket; no TLS, TLS or mutual TLS; no proxy, an HTTP proxy, an
// HTTP proxy over TLS, or SOCKS5. The client connects, trusting only the CA it is given and
// presenting the client certificate it is given; every request reaches the server through the
// proxy with its credentials; and a package download goes the same way.
// covers: RULE-TLS-01, RULE-TLS-02, RULE-PROXY-01
//
// In both modes: Go's net/http and gorilla, and the core's own HTTP and WebSocket over Go's
// connections and crypto/tls (Rust mode).
func TestEveryTransportOverEveryTLSAndProxy(t *testing.T) {
	for _, client := range []Mode{ModeGo, ModeRust} {
		for _, ws := range []bool{false, true} {
			for _, mode := range []string{"", "tls", "mtls"} {
				for _, via := range []string{"", "http", "https", "socks5"} {
					name := fmt.Sprintf("%s/ws=%v/tls=%q/proxy=%q", client, ws, mode, via)
					t.Run(name, func(t *testing.T) { checkCombination(t, client, ws, mode, via) })
				}
			}
		}
	}
}

func checkCombination(t *testing.T, client Mode, ws bool, mode, via string) {
	ca := newTestCA(t, "test CA")
	srv, ts := startServer(t, ca, mode)
	cfg := clientConfig(ts, ws)
	cfg.Mode = client
	if client == ModeRust && via == "" {
		cfg.Proxy.Direct = true // the local server, whatever the environment says (Rust mode's rule proxies loopback too)
	}
	if mode != "" || via == "https" {
		cfg.TLS.CAPEM = ca.pem
	}
	if mode == "mtls" {
		cfg.TLS.CertPEM, cfg.TLS.KeyPEM, _ = ca.issue(t, "agent", false)
	}
	var proxy *testProxy
	if via != "" {
		proxy = startProxy(t, via, ca)
		cfg.Proxy.URL = proxy.url(proxyUser, proxyPass)
		if via != "socks5" {
			cfg.Proxy.ConnectHeader = http.Header{"X-Proxy-Test": {"yes"}}
		}
	}

	got, failures := run(t, cfg, srv)
	if len(failures) > 0 {
		t.Fatalf("failed to connect: %v", failures)
	}
	if len(got) < 3 {
		t.Fatalf("the server got %d messages", len(got))
	}
	if err := checkSequence(got); err != nil {
		t.Fatal(err)
	}

	// A package file, through the same connection options.
	conn, err := cfg.connection()
	if err != nil {
		t.Fatal(err)
	}
	x := newExecutor(conn)
	x.stateFile = filepath.Join(t.TempDir(), "state")
	in := x.download(context.Background(), &corepb.Download{Id: 1, Url: ts.URL + "/file", MaxBytes: 1 << 20, TimeoutMs: 5000})
	if sum := sha256.Sum256(srv.file); in.GetDownloadResponse().GetStatus() != 200 || !bytes.Equal(in.GetDownloadResponse().Sha256, sum[:]) {
		t.Fatalf("download: %v", in)
	}

	reqs := srv.requests()
	for _, r := range reqs {
		if r.tls != (mode != "") {
			t.Fatalf("request from %s: over TLS %v", r.remote, r.tls)
		}
		if want := map[string]string{"mtls": "agent"}[mode]; r.peer != want {
			t.Fatalf("request from %s: client certificate %q, want %q", r.remote, r.peer, want)
		}
	}
	if proxy == nil {
		return
	}
	tunnels, refused := proxy.seen()
	if len(refused) > 0 {
		t.Fatalf("the proxy refused: %v", refused)
	}
	upstream := map[string]tunnel{}
	for _, tn := range tunnels {
		upstream[tn.upstream] = tn
		if tn.user != proxyUser {
			t.Fatalf("tunnel as %q", tn.user)
		}
		if tn.via == "connect" && tn.header != "yes" {
			t.Fatalf("CONNECT without the configured header")
		}
	}
	for _, r := range reqs {
		tn, ok := upstream[r.remote]
		if !ok {
			t.Fatalf("%s came from %s, not through the proxy (tunnels %v)", r.path, r.remote, tunnels)
		}
		// Plain HTTP through an HTTP proxy is in absolute form with net/http (the download below
		// is Go mode's executor in both modes); everything else tunnels, and in Rust mode the
		// core tunnels plain HTTP too.
		want := "connect"
		if via == "socks5" {
			want = "socks5"
		} else if mode == "" && ((!ws && client == ModeGo) || r.path == "/file") {
			want = "forward"
		}
		if tn.via != want {
			t.Fatalf("%s went via %s, want %s", r.path, tn.via, want)
		}
	}
}

// A server whose certificate the given CA didn't sign hears nothing: not the first message, not
// the headers. Nor does one the system's CAs don't know, when no CA is given.
// covers: RULE-TLS-01
func TestAnUntrustedServerHearsNothing(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, given := range []bool{false, true} {
			t.Run(fmt.Sprintf("ws=%v/ca=%v", ws, given), func(t *testing.T) {
				srv, ts := startServer(t, newTestCA(t, "the server's CA"), "tls")
				cfg := clientConfig(ts, ws)
				if given {
					cfg.TLS.CAPEM = newTestCA(t, "another CA").pem
				}
				_, failures := run(t, cfg, srv)
				if len(failures) == 0 {
					t.Fatal("no connect failure")
				}
				if !strings.Contains(failures[0], "certificate") {
					t.Fatalf("failed for another reason: %s", failures[0])
				}
				if reqs := srv.requests(); len(reqs) > 0 {
					t.Fatalf("the server got %d requests", len(reqs))
				}
			})
		}
	}
}

// A server that requires a client certificate refuses a client without one, and one signed by
// another CA.
// covers: RULE-TLS-02
func TestWithoutTheRightClientCertificateNoConnection(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, other := range []bool{false, true} {
			t.Run(fmt.Sprintf("ws=%v/other=%v", ws, other), func(t *testing.T) {
				ca := newTestCA(t, "test CA")
				srv, ts := startServer(t, ca, "mtls")
				cfg := clientConfig(ts, ws)
				cfg.TLS.CAPEM = ca.pem
				if other {
					cfg.TLS.CertPEM, cfg.TLS.KeyPEM, _ = newTestCA(t, "another CA").issue(t, "agent", false)
				}
				if _, failures := run(t, cfg, srv); len(failures) == 0 {
					t.Fatal("connected without the right certificate")
				}
				if reqs := srv.requests(); len(reqs) > 0 {
					t.Fatalf("the server got %d requests", len(reqs))
				}
			})
		}
	}
}

// Wrong proxy credentials: nothing gets through.
// covers: RULE-PROXY-01
func TestProxyWithWrongCredentials(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, via := range []string{"http", "socks5"} {
			t.Run(fmt.Sprintf("ws=%v/%s", ws, via), func(t *testing.T) {
				srv, ts := startServer(t, nil, "")
				p := startProxy(t, via, nil)
				cfg := clientConfig(ts, ws)
				cfg.Proxy.URL = p.url(proxyUser, "wrong")
				if _, failures := run(t, cfg, srv); len(failures) == 0 {
					t.Fatal("no connect failure")
				}
				if reqs := srv.requests(); len(reqs) > 0 {
					t.Fatalf("the server got %d requests", len(reqs))
				}
				if _, refused := p.seen(); len(refused) == 0 || refused[0] != "auth" {
					t.Fatalf("the proxy refused %v", refused)
				}
			})
		}
	}
}

// MinVersion and MaxVersion bound the TLS version, over both transports.
func TestTLSVersions(t *testing.T) {
	ca := newTestCA(t, "test CA")
	for _, ws := range []bool{false, true} {
		// Up to 1.2, with a server that speaks 1.2 at most: connects.
		srv, ts := startServer(t, ca, "tls12")
		cfg := clientConfig(ts, ws)
		cfg.TLS = TLS{CAPEM: ca.pem, MaxVersion: tls.VersionTLS12}
		if got, failures := run(t, cfg, srv); len(failures) > 0 || len(got) < 3 {
			t.Fatalf("ws=%v: TLS 1.2: %d messages, failures %v", ws, len(got), failures)
		}
		// 1.3 only: doesn't.
		srv, ts = startServer(t, ca, "tls12")
		cfg = clientConfig(ts, ws)
		cfg.TLS = TLS{CAPEM: ca.pem, MinVersion: tls.VersionTLS13}
		if _, failures := run(t, cfg, srv); len(failures) == 0 {
			t.Fatalf("ws=%v: TLS 1.3 only, with a TLS 1.2 server: no connect failure", ws)
		}
		if reqs := srv.requests(); len(reqs) > 0 {
			t.Fatalf("ws=%v: the server got %d requests", ws, len(reqs))
		}
		// Up to 1.3: the server's certificate, signed for 127.0.0.1, over 1.3 by default.
		srv, ts = startServer(t, ca, "tls")
		cfg = clientConfig(ts, ws)
		cfg.TLS = TLS{CAPEM: ca.pem, MinVersion: tls.VersionTLS13}
		if got, failures := run(t, cfg, srv); len(failures) > 0 || len(got) < 3 {
			t.Fatalf("ws=%v: TLS 1.3: %d messages, failures %v", ws, len(got), failures)
		}
	}
}

// The application's own HTTP client: WebSocket goes through its proxy and trusts its CAs too.
// covers: RULE-PROXY-01
func TestWebSocketTakesTheApplicationsHTTPClient(t *testing.T) {
	ca := newTestCA(t, "test CA")
	for _, ws := range []bool{false, true} {
		srv, ts := startServer(t, ca, "tls")
		p := startProxy(t, "http", ca)
		u, err := url.Parse(p.url(proxyUser, proxyPass))
		if err != nil {
			t.Fatal(err)
		}
		cfg := clientConfig(ts, ws)
		cfg.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u),
			TLSClientConfig: &tls.Config{RootCAs: ca.pool}, ProxyConnectHeader: http.Header{"X-Proxy-Test": {"yes"}}}}
		if got, failures := run(t, cfg, srv); len(failures) > 0 || len(got) < 3 {
			t.Fatalf("ws=%v: %d messages, failures %v", ws, len(got), failures)
		}
		tunnels, _ := p.seen()
		upstream := map[string]bool{}
		for _, tn := range tunnels {
			upstream[tn.upstream] = tn.header == "yes"
		}
		for _, r := range srv.requests() {
			if !upstream[r.remote] {
				t.Fatalf("ws=%v: a request came from %s, not through the proxy with the header", ws, r.remote)
			}
		}
	}
}

// With no proxy configured, the platform's default proxy (the environment's, stood in for here)
// carries HTTP, WebSocket and downloads alike; Direct ignores it.
// covers: RULE-PROXY-01
func TestDefaultProxyOrDirect(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("ws=%v/direct=%v", ws, direct), func(t *testing.T) {
				srv, ts := startServer(t, nil, "")
				p := startProxy(t, "http", nil)
				u, err := url.Parse(p.url(proxyUser, proxyPass))
				if err != nil {
					t.Fatal(err)
				}
				defer func(old func(*http.Request) (*url.URL, error)) { envProxy = old }(envProxy)
				envProxy = http.ProxyURL(u)
				cfg := clientConfig(ts, ws)
				cfg.Proxy.Direct = direct
				if got, failures := run(t, cfg, srv); len(failures) > 0 || len(got) < 3 {
					t.Fatalf("%d messages, failures %v", len(got), failures)
				}
				conn, err := cfg.connection()
				if err != nil {
					t.Fatal(err)
				}
				x := newExecutor(conn)
				x.stateFile = filepath.Join(t.TempDir(), "state")
				in := x.download(context.Background(), &corepb.Download{Id: 1, Url: ts.URL + "/file", MaxBytes: 1 << 20, TimeoutMs: 5000})
				if in.GetDownloadResponse().GetStatus() != 200 {
					t.Fatalf("download: %v", in)
				}
				tunnels, _ := p.seen()
				upstream := map[string]bool{}
				for _, tn := range tunnels {
					upstream[tn.upstream] = true
				}
				reqs := srv.requests()
				if len(reqs) == 0 {
					t.Fatal("the server got no requests")
				}
				for _, r := range reqs {
					if upstream[r.remote] == direct {
						t.Fatalf("%s from %s: through the proxy %v, want %v", r.path, r.remote, direct, !direct)
					}
				}
				if direct && len(tunnels) > 0 {
					t.Fatalf("direct, but the proxy opened %d tunnels", len(tunnels))
				}
			})
		}
	}
}

// Options that can't work stop Start, with the reason.
func TestBadOptions(t *testing.T) {
	ca := newTestCA(t, "test CA")
	cert, key, _ := ca.issue(t, "agent", false)
	other, _, _ := ca.issue(t, "other", false)
	wrapped := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		return http.DefaultTransport.RoundTrip(r)
	})}
	cases := map[string]Config{
		"not PEM":                        {TLS: TLS{CAPEM: []byte("junk")}},
		"system CAs only":                {TLS: TLS{IncludeSystemCAs: true}},
		"certificate without key":        {TLS: TLS{CertPEM: cert}},
		"key of another cert":            {TLS: TLS{CertPEM: other, KeyPEM: key}},
		"TLS 1.1":                        {TLS: TLS{MinVersion: tls.VersionTLS11}},
		"TLS 1.1 in Config":              {TLS: TLS{Config: &tls.Config{MinVersion: tls.VersionTLS10}}},
		"max below min":                  {TLS: TLS{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS12}},
		"proxy scheme":                   {Proxy: Proxy{URL: "ftp://127.0.0.1:1"}},
		"proxy without host":             {Proxy: Proxy{URL: "http://"}},
		"header without proxy":           {Proxy: Proxy{ConnectHeader: http.Header{"A": {"b"}}}},
		"header to SOCKS5":               {Proxy: Proxy{URL: "socks5://127.0.0.1:1", ConnectHeader: http.Header{"A": {"b"}}}},
		"HTTPClient and TLS":             {HTTPClient: &http.Client{}, TLS: TLS{CAPEM: ca.pem}},
		"HTTPClient and Proxy":           {HTTPClient: &http.Client{}, Proxy: Proxy{URL: "http://127.0.0.1:1"}},
		"Direct and URL":                 {Proxy: Proxy{Direct: true, URL: "http://127.0.0.1:1"}},
		"Direct and header":              {Proxy: Proxy{Direct: true, ConnectHeader: http.Header{"A": {"b"}}}},
		"HTTPClient and Direct":          {HTTPClient: &http.Client{}, Proxy: Proxy{Direct: true}},
		"SOCKS4":                         {Proxy: Proxy{URL: "socks4://127.0.0.1:1"}},
		"SOCKS4a":                        {Proxy: Proxy{URL: "socks4a://127.0.0.1:1"}},
		"unknown proxy scheme":           {Proxy: Proxy{URL: "quic://127.0.0.1:1"}},
		"ResolveLocally with HTTP":       {Proxy: Proxy{URL: "http://127.0.0.1:1", ResolveLocally: true}},
		"ResolveLocally with HTTPS":      {Proxy: Proxy{URL: "https://127.0.0.1:1", ResolveLocally: true}},
		"Direct and ResolveLocally":      {Proxy: Proxy{Direct: true, ResolveLocally: true}},
		"HTTPClient and ResolveLocally":  {HTTPClient: &http.Client{}, Proxy: Proxy{ResolveLocally: true}},
		"WebSocket, not *http.Transport": {HTTPClient: wrapped, Endpoint: "ws://127.0.0.1:1"},
	}
	for name, cfg := range cases {
		if cfg.Endpoint == "" {
			cfg.Endpoint = "http://127.0.0.1:1"
		}
		// In each mode that takes the option: an HTTPClient is Go mode's (and selects it).
		modes := []Mode{ModeGo, ModeRust}
		if cfg.HTTPClient != nil {
			modes = []Mode{"", ModeGo}
		}
		for _, mode := range modes {
			cfg.Mode = mode
			if c, err := Start(context.Background(), cfg, Status{}, nil); err == nil {
				c.Stop(context.Background())
				t.Errorf("%s (Mode %q): started", name, mode)
			}
		}
	}
	// A wrapped RoundTripper is fine over plain HTTP (the HTTPClient selects Go mode).
	c, err := Start(context.Background(), Config{Endpoint: "http://127.0.0.1:1", HTTPClient: wrapped}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Stop(context.Background())
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Through a SOCKS5 proxy, socks5:// and socks5h:// alike, the proxy resolves the server's name:
// it is asked for "localhost" (ATYP DOMAIN). With ResolveLocally the client resolves it and asks
// for an address (ATYP IPv4 or IPv6). The same for a proxy from the environment, over HTTP,
// WebSocket and downloads, with and without TLS.
// covers: RULE-PROXY-01
func TestSOCKS5NameOrAddress(t *testing.T) {
	ca := newTestCA(t, "test CA")
	for _, ws := range []bool{false, true} {
		for _, mode := range []string{"", "tls"} {
			for _, scheme := range []string{"socks5", "socks5h"} {
				for _, local := range []bool{false, true} {
					for _, env := range []bool{false, true} {
						name := fmt.Sprintf("ws=%v/tls=%q/%s/local=%v/env=%v", ws, mode, scheme, local, env)
						t.Run(name, func(t *testing.T) { checkSOCKS5Name(t, ca, ws, mode, scheme, local, env) })
					}
				}
			}
		}
	}
}

func checkSOCKS5Name(t *testing.T, ca *testCA, ws bool, mode, scheme string, local, env bool) {
	srv, ts := startServer(t, ca, mode)
	byName := func(u string) string { return strings.Replace(u, "127.0.0.1", "localhost", 1) }
	cfg := clientConfig(ts, ws)
	cfg.Endpoint = byName(cfg.Endpoint)
	if mode != "" {
		cfg.TLS.CAPEM = ca.pem
	}
	p := startProxy(t, scheme, nil)
	if env {
		u, err := url.Parse(p.url(proxyUser, proxyPass))
		if err != nil {
			t.Fatal(err)
		}
		defer func(old func(*http.Request) (*url.URL, error)) { envProxy = old }(envProxy)
		envProxy = http.ProxyURL(u)
	} else {
		cfg.Proxy.URL = p.url(proxyUser, proxyPass)
	}
	cfg.Proxy.ResolveLocally = local

	got, failures := run(t, cfg, srv)
	if len(failures) > 0 || len(got) < 3 {
		t.Fatalf("%d messages, failures %v", len(got), failures)
	}
	conn, err := cfg.connection()
	if err != nil {
		t.Fatal(err)
	}
	x := newExecutor(conn)
	x.stateFile = filepath.Join(t.TempDir(), "state")
	in := x.download(context.Background(), &corepb.Download{Id: 1, Url: byName(ts.URL) + "/file", MaxBytes: 1 << 20, TimeoutMs: 5000})
	if in.GetDownloadResponse().GetStatus() != 200 {
		t.Fatalf("download: %v", in)
	}

	tunnels, refused := p.seen()
	for _, r := range refused {
		// With ResolveLocally, localhost may resolve to ::1 first, where no server listens: the
		// client tries its next address.
		if !local || !strings.HasPrefix(r, "target:") {
			t.Fatalf("the proxy refused: %v", refused)
		}
	}
	upstream := map[string]bool{}
	for _, tn := range tunnels {
		upstream[tn.upstream] = true
		if local {
			if (tn.atyp != 1 && tn.atyp != 4) || net.ParseIP(tn.target) == nil {
				t.Fatalf("ResolveLocally: the proxy was asked for %q (ATYP %d), want an address", tn.target, tn.atyp)
			}
		} else if tn.atyp != 3 || tn.target != "localhost" {
			t.Fatalf("the proxy was asked for %q (ATYP %d), want the name localhost (ATYP 3)", tn.target, tn.atyp)
		}
	}
	reqs := srv.requests()
	if len(reqs) == 0 {
		t.Fatal("the server got no requests")
	}
	for _, r := range reqs {
		if !upstream[r.remote] {
			t.Fatalf("%s came from %s, not through the proxy (tunnels %v)", r.path, r.remote, tunnels)
		}
	}
}

// A proxy from the environment follows the rules of one configured: SOCKS4 and unknown schemes,
// and an HTTP proxy with ResolveLocally, are refused with the reason. Nothing goes direct.
// covers: RULE-PROXY-01
func TestEnvironmentProxyRules(t *testing.T) {
	cases := []struct {
		url   string
		local bool
		why   string
	}{
		{"socks4://127.0.0.1:1", false, "SOCKS4"},
		{"socks4a://127.0.0.1:1", false, "SOCKS4"},
		{"quic://127.0.0.1:1", false, "proxy scheme"},
		{"http://127.0.0.1:1", true, "ResolveLocally"},
	}
	for _, ws := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("ws=%v/%s/local=%v", ws, c.url, c.local), func(t *testing.T) {
				srv, ts := startServer(t, nil, "")
				u, err := url.Parse(c.url)
				if err != nil {
					t.Fatal(err)
				}
				defer func(old func(*http.Request) (*url.URL, error)) { envProxy = old }(envProxy)
				envProxy = http.ProxyURL(u)
				cfg := clientConfig(ts, ws)
				cfg.Proxy.ResolveLocally = c.local
				_, failures := run(t, cfg, srv)
				if len(failures) == 0 {
					t.Fatal("no connect failure")
				}
				if !strings.Contains(failures[0], c.why) {
					t.Fatalf("failed for another reason: %s", failures[0])
				}
				conn, err := cfg.connection()
				if err != nil {
					t.Fatal(err)
				}
				x := newExecutor(conn)
				x.stateFile = filepath.Join(t.TempDir(), "state")
				if in := x.download(context.Background(), &corepb.Download{Id: 1, Url: ts.URL + "/file", MaxBytes: 1 << 20, TimeoutMs: 5000}); in.GetDownloadFailed() == nil {
					t.Fatal("downloaded")
				}
				if reqs := srv.requests(); len(reqs) > 0 {
					t.Fatalf("the server got %d requests", len(reqs))
				}
			})
		}
	}
}

// SOCKS4 is refused by name, not as just another unknown scheme.
func TestSOCKS4Refused(t *testing.T) {
	for _, scheme := range []string{"socks4", "socks4a"} {
		_, err := newConnection(TLS{}, Proxy{URL: scheme + "://127.0.0.1:1080"})
		if err == nil || !strings.Contains(err.Error(), "SOCKS4") {
			t.Errorf("%s: %v", scheme, err)
		}
	}
}
