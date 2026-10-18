// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/open-telemetry/opamp-go/protobufs"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
	"google.golang.org/protobuf/proto"
)

// The Supervisor's local OpAMP server, which the collector's opamp extension connects to, and the
// one-shot server of the bootstrap take a request only with the bearer token the Supervisor makes
// at each Start and writes into the extension config (server.ws.headers; configopaque in the
// extension, so redacted in the effective config it reports).

// redactedLocalServerToken stands for the token in what the Supervisor sends on.
const redactedLocalServerToken = "[REDACTED]"

// newLocalServerToken is a new random token: 32 bytes, in hex.
func newLocalServerToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// authorizedLocalRequest says whether req carries the local server's token. Without a token
// (no Start made one) nothing is.
func (s *Supervisor) authorizedLocalRequest(req *http.Request) bool {
	if s.localServerToken == "" {
		return false
	}
	want := []byte("Bearer " + s.localServerToken)
	return subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) == 1
}

// redactLocalServerToken is b without the local server's token, b itself when it holds none.
func (s *Supervisor) redactLocalServerToken(b []byte) []byte {
	token := []byte(s.localServerToken)
	if len(token) == 0 || !bytes.Contains(b, token) {
		return b
	}
	return bytes.ReplaceAll(b, token, []byte(redactedLocalServerToken))
}

// redactLocalServerTokenInConfig is cfg without the local server's token in any file, cfg itself
// when it holds none.
func (s *Supervisor) redactLocalServerTokenInConfig(cfg *protobufs.EffectiveConfig) *protobufs.EffectiveConfig {
	var redacted *protobufs.EffectiveConfig
	for name, file := range cfg.GetConfigMap().GetConfigMap() {
		body := s.redactLocalServerToken(file.GetBody())
		if bytes.Equal(body, file.GetBody()) {
			continue
		}
		if redacted == nil {
			redacted = proto.Clone(cfg).(*protobufs.EffectiveConfig)
		}
		redacted.ConfigMap.ConfigMap[name].Body = body
	}
	if redacted == nil {
		return cfg
	}
	return redacted
}

// localConnection is the collector's connection to the Supervisor's local OpAMP server, which
// takes one at a time. It is held from the connection's OnConnected to its OnConnectionClose,
// so a request that never becomes a connection (a plain HTTP request that fails to decode, a
// failed upgrade) can't hold it, and a closed connection isn't used after it closes.
type localConnection struct {
	mu   sync.Mutex
	conn serverTypes.Connection
	// n counts the connections claimed; the one held is the n-th.
	n uint64
}

// busy says whether a collector is connected.
func (l *localConnection) busy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// claim makes conn the collector's connection, unless another one is: false then.
func (l *localConnection) claim(conn serverTypes.Connection) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return false
	}
	l.conn = conn
	l.n++
	return true
}

// release frees the connection, if it is conn.
func (l *localConnection) release(conn serverTypes.Connection) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == conn {
		l.conn = nil
	}
}

// holds says whether conn is the collector's connection.
func (l *localConnection) holds(conn serverTypes.Connection) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil && l.conn == conn
}

// held is which connection conn is (its count, from 1), while it is the collector's.
func (l *localConnection) held(conn serverTypes.Connection) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil || l.conn != conn {
		return 0, false
	}
	return l.n, true
}

// count is how many connections have been claimed: a later one has a higher count.
func (l *localConnection) count() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// current is the collector's connection, or nil.
func (l *localConnection) current() serverTypes.Connection {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}
