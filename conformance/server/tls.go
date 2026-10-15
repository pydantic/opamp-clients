// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

// TLS for scenarios with "server": {"tls": {...}}: a CA made in memory at start, a server
// certificate it signs, and (for client_auth) a client certificate it signs too. The CA and the
// client's certificate and key are written as PEM for the runner to hand to the driver.
//
// The listener does every handshake itself, before net/http sees the connection, so that each
// one is logged: "tls_handshake" when it succeeds (version, client certificate), and
// "tls_handshake_failed" when it doesn't, with the alert the client sent, if any. A connection
// whose handshake succeeded logs "tls_closed" once, when it closes (whoever closes it, or at
// shutdown), with the application bytes the client sent on it: a client that finishes the
// handshake before checking the server's certificate, then hangs up, sent nothing.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hosts the server certificate is valid for: the runner's loopback address, and the name a driver
// in a container reaches the host by.
var serverHosts = []string{"127.0.0.1", "localhost", "host.docker.internal", "::1"}

// ClientSubject is the common name of the client certificate the runner hands to drivers.
const ClientSubject = "conformance-agent"

type issuer struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newKey() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n
}

func newCA(name string) *issuer {
	key := newKey()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"opamp-clients conformance"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &issuer{cert: cert, key: key, der: der}
}

// sign issues a leaf certificate: for the server (hosts set) or a client.
func (ca *issuer) sign(cn string, hosts []string, usage x509.ExtKeyUsage) (tls.Certificate, *ecdsa.PrivateKey) {
	key := newKey()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"opamp-clients conformance"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.der}, PrivateKey: key}, key
}

func pemFile(path, kind string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o644)
}

// tlsFiles are the PEM files written for the runner.
type tlsFiles struct {
	CA         string `json:"ca_file"`
	ClientCert string `json:"client_cert,omitempty"`
	ClientKey  string `json:"client_key,omitempty"`
}

// tlsConfig: a server's TLS config with this certificate.
func tlsConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// HTTP/1.1 only: gorilla/websocket upgrades HTTP/1.1 connections.
		NextProtos: []string{"http/1.1"},
	}
}

// setupTLS makes the CA and certificates for the scenario's TLS options, writes the files the
// runner needs to dir, and returns the server's TLS config.
func setupTLS(opts *TLSOpts, dir string) (*tls.Config, tlsFiles, error) {
	ca := newCA("opamp-clients conformance CA")
	serverCert, _ := ca.sign("conformance-server", serverHosts, x509.ExtKeyUsageServerAuth)
	files := tlsFiles{CA: filepath.Join(dir, "ca.pem")}
	// Untrusted: the driver is handed a CA that did not sign the server's certificate.
	trusted := ca
	if opts.Untrusted {
		trusted = newCA("an unrelated CA")
	}
	if err := pemFile(files.CA, "CERTIFICATE", trusted.der); err != nil {
		return nil, files, err
	}
	cfg := tlsConfig(serverCert)
	switch opts.ClientAuth {
	case "", "none":
		return cfg, files, nil
	case "request":
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	case "require":
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	cfg.ClientCAs = pool
	clientCert, clientKey := ca.sign(ClientSubject, nil, x509.ExtKeyUsageClientAuth)
	files.ClientCert = filepath.Join(dir, "client.pem")
	files.ClientKey = filepath.Join(dir, "client-key.pem")
	keyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err == nil {
		err = pemFile(files.ClientCert, "CERTIFICATE", clientCert.Certificate[0])
	}
	if err == nil {
		err = pemFile(files.ClientKey, "PRIVATE KEY", keyDER)
	}
	return cfg, files, err
}

// ---------------------------------------------------------------------------------------------
// The listener

const handshakeTimeout = 10 * time.Second

// loggingListener logs every TCP connection it accepts ("tcp_accept"), and with a TLS config does
// the handshake itself and logs it. net/http is handed only connections whose handshake succeeded.
type loggingListener struct {
	net.Listener
	s      *server
	tls    *tls.Config
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	nextID int
	live   map[*countingConn]bool // handshake done, not closed yet
	// stopping: the server is shutting down, and closes what is still open.
	stopping atomic.Bool
}

func newLoggingListener(ln net.Listener, s *server, cfg *tls.Config) *loggingListener {
	l := &loggingListener{Listener: ln, s: s, tls: cfg, conns: make(chan net.Conn), done: make(chan struct{}),
		live: map[*countingConn]bool{}}
	go l.acceptLoop()
	return l
}

func (l *loggingListener) acceptLoop() {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			l.Close()
			return
		}
		l.mu.Lock()
		l.nextID++
		id := l.nextID
		l.mu.Unlock()
		go l.admit(id, raw)
	}
}

func (l *loggingListener) admit(id int, raw net.Conn) {
	remote := raw.RemoteAddr().String()
	if l.tls == nil {
		l.s.log.emit("tcp_accept", map[string]any{"tcp": id, "remote": remote})
		l.hand(raw)
		return
	}
	tc := tls.Server(raw, l.tls)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		l.s.log.emit("tls_handshake_failed", map[string]any{"tcp": id, "remote": remote, "error": err.Error(),
			"alert": remoteAlert(err)})
		raw.Close()
		return
	}
	st := tc.ConnectionState()
	ev := map[string]any{"tcp": id, "remote": remote, "version": tls.VersionName(st.Version),
		"cipher": tls.CipherSuiteName(st.CipherSuite), "alpn": st.NegotiatedProtocol, "sni": st.ServerName,
		"client_cert": len(st.PeerCertificates) > 0, "client_subject": nil, "client_verified": len(st.VerifiedChains) > 0}
	if len(st.PeerCertificates) > 0 {
		ev["client_subject"] = st.PeerCertificates[0].Subject.CommonName
	}
	l.s.log.emit("tls_handshake", ev)
	cc := &countingConn{Conn: tc, l: l, id: id}
	l.mu.Lock()
	l.live[cc] = true
	l.mu.Unlock()
	l.hand(cc)
}

// countingConn is a TLS connection whose handshake succeeded. It counts the application bytes
// the client sends and logs "tls_closed" when it closes: closed_by "shutdown" when the server
// stops, else "client" once a read has failed (the client hung up), else "server". net/http
// doesn't see a *tls.Conn, so requests get their TLS state from connTLS.
type countingConn struct {
	*tls.Conn
	l      *loggingListener
	id     int
	bytes  atomic.Int64
	eof    atomic.Bool
	closed sync.Once
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.bytes.Add(int64(n))
	if err != nil {
		c.eof.Store(true)
	}
	return n, err
}

func (c *countingConn) Close() error {
	c.logClosed("server")
	return c.Conn.Close()
}

func (c *countingConn) logClosed(by string) {
	c.closed.Do(func() {
		if by == "server" && c.l.stopping.Load() {
			by = "shutdown"
		} else if by == "server" && c.eof.Load() {
			by = "client"
		}
		c.l.mu.Lock()
		delete(c.l.live, c)
		c.l.mu.Unlock()
		c.l.s.log.emit("tls_closed", map[string]any{"tcp": c.id, "app_bytes": c.bytes.Load(), "closed_by": by})
	})
}

// closeLive closes the connections still open when the server stops (hijacked WebSockets among
// them: http.Server.Close leaves those alone), so each has its "tls_closed".
func (l *loggingListener) closeLive() {
	l.mu.Lock()
	live := make([]*countingConn, 0, len(l.live))
	for c := range l.live {
		live = append(live, c)
	}
	l.mu.Unlock()
	for _, c := range live {
		c.logClosed("shutdown")
		c.Conn.Close()
	}
}

type connKey struct{}

// connTLS is the request's TLS state (r.TLS is nil: net/http is handed a countingConn).
func connTLS(r *http.Request) *tls.ConnectionState {
	if r.TLS != nil {
		return r.TLS
	}
	if cc, ok := r.Context().Value(connKey{}).(*countingConn); ok {
		st := cc.ConnectionState()
		return &st
	}
	return nil
}

func (l *loggingListener) hand(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *loggingListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *loggingListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.Listener.Close()
	})
	return err
}

// remoteAlert is the alert the peer sent, if the handshake failed on one ("bad certificate",
// "unknown certificate authority", ...), or nil.
func remoteAlert(err error) any {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return strings.TrimPrefix(op.Err.Error(), "tls: ")
	}
	if msg := err.Error(); strings.Contains(msg, "remote error: tls: ") {
		return msg[strings.Index(msg, "remote error: tls: ")+len("remote error: tls: "):]
	}
	return nil
}

// tlsInfo describes, for the a2s event, how a request reached the server: its TLS version and the
// client certificate it presented (with TLS), and the peer address the proxy checks match.
func tlsInfo(st *tls.ConnectionState) map[string]any {
	if st == nil {
		return map[string]any{"tls": nil}
	}
	info := map[string]any{"tls": tls.VersionName(st.Version), "client_subject": nil}
	if len(st.PeerCertificates) > 0 {
		info["client_subject"] = st.PeerCertificates[0].Subject.CommonName
	}
	return info
}
