// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// connection is how the client reaches servers: one HTTP transport for OpAMP requests and
// package downloads, and WebSocket over the same TLS and proxy. It is a function of its options
// alone, so connection settings a server offers can have one of their own.
type connection struct {
	// http carries OpAMP requests and package downloads, one round trip each: a 3xx goes to the
	// core as it is, which decides. (http.Client, even told to follow no redirect, fails a
	// response whose Location it can't parse; and it would add cookies from a jar.)
	http http.RoundTripper

	// WebSocket. gorilla's own proxy support has no https:// proxies and no CONNECT headers,
	// so the connection makes its tunnels itself and gorilla only speaks TLS and WebSocket.
	wsTLS         *tls.Config // HTTP/1.1 only: gorilla can't speak HTTP/2
	proxy         func(*http.Request) (*url.URL, error)
	connectHeader func(ctx context.Context, proxy *url.URL, target string) (http.Header, error)
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
	wsErr         error // why this connection can't do WebSocket
	// resolveLocally: through a SOCKS5 proxy, resolve the server's name here and give the proxy
	// an address (Proxy.ResolveLocally), over HTTP and WebSocket alike.
	resolveLocally bool

	own *http.Transport // the transport newConnection made, closed with the client
}

// envProxy is the platform's default proxy: the environment's. A variable so tests can stand in
// for it (net/http reads the environment once, and never proxies localhost).
var envProxy = environmentProxy

// environmentProxy is the environment's proxy for req, but none under CGI: REQUEST_METHOD set at
// all, even empty (net/http's rule is non-empty, and then fails the request), since a CGI request's
// Proxy header becomes HTTP_PROXY (httpoxy). Every SDK reads it so (tests/shell-tables/proxy.json).
func environmentProxy(req *http.Request) (*url.URL, error) {
	if _, cgi := os.LookupEnv("REQUEST_METHOD"); cgi {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// newConnection makes a connection from the TLS and proxy options.
func newConnection(t TLS, p Proxy) (*connection, error) {
	cfg, err := t.tlsConfig()
	if err != nil {
		return nil, fmt.Errorf("opamp: TLS: %w", err)
	}
	pu, err := p.proxyURL()
	if err != nil {
		return nil, fmt.Errorf("opamp: proxy: %w", err)
	}
	var proxy func(*http.Request) (*url.URL, error)
	switch {
	case p.Direct:
	case pu != nil:
		proxy = http.ProxyURL(pu)
	default:
		// The environment's proxy, by the same rules as one configured.
		proxy = checkedProxy(envProxy, p.ResolveLocally)
	}
	// No connect or handshake timeouts: how long anything may take is the core's.
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	header := p.ConnectHeader.Clone()
	conn := &connection{proxy: proxy, dial: dialer.DialContext, resolveLocally: p.ResolveLocally}
	// net/http gives a SOCKS5 proxy the server's name. To give it an address instead, the
	// transport goes "direct" and its dialer makes the tunnel, to the proxy that viaSOCKS5 put
	// in the request's context.
	trProxy, dial := proxy, dialer.DialContext
	if p.ResolveLocally && proxy != nil {
		trProxy = nil
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if u, _ := ctx.Value(socksProxyKey{}).(*url.URL); u != nil {
				return conn.dialVia(ctx, u, network, addr)
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	tr := &http.Transport{
		Proxy:                 trProxy,
		ProxyConnectHeader:    header,
		DialContext:           dial,
		TLSClientConfig:       cfg.Clone(), // net/http adds h2 to its copy's NextProtos
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	var rt http.RoundTripper = tr
	if p.ResolveLocally && proxy != nil {
		rt = viaSOCKS5{tr: tr, proxy: proxy}
	}
	conn.http = rt
	conn.wsTLS = http1Only(cfg)
	conn.connectHeader = func(context.Context, *url.URL, string) (http.Header, error) {
		return header, nil
	}
	conn.own = tr
	return conn, nil
}

// checkedProxy refuses the proxies f names that the client can't use (checkProxyScheme): a
// request that would go through one fails with the reason, rather than going some other way.
func checkedProxy(f func(*http.Request) (*url.URL, error), resolveLocally bool) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		u, err := f(req)
		if err != nil || u == nil {
			return u, err
		}
		if err := checkProxyScheme(u, resolveLocally); err != nil {
			return nil, fmt.Errorf("proxy %s: %w", u.Redacted(), err)
		}
		return u, nil
	}
}

type socksProxyKey struct{}

// viaSOCKS5 carries HTTP requests with ResolveLocally: it puts the SOCKS5 proxy for each request
// in its context, for the transport's dialer to tunnel through. (With ResolveLocally every proxy
// is SOCKS5; the proxy function refuses any other.) The transport's connections are pooled by
// scheme and address, which pick the proxy.
type viaSOCKS5 struct {
	tr    *http.Transport
	proxy func(*http.Request) (*url.URL, error)
}

func (v viaSOCKS5) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := v.proxy(req)
	if err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	if u != nil {
		req = req.WithContext(context.WithValue(req.Context(), socksProxyKey{}, u))
	}
	return v.tr.RoundTrip(req)
}

// close drops the idle connections of a transport the client made itself (not the
// application's).
func (c *connection) close() {
	if c.own != nil {
		c.own.CloseIdleConnections()
	}
}

// connectionFrom makes a connection from the application's HTTP client. OpAMP requests and
// downloads go through its Transport as it is, one round trip each (no redirect followed);
// WebSocket takes the TLS settings, proxy, CONNECT headers and dialer of its *http.Transport, and
// can't be used with any other RoundTripper.
func connectionFrom(c *http.Client) *connection {
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	conn := &connection{http: rt}
	tr, ok := rt.(*http.Transport)
	if !ok {
		conn.wsErr = fmt.Errorf("opamp: HTTPClient.Transport is a %T: WebSocket takes its TLS and proxy "+
			"settings from an *http.Transport only (or use Config.TLS and Config.Proxy)", rt)
		return conn
	}
	conn.wsTLS = http1Only(tr.TLSClientConfig)
	conn.proxy = tr.Proxy
	conn.connectHeader = func(ctx context.Context, proxy *url.URL, target string) (http.Header, error) {
		if tr.GetProxyConnectHeader != nil {
			return tr.GetProxyConnectHeader(ctx, proxy, target)
		}
		return tr.ProxyConnectHeader, nil
	}
	conn.dial = tr.DialContext
	if conn.dial == nil {
		conn.dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	return conn
}

// http1Only copies cfg for WebSocket and for TLS to a proxy: HTTP/1.1 only.
func http1Only(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	c := cfg.Clone()
	c.NextProtos = nil
	return c
}

// dialWebSocket opens a WebSocket connection, through the proxy the connection's proxy function
// names for the URL (as for HTTP: it sees http:// for ws:// and https:// for wss://). It takes as
// long as it takes: the core cancels ctx when it gives up.
func (c *connection) dialWebSocket(ctx context.Context, rawURL string, header http.Header) (*websocket.Conn, *http.Response, error) {
	if c.wsErr != nil {
		return nil, nil, c.wsErr
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	via, err := c.wsProxy(u, header)
	if err != nil {
		return nil, nil, err
	}
	// gorilla bounds the handshake by ctx's deadline only: a cancel closes the connection.
	var stop func() bool
	d := websocket.Dialer{
		ReadBufferSize:  64 << 10,
		TLSClientConfig: c.wsTLS,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := c.dialVia(ctx, via, network, addr)
			if err == nil {
				stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
			}
			return conn, err
		},
	}
	conn, resp, err := d.DialContext(ctx, rawURL, header)
	if stop != nil {
		stop()
	}
	return conn, resp, err
}

// wsProxy is the proxy for WebSocket URL u, if any: the one the connection's proxy function names
// for it as http:// (ws://) or https:// (wss://).
func (c *connection) wsProxy(u *url.URL, header http.Header) (*url.URL, error) {
	if c.proxy == nil {
		return nil, nil
	}
	hu := *u
	switch u.Scheme {
	case "ws":
		hu.Scheme = "http"
	case "wss":
		hu.Scheme = "https"
	}
	return c.proxy(&http.Request{Method: http.MethodGet, URL: &hu, Host: hu.Host, Header: header})
}

// dialVia connects to addr, through the proxy if there is one. With resolveLocally, a SOCKS5
// proxy is given the addresses the name resolves to here, one after another until one connects.
func (c *connection) dialVia(ctx context.Context, proxy *url.URL, network, addr string) (net.Conn, error) {
	if proxy == nil {
		return c.dial(ctx, network, addr)
	}
	if err := checkProxyScheme(proxy, c.resolveLocally); err != nil {
		return nil, fmt.Errorf("proxy %s: %w", proxy.Redacted(), err)
	}
	if !c.resolveLocally {
		return c.tunnel(ctx, proxy, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return c.tunnel(ctx, proxy, addr)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: resolving %s locally: %w", proxy.Redacted(), host, err)
	}
	var first error
	for _, ip := range ips {
		conn, err := c.tunnel(ctx, proxy, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// tunnel connects to addr through the proxy.
func (c *connection) tunnel(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error) {
	conn, err := c.dial(ctx, "tcp", proxyAddr(proxy))
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", proxy.Redacted(), err)
	}
	// The exchange with the proxy is bounded by ctx; the caller sets its own deadlines after.
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	if isSOCKS5(proxy) {
		err = socks5Connect(conn, proxy, addr)
	} else {
		conn, err = c.httpConnect(ctx, conn, proxy, addr)
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy %s: %w", proxy.Redacted(), err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func proxyAddr(u *url.URL) string {
	if port := u.Port(); port != "" {
		return u.Host
	}
	port := map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	return net.JoinHostPort(u.Hostname(), port)
}

// httpConnect opens a tunnel to addr through an HTTP proxy (over TLS for https://), as net/http
// does: Proxy-Authorization from the URL's credentials, and the connection's CONNECT headers.
func (c *connection) httpConnect(ctx context.Context, conn net.Conn, proxy *url.URL, addr string) (net.Conn, error) {
	if proxy.Scheme == "https" {
		cfg := c.wsTLS.Clone()
		cfg.ServerName = proxy.Hostname()
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return conn, err
		}
		conn = tc
	}
	hdr := http.Header{}
	if c.connectHeader != nil {
		h, err := c.connectHeader(ctx, proxy, addr)
		if err != nil {
			return conn, err
		}
		hdr = h.Clone()
		if hdr == nil {
			hdr = http.Header{}
		}
	}
	if u := proxy.User; u != nil {
		pass, _ := u.Password()
		hdr.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pass)))
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: hdr}
	if err := req.Write(conn); err != nil {
		return conn, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return conn, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return conn, fmt.Errorf("CONNECT %s: %s", addr, resp.Status)
	}
	if br.Buffered() > 0 { // the server spoke first, and the reader has some of it
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// socks5Connect asks a SOCKS5 proxy (RFC 1928) for a connection to addr, with the URL's
// username and password (RFC 1929) if it has them. A host name goes to the proxy as it is
// (ATYP DOMAIN), for the proxy to resolve; dialVia resolves it first with ResolveLocally.
func socks5Connect(conn net.Conn, proxy *url.URL, addr string) error {
	methods := []byte{0x00} // no authentication
	if proxy.User != nil {
		methods = []byte{0x00, 0x02} // or username/password
	}
	if _, err := conn.Write(append([]byte{5, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 {
		return errors.New("not a SOCKS5 proxy")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if proxy.User == nil {
			return errors.New("SOCKS5 proxy wants a username and password")
		}
		user := proxy.User.Username()
		pass, _ := proxy.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			return errors.New("SOCKS5 username or password over 255 bytes")
		}
		msg := append([]byte{1, byte(len(user))}, user...)
		msg = append(append(msg, byte(len(pass))), pass...)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return err
		}
		if reply[1] != 0 {
			return errors.New("SOCKS5 proxy refused the username and password")
		}
	default:
		return fmt.Errorf("SOCKS5 proxy accepts none of the methods offered (%#x)", reply[1])
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return fmt.Errorf("port %q", portStr)
	}
	req := []byte{5, 1, 0} // CONNECT
	if ip := net.ParseIP(host); ip.To4() != nil {
		req = append(append(req, 1), ip.To4()...)
	} else if ip != nil {
		req = append(append(req, 4), ip.To16()...)
	} else {
		if len(host) > 255 {
			return errors.New("host name over 255 bytes")
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 5 {
		return errors.New("not a SOCKS5 reply")
	}
	if head[1] != 0 {
		return fmt.Errorf("SOCKS5 proxy: %s", socks5Reply(head[1]))
	}
	// The address the proxy bound, then its port: of no use here.
	var skip int
	switch head[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return err
		}
		skip = int(n[0]) + 2
	default:
		return fmt.Errorf("SOCKS5 reply with address type %d", head[3])
	}
	_, err = io.ReadFull(conn, make([]byte, skip))
	return err
}

func socks5Reply(code byte) string {
	reasons := map[byte]string{1: "general failure", 2: "connection not allowed", 3: "network unreachable",
		4: "host unreachable", 5: "connection refused", 6: "TTL expired", 7: "command not supported",
		8: "address type not supported"}
	if r, ok := reasons[code]; ok {
		return r
	}
	return "reply " + strconv.Itoa(int(code))
}
