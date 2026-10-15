// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

// Proxies for scenarios with "server": {"proxy": {...}}: an HTTP proxy (CONNECT tunnels, and
// plain requests in absolute form, which is how HTTP clients send http:// URLs through a proxy)
// or a SOCKS5 proxy (RFC 1928, with RFC 1929 username/password when "auth" is set).
//
// They forward to this server only: a target on any other port is refused. Every tunnel is
// logged ("proxy_tunnel") with the address its connection to the server comes from, which is
// the "remote" the server logs for each request: a check can match every message to a tunnel.

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type proxy struct {
	s      *server
	opts   *ProxyOpts
	ln     net.Listener
	server string // the address the proxy dials: this server, on loopback
	port   string // the server's port: the only target the proxy forwards to
	mu     sync.Mutex
	nextID int
}

func startProxy(s *server, opts *ProxyOpts, listenHost, serverPort string) (*proxy, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		return nil, err
	}
	p := &proxy{s: s, opts: opts, ln: ln, server: net.JoinHostPort("127.0.0.1", serverPort), port: serverPort}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.nextID++
			id := p.nextID
			p.mu.Unlock()
			go p.serve(id, c)
		}
	}()
	return p, nil
}

func (p *proxy) Close() { p.ln.Close() }

func (p *proxy) refuse(id int, c net.Conn, reason, detail string) {
	p.s.log.emit("proxy_refused", map[string]any{"proxy": p.opts.Kind, "id": id, "client": c.RemoteAddr().String(),
		"reason": reason, "detail": detail})
}

// dial connects to the server for a client that asked for target, if the target is this server.
func (p *proxy) dial(target string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	if port != p.port {
		return nil, fmt.Errorf("target %s is not this server", target)
	}
	return net.DialTimeout("tcp", p.server, 5*time.Second)
}

func (p *proxy) tunnel(id int, c net.Conn, via, target, user string, up net.Conn) {
	p.s.log.emit("proxy_tunnel", map[string]any{"proxy": p.opts.Kind, "id": id, "client": c.RemoteAddr().String(),
		"via": via, "target": target, "user": user, "upstream": up.LocalAddr().String()})
}

func (p *proxy) serve(id int, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if p.opts.Kind == "socks5" {
		p.serveSOCKS5(id, c)
	} else {
		p.serveHTTP(id, c)
	}
}

// pipe copies both ways until either side is done, then closes both.
func pipe(client io.Reader, c net.Conn, up net.Conn) {
	c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	c.Close()
	up.Close()
	<-done
}

// ---------------------------------------------------------------------------------------------
// HTTP: CONNECT, and requests in absolute form

func (p *proxy) serveHTTP(id int, c net.Conn) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.refuse(id, c, "protocol", err.Error())
		return
	}
	user, ok := p.httpAuth(req)
	if !ok {
		p.refuse(id, c, "auth", "missing or wrong Proxy-Authorization")
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n"+
			"Proxy-Authenticate: Basic realm=\"conformance\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	if req.Method == http.MethodConnect {
		target := req.Host
		up, err := p.dial(target)
		if err != nil {
			p.refuse(id, c, "target", err.Error())
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		p.tunnel(id, c, "connect", target, user, up)
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		pipe(br, c, up)
		return
	}
	// A plain request through the proxy: its URL is absolute. The rest of the connection is
	// passed on as it comes; the server takes requests in absolute form too.
	target := req.URL.Host
	if req.URL.Port() == "" {
		target = net.JoinHostPort(req.URL.Hostname(), "80")
	}
	if !req.URL.IsAbs() {
		p.refuse(id, c, "protocol", "request not in absolute form: "+req.RequestURI)
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	up, err := p.dial(target)
	if err != nil {
		p.refuse(id, c, "target", err.Error())
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	p.tunnel(id, c, "forward", target, user, up)
	req.Header.Del("Proxy-Authorization")
	if err := req.WriteProxy(up); err != nil {
		up.Close()
		return
	}
	pipe(br, c, up)
}

// httpAuth checks Proxy-Authorization (Basic) when the proxy needs credentials, and returns the
// user it names.
func (p *proxy) httpAuth(req *http.Request) (string, bool) {
	a := p.opts.Auth
	h := req.Header.Get("Proxy-Authorization")
	user := ""
	if len(h) > 6 && h[:6] == "Basic " {
		if raw, err := base64.StdEncoding.DecodeString(h[6:]); err == nil {
			for i := range raw {
				if raw[i] == ':' {
					user = string(raw[:i])
					if a != nil && user == a.User && string(raw[i+1:]) == a.Pass {
						return user, true
					}
					break
				}
			}
		}
	}
	return user, a == nil
}

// ---------------------------------------------------------------------------------------------
// SOCKS5

func (p *proxy) serveSOCKS5(id int, c net.Conn) {
	br := bufio.NewReader(c)
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || hdr[0] != 5 {
		p.refuse(id, c, "protocol", "not SOCKS5")
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		p.refuse(id, c, "protocol", err.Error())
		return
	}
	want := byte(0x00) // no authentication
	if p.opts.Auth != nil {
		want = 0x02 // username/password
	}
	offered := false
	for _, m := range methods {
		offered = offered || m == want
	}
	if !offered {
		c.Write([]byte{5, 0xff})
		p.refuse(id, c, "auth", fmt.Sprintf("client offered methods %v, not %d", methods, want))
		return
	}
	c.Write([]byte{5, want})
	user := ""
	if want == 0x02 {
		u, pass, err := readUserPass(br)
		user = u
		if err != nil || u != p.opts.Auth.User || pass != p.opts.Auth.Pass {
			c.Write([]byte{1, 1})
			p.refuse(id, c, "auth", "wrong username or password")
			return
		}
		c.Write([]byte{1, 0})
	}
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != 5 {
		p.refuse(id, c, "protocol", "bad request")
		return
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		n, err := br.ReadByte()
		if err != nil {
			return
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(br, name); err != nil {
			return
		}
		host = string(name)
	default:
		p.refuse(id, c, "protocol", "address type "+strconv.Itoa(int(req[3])))
		c.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))))
	if req[1] != 1 { // CONNECT only
		p.refuse(id, c, "protocol", "command "+strconv.Itoa(int(req[1])))
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	up, err := p.dial(target)
	if err != nil {
		p.refuse(id, c, "target", err.Error())
		c.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	p.tunnel(id, c, "socks5", target, user, up)
	reply := []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	if a, ok := up.LocalAddr().(*net.TCPAddr); ok {
		if ip4 := a.IP.To4(); ip4 != nil {
			copy(reply[4:8], ip4)
		}
		binary.BigEndian.PutUint16(reply[8:], uint16(a.Port))
	}
	c.Write(reply)
	pipe(br, c, up)
}

func readUserPass(br *bufio.Reader) (string, string, error) {
	ver, err := br.ReadByte()
	if err != nil {
		return "", "", err
	}
	if ver != 1 {
		return "", "", errors.New("bad username/password version")
	}
	field := func() (string, error) {
		n, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		b := make([]byte, n)
		_, err = io.ReadFull(br, b)
		return string(b), err
	}
	u, err := field()
	if err != nil {
		return "", "", err
	}
	pw, err := field()
	return u, pw, err
}
