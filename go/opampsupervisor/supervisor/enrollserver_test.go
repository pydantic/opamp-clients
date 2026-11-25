// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"cmp"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// enrollServer is an OpAMP server that enrolls agents: an agent that comes with the enrollment
// token is answered with a credential of its own, offered as a connection settings offer, and the
// connection closed (1000). The credential is bound to the agent, which connects with it next.
// refuseClaim, if set, refuses that first connection (a Close of 1008 with the reason). Past
// maxOffers credentials no one claimed, an agent with the token is told to come back later (an
// UNAVAILABLE with the wait, then a Close of 1013).
type enrollServer struct {
	*httptest.Server

	mu          sync.Mutex
	refuseClaim string
	dropClaims  int // how many of the key's connections are dropped unanswered, first
	maxOffers   int
	retryAfter  time.Duration
	// refuseToken, if set, is the reason the token's connection is closed for, unanswered.
	refuseToken string
	// dest, if set, is the endpoint the offers are for, instead of the server's own.
	dest string
	// closeAfter is how long after its answer the server closes the connection.
	closeAfter time.Duration
	// serveToken: the token is answered as any credential, with no offer.
	serveToken bool

	offers, tokenConns, keyConns, later, served int
	// keys are the credentials that came back, in order.
	keys []string
	// health is the last health a host served reported.
	health string
}

const tooManyUnclaimed = "This Collector has too many unclaimed credentials. Retry after one lapses."

func newEnrollServer(t *testing.T) *enrollServer {
	s := &enrollServer{maxOffers: 16, retryAfter: time.Hour}
	up := websocket.Upgrader{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization") == "Bearer token"
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		s.mu.Lock()
		drop := false
		if token {
			s.tokenConns++
		} else {
			s.keyConns++
			s.keys = append(s.keys, r.Header.Get("Authorization"))
			drop = s.dropClaims > 0
			if drop {
				s.dropClaims--
			}
		}
		s.mu.Unlock()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			m := &protobufs.AgentToServer{}
			if len(data) < 1 || data[0] != 0 || proto.Unmarshal(data[1:], m) != nil {
				return
			}
			if drop {
				return
			}
			reply, code, reason := s.answer(token, m)
			if reply != nil {
				b, _ := proto.Marshal(reply)
				if c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...)) != nil {
					return
				}
			}
			if code != 0 {
				s.mu.Lock()
				wait := s.closeAfter
				s.mu.Unlock()
				time.Sleep(wait)
				_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *enrollServer) url() string { return "ws" + s.URL[4:] + "/v1/opamp" }

func (s *enrollServer) answer(token bool, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
	switch {
	case token && s.refuseToken != "":
		return nil, websocket.ClosePolicyViolation, s.refuseToken
	case token && s.serveToken, !token && s.refuseClaim == "":
		s.served++
		if m.Health != nil {
			s.health = m.Health.Status
		}
		return out, 0, ""
	case !token:
		return nil, websocket.ClosePolicyViolation, s.refuseClaim
	case s.offers >= s.maxOffers:
		s.later++
		out.ErrorResponse = &protobufs.ServerErrorResponse{
			Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable, ErrorMessage: tooManyUnclaimed,
			Details: &protobufs.ServerErrorResponse_RetryInfo{RetryInfo: &protobufs.RetryInfo{RetryAfterNanoseconds: uint64(s.retryAfter)}},
		}
		return out, websocket.CloseTryAgainLater, tooManyUnclaimed
	}
	s.offers++
	out.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
		Hash: []byte(fmt.Sprintf("offer-%d", s.offers)),
		Opamp: &protobufs.OpAMPConnectionSettings{DestinationEndpoint: cmp.Or(s.dest, s.url()),
			Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: fmt.Sprintf("Bearer key-%d", s.offers)}}}},
	}
	return out, websocket.CloseNormalClosure, ""
}

// seen is how many credentials the server offered, and how many connections came with the token
// and with a credential it offered.
func (s *enrollServer) seen() (offers, tokenConns, keyConns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offers, s.tokenConns, s.keyConns
}

// claims are the credentials that came back, in order.
func (s *enrollServer) claims() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func (s *enrollServer) set(change func(*enrollServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

// reported is whether a host served reported this health.
func (s *enrollServer) reported(health string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health == health
}
