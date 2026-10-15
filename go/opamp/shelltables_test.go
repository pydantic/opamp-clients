// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

// The shells' shared tables (tests/shell-tables), as this client reads them: every URL the
// core accepts is taken by net/http and gorilla (both read it with net/url), read alike, and sent
// as exactly its target; the environment names the proxy the table says (net/http's
// ProxyFromEnvironment, behind the connection's proxy function); every TLS setting is taken or
// refused as the table says. Where Go's convention differs, the case is listed with its reason.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func shellTable(t *testing.T, name string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "shell-tables", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

// targetServer records the raw request-target of every request (the request line as it came,
// nothing decoded), answering 200 and closing.
type targetServer struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
}

func newTargetServer(t *testing.T) *targetServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &targetServer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				parts := strings.Split(strings.TrimRight(line, "\r\n"), " ")
				if len(parts) == 3 {
					s.mu.Lock()
					s.targets = append(s.targets, parts[1])
					s.mu.Unlock()
				}
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}()
		}
	}()
	return s
}

// since is the targets requested after the first n.
func (s *targetServer) since(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targets) <= n {
		return nil
	}
	return append([]string(nil), s.targets[n:]...)
}

func (s *targetServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets)
}

func (s *targetServer) last(n int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targets) != n+1 {
		return "", false
	}
	return s.targets[n], true
}

// Every URL of the corpus: read by net/url (net/http's and gorilla's parser) as the core reads
// it, and sent through the client's HTTP transport, and for ws:// and wss:// through its
// WebSocket dialer, as exactly its target.
func TestTheSharedURLTable(t *testing.T) {
	var table struct {
		URLs []struct {
			URL, Scheme, Host, Path, Target string
			Port                            int
			Query                           *string
		}
	}
	shellTable(t, "urls.json", &table)
	if len(table.URLs) < 200 {
		t.Fatalf("%d URLs", len(table.URLs))
	}
	conn, err := newConnection(TLS{}, Proxy{Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := newTargetServer(t)
	base := "127.0.0.1:" + strconv.Itoa(srv.ln.Addr().(*net.TCPAddr).Port)
	sent := 0
	var wrong []string
	for _, e := range table.URLs {
		// Read as the executor reads it: http.NewRequestWithContext (net/url) for HTTP, url.Parse
		// for WebSocket (dialWebSocket, and gorilla's Dial).
		u, err := url.Parse(e.URL)
		if req, rerr := http.NewRequestWithContext(context.Background(), http.MethodPost, e.URL, nil); rerr == nil {
			u = req.URL
		} else if err == nil {
			err = rerr
		}
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: refused: %v", e.URL, err))
		} else {
			hostOK := strings.EqualFold(u.Hostname(), e.Host)
			if ip := net.ParseIP(strings.Trim(e.Host, "[]")); ip != nil {
				hostOK = ip.Equal(net.ParseIP(u.Hostname()))
			}
			port := map[string]int{"http": 80, "ws": 80, "https": 443, "wss": 443}[u.Scheme]
			if p := u.Port(); p != "" {
				port, _ = strconv.Atoi(p)
			}
			path := u.EscapedPath()
			if path == "" {
				path = "/"
			}
			var query *string
			if u.ForceQuery || u.RawQuery != "" {
				query = &u.RawQuery
			}
			if !hostOK || u.Scheme != e.Scheme || port != e.Port || path != e.Path || (query == nil) != (e.Query == nil) ||
				(query != nil && *query != *e.Query) {
				wrong = append(wrong, fmt.Sprintf("%s: read as %s %s %d %q %v, the core as %s %s %d %q %v", e.URL,
					u.Scheme, u.Hostname(), port, path, show(query), e.Scheme, e.Host, e.Port, e.Path, show(e.Query)))
			}
		}
		// Sent: the same target.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+base+e.Target, strings.NewReader("x"))
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: target %q not taken: %v", e.URL, e.Target, err))
			continue
		}
		resp, err := conn.http.RoundTrip(req)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: not sent: %v", e.URL, err))
			continue
		}
		resp.Body.Close()
		if got, ok := srv.last(sent); !ok || got != e.Target {
			wrong = append(wrong, fmt.Sprintf("%s: sent %q, not %q", e.URL, got, e.Target))
		}
		sent++
		if e.Scheme == "ws" || e.Scheme == "wss" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c, _, err := conn.dialWebSocket(ctx, "ws://"+base+e.Target, nil)
			cancel()
			if c != nil {
				c.Close()
			}
			if err == nil {
				wrong = append(wrong, fmt.Sprintf("%s: a WebSocket from a server that answered 200", e.URL))
			}
			if got, ok := srv.last(sent); !ok || got != e.Target {
				wrong = append(wrong, fmt.Sprintf("%s: WebSocket sent %q, not %q (%v)", e.URL, got, e.Target, err))
			}
			sent++
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d:\n%s", len(wrong), len(table.URLs), strings.Join(wrong, "\n"))
	}
}

// Rust mode: every URL of the corpus as the endpoint of a client, whose core sends its first
// request (a POST, or a WebSocket upgrade for ws:// and wss://) over Go's connection: the target
// on the wire is exactly the table's.
func TestTheSharedURLTableRustMode(t *testing.T) {
	var table struct {
		URLs []struct{ URL, Scheme, Target string }
	}
	shellTable(t, "urls.json", &table)
	if len(table.URLs) < 200 {
		t.Fatalf("%d URLs", len(table.URLs))
	}
	srv := newTargetServer(t)
	base := "127.0.0.1:" + strconv.Itoa(srv.ln.Addr().(*net.TCPAddr).Port)
	var wrong []string
	for _, e := range table.URLs {
		scheme := "http"
		if e.Scheme == "ws" || e.Scheme == "wss" {
			scheme = "ws"
		}
		before := srv.count()
		c, err := Start(context.Background(), Config{Endpoint: scheme + "://" + base + e.Target, Mode: ModeRust,
			Proxy: Proxy{Direct: true}, InstanceUID: [16]byte{1}, HeartbeatInterval: time.Hour}, Status{}, nil)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: start: %v", e.URL, err))
			continue
		}
		eventually(10*time.Second, func() bool { return len(srv.since(before)) > 0 })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.Stop(ctx); err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: stop: %v", e.URL, err))
		}
		cancel()
		if got := srv.since(before); len(got) == 0 || got[0] != e.Target {
			wrong = append(wrong, fmt.Sprintf("%s: sent %q, not %q", e.URL, got, e.Target))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d:\n%s", len(wrong), len(table.URLs), strings.Join(wrong, "\n"))
	}
}

func show(s *string) string {
	if s == nil {
		return "no query"
	}
	return strconv.Quote(*s)
}

// proxyDifferences are the proxy table's cases where Go's convention (net/http's
// ProxyFromEnvironment, golang.org/x/net/http/httpproxy) differs from the reference: what this
// client takes instead, and why.
var proxyDifferences = map[string]struct{ got, why string }{
	"all-proxy-for-http":  {"", "Go reads no ALL_PROXY: only HTTP_PROXY, HTTPS_PROXY and NO_PROXY (net/http's ProxyFromEnvironment)"},
	"all-proxy-for-https": {"", "Go reads no ALL_PROXY"},
	"empty-falls-to-all":  {"", "Go reads no ALL_PROXY"},
	"no-proxy-port":       {"", "Go's NO_PROXY takes host:port entries, matching that port only (httpproxy)"},
	"localhost":           {"", "Go never proxies localhost (httpproxy: \"requests to localhost are never proxied\")"},
	"loopback":            {"", "Go never proxies a loopback address"},
	"no-proxy-leading-dot-itself": {"http://web-proxy:3128",
		"Go's NO_PROXY: a leading dot matches subdomains only, not the name itself (httpproxy)"},
	"upper-empty-hides-lower": {"http://lower-proxy:1",
		"Go takes the first of HTTP_PROXY and http_proxy that is set and not empty (httpproxy.FromEnvironment)"},
	"value-trimmed":               {"", "Go takes the value as it is: with spaces around it, it is no proxy URL, and the request goes direct"},
	"socks5-from-the-environment": {"", "Go reads no ALL_PROXY"},
	"no-proxy-star-label":         {"", "Go's NO_PROXY takes *.example.com as example.com and its subdomains (httpproxy)"},
	"no-proxy-star-address":       {"", "Go's NO_PROXY * covers every host, addresses too (httpproxy)"},
	"no-proxy-name-not-an-address": {"",
		"Go never proxies a loopback address (httpproxy: localhost and loopback addresses always go direct)"},
	"no-proxy-ipv4-network-not-ipv6": {"",
		"Go's NO_PROXY matches an IPv4-mapped IPv6 address against an IPv4 network (net.IPNet.Contains)"},
	"no-proxy-bracketed-entry": {"", "Go never proxies a loopback address (httpproxy), [::1] included"},
}

const proxyChildEnv = "OPAMP_PROXY_TABLE_CHILD"

// proxyChildren: the proxy table's children at once (each is a fresh test binary).
const proxyChildren = 8

// The proxy the environment names for each case of the table, as the client's connection picks
// it (the environment's proxy, http:// for ws:// and https:// for wss://), in a child process
// with exactly the case's variables: net/http reads the environment once per process.
func TestTheSharedProxyTable(t *testing.T) {
	if u := os.Getenv(proxyChildEnv); u != "" {
		got, err := environmentProxyFor(u)
		if err != nil {
			fmt.Printf("ERROR %v\n", err)
		} else {
			fmt.Printf("PROXY %s\n", got)
		}
		return
	}
	t.Parallel()
	var table struct {
		Cases []struct {
			ID, URL, Why string
			Env          map[string]string
			Proxy        *string
		}
	}
	shellTable(t, "proxy.json", &table)
	if len(table.Cases) < 30 {
		t.Fatalf("%d cases", len(table.Cases))
	}
	// The environment without a proxy variable (nor REQUEST_METHOD, which turns HTTP_PROXY off).
	var base []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUEST_METHOD", proxyChildEnv:
			continue
		}
		base = append(base, kv)
	}
	// A child a case, proxyChildren at once: each spends its time starting the test binary.
	type child struct {
		out []byte
		err error
	}
	children := make([]child, len(table.Cases))
	sem := make(chan struct{}, proxyChildren)
	var wg sync.WaitGroup
	for i, c := range table.Cases {
		env := append([]string(nil), base...)
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestTheSharedProxyTable$", "-test.count=1")
		cmd.Env = append(env, proxyChildEnv+"="+c.URL)
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			children[i].out, children[i].err = cmd.CombinedOutput()
		})
	}
	wg.Wait()
	var wrong []string
	seen := map[string]bool{}
	for i, c := range table.Cases {
		out, err := children[i].out, children[i].err
		if err != nil {
			t.Fatalf("%s: the child: %v\n%s", c.ID, err, out)
		}
		var got string
		found := false
		for _, line := range strings.Split(string(out), "\n") {
			if p, ok := strings.CutPrefix(line, "PROXY "); ok {
				got, found = p, true
			} else if e, ok := strings.CutPrefix(line, "ERROR "); ok {
				got, found = "error: "+e, true
			}
		}
		if !found {
			t.Fatalf("%s: the child said nothing:\n%s", c.ID, out)
		}
		want := ""
		if c.Proxy != nil {
			want = *c.Proxy
		}
		if d, ok := proxyDifferences[c.ID]; ok {
			seen[c.ID] = true
			if got == want {
				wrong = append(wrong, fmt.Sprintf("%s: now as the table says (%q): drop it from proxyDifferences", c.ID, got))
			} else if got != d.got {
				wrong = append(wrong, fmt.Sprintf("%s: %q, neither the table's %q nor the known difference's %q", c.ID, got, want, d.got))
			}
			continue
		}
		if got != want {
			wrong = append(wrong, fmt.Sprintf("%s: %q, the table says %q (%s)", c.ID, got, want, c.Why))
		}
	}
	for id := range proxyDifferences {
		if !seen[id] {
			wrong = append(wrong, fmt.Sprintf("%s: a known difference the table no longer has", id))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d:\n%s", len(wrong), len(table.Cases), strings.Join(wrong, "\n"))
	}
}

// environmentProxyFor is the route the client takes from the environment for rawURL: the proxy
// as scheme://host:port (no credentials), or "" for none.
func environmentProxyFor(rawURL string) (string, error) {
	conn, err := newConnection(TLS{}, Proxy{})
	if err != nil {
		return "", err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	var p *url.URL
	if u.Scheme == "ws" || u.Scheme == "wss" {
		p, err = conn.wsProxy(u, nil)
	} else {
		p, err = conn.proxy(&http.Request{Method: http.MethodPost, URL: u, Host: u.Host, Header: http.Header{}})
	}
	if err != nil || p == nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(proxyAddr(p))
	if err != nil {
		return "", err
	}
	return p.Scheme + "://" + net.JoinHostPort(host, port), nil
}

// proxyRoute is a proxy URL as the table writes a route: scheme://host:port, without
// credentials (no scheme: an HTTP proxy, as the core reads it).
func proxyRoute(p string) string {
	if p == "" {
		return ""
	}
	if !strings.Contains(p, "://") {
		p = "http://" + p
	}
	u, err := url.Parse(p)
	if err != nil {
		return "unparsed: " + p
	}
	host, port, err := net.SplitHostPort(proxyAddr(u))
	if err != nil {
		return "unparsed: " + p
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port)
}

// Rust mode: the proxy each case of the table names, by Rust mode's one rule (the core's
// proxy_query answered from the environment), with no known differences.
//
// covers: RULE-PROXY-02
func TestTheSharedProxyTableRustMode(t *testing.T) {
	var table struct {
		Cases []struct {
			ID, URL, Why string
			Env          map[string]string
			Proxy        *string
		}
	}
	shellTable(t, "proxy.json", &table)
	if len(table.Cases) < 50 {
		t.Fatalf("%d cases", len(table.Cases))
	}
	var wrong []string
	for _, c := range table.Cases {
		env := func(name string) (string, bool) { v, ok := c.Env[name]; return v, ok }
		// As the client answers the core: through Proxy's zero value (the environment's).
		a := newRustProxies(Proxy{}, env).answer(7, c.URL).GetProxyAnswer()
		got := proxyRoute(a.GetProxy())
		want := ""
		if c.Proxy != nil {
			want = *c.Proxy
		}
		if got != want || a.GetId() != 7 || len(a.GetHeaders()) > 0 || a.GetResolveLocally() {
			wrong = append(wrong, fmt.Sprintf("%s: %q (%v), the table says %q (%s)", c.ID, got, a, want, c.Why))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d:\n%s", len(wrong), len(table.Cases), strings.Join(wrong, "\n"))
	}
}

// Rust mode's proxy rule: the corners the table leaves out, and what the answer carries.
//
// covers: RULE-PROXY-02
func TestRustModeProxyRuleCorners(t *testing.T) {
	for _, c := range []struct {
		list, host string
		want       bool
	}{
		{"*", "192.0.2.1", false},
		{"*", "example.com", true},
		{"2001:db8::/32", "2001:db8:1::7", true},
		{"2001:db8::/32", "2001:db9::7", false},
		{"::ffff:10.0.0.0/104", "::ffff:10.1.2.3", true},
		{"10.0.0.0/8", "::ffff:10.1.2.3", false},
		{"fe80::1%eth0", "fe80::1", false}, // a zone: no address, and a name never matches one
		{"010.0.0.1", "10.0.0.1", false},   // leading zeros: no address
		{"example.com", "example.com.", false},
		{".example.com", "a.b.example.com", true},
		{"b.example.com", "a.b.example.com", true},
		{"ample.com", "example.com", false},
		{"EXAMPLE.COM", "Api.Example.Com", true},
		{"", "example.com", false},
		{" , ,", "example.com", false},
	} {
		if got := rustNoProxy(c.list, c.host); got != c.want {
			t.Errorf("NO_PROXY=%q for %s: %v, want %v", c.list, c.host, got, c.want)
		}
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { v, ok := m[name]; return v, ok }
	}
	none := env(nil)
	// Configured: the URL as given (credentials and all), the CONNECT headers, resolve locally;
	// whatever the environment says.
	all := env(map[string]string{"HTTP_PROXY": "http://env:1", "NO_PROXY": "*"})
	a := newRustProxies(Proxy{URL: "http://u:p%40ss@proxy:3128", ConnectHeader: http.Header{"X-B": {"2"}, "X-A": {"1", "3"}}}, all).
		answer(3, "http://example.com/").GetProxyAnswer()
	if a.Proxy != "http://u:p%40ss@proxy:3128" || len(a.Headers) != 3 || a.Headers[0].Name != "X-A" || a.Headers[1].Value != "3" ||
		a.Headers[2].Name != "X-B" || a.ResolveLocally {
		t.Errorf("configured: %v", a)
	}
	a = newRustProxies(Proxy{URL: "socks5://proxy:1080", ResolveLocally: true}, none).answer(3, "wss://example.com/").GetProxyAnswer()
	if a.Proxy != "socks5://proxy:1080" || !a.ResolveLocally {
		t.Errorf("configured SOCKS5, resolving locally: %v", a)
	}
	// Direct: none, whatever the environment says.
	if a := newRustProxies(Proxy{Direct: true}, all).answer(3, "http://example.com/").GetProxyAnswer(); a.Proxy != "" {
		t.Errorf("direct: %v", a)
	}
	// The environment's, with credentials as given; ResolveLocally applies to a SOCKS5 one only.
	a = newRustProxies(Proxy{ResolveLocally: true}, env(map[string]string{"ALL_PROXY": " socks5h://u:p@s:1 "})).
		answer(3, "https://example.com/").GetProxyAnswer()
	if a.Proxy != "socks5h://u:p@s:1" || !a.ResolveLocally {
		t.Errorf("SOCKS5 from the environment, resolving locally: %v", a)
	}
	a = newRustProxies(Proxy{ResolveLocally: true}, env(map[string]string{"https_proxy": "web:3128"})).
		answer(3, "wss://example.com/").GetProxyAnswer()
	if a.Proxy != "web:3128" || a.ResolveLocally {
		t.Errorf("an HTTP proxy from the environment: %v", a)
	}
	if a := newRustProxies(Proxy{}, none).answer(3, "http://example.com/").GetProxyAnswer(); a.Proxy != "" {
		t.Errorf("no variable: %v", a)
	}
}

// tlsDifferences are the TLS table's cases where this client differs from the reference: none.
var tlsDifferences = map[string]string{}

// Every TLS setting of the table, as an application gives it (TLS's fields): the client's
// connection is made, or refused at once.
func TestTheSharedTLSTable(t *testing.T) {
	var table struct {
		Cases []struct {
			ID, CA, Cert, Key, Min, Max, Expect, Why string
		}
	}
	shellTable(t, "tls.json", &table)
	if len(table.Cases) < 20 {
		t.Fatalf("%d cases", len(table.Cases))
	}
	read := func(p string) []byte {
		if p == "" {
			return nil
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "tests", "shell-tables", p))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	versions := map[string]uint16{"": 0, "1.0": tls.VersionTLS10, "1.1": tls.VersionTLS11, "1.2": tls.VersionTLS12, "1.3": tls.VersionTLS13}
	var wrong []string
	for _, c := range table.Cases {
		lo, okLo := versions[c.Min]
		hi, okHi := versions[c.Max]
		got := "refused" // a version Go can't name
		if okLo && okHi {
			opts := TLS{CAPEM: read(c.CA), CertPEM: read(c.Cert), KeyPEM: read(c.Key), MinVersion: lo, MaxVersion: hi}
			goOK := false
			if _, err := newConnection(opts, Proxy{Direct: true}); err == nil {
				goOK = true
			}
			// Rust mode's TLS, from the same settings: Start takes them, or refuses them at once.
			rustOK := false
			if cl, err := Start(context.Background(), Config{Endpoint: "http://127.0.0.1:9/", Mode: ModeRust, TLS: opts,
				Proxy: Proxy{Direct: true}, HeartbeatInterval: time.Hour}, Status{}, nil); err == nil {
				rustOK = true
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = cl.Stop(ctx)
				cancel()
			}
			if goOK != rustOK {
				wrong = append(wrong, fmt.Sprintf("%s: Go mode ok=%v, Rust mode ok=%v", c.ID, goOK, rustOK))
			}
			if goOK && rustOK {
				got = "ok"
			}
		}
		want := c.Expect
		if why, ok := tlsDifferences[c.ID]; ok {
			if got == want {
				wrong = append(wrong, fmt.Sprintf("%s: now as the table says: drop it from tlsDifferences (%s)", c.ID, why))
			}
			continue
		}
		if got != want {
			wrong = append(wrong, fmt.Sprintf("%s: %s, the table says %s (%s)", c.ID, got, want, c.Why))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d:\n%s", len(wrong), len(table.Cases), strings.Join(wrong, "\n"))
	}
}
