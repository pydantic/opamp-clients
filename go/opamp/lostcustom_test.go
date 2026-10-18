// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// TestACustomMessageOnALostConnectionGoesAgain: over WebSocket a custom message goes in a
// message the server never reads, a push the server wrote before that (unsolicited: an offer)
// reaches the client after it, and then the connection drops. The push is no answer to the
// message, but the core takes it for one (Unanswered::answer pops the oldest unanswered message),
// so when the connection goes nothing is unanswered and the custom message is not sent again:
// lost, with no CustomMessageNotSent (RULE-CLIENT-10: "again if the message carrying one is
// lost"). TestSchedules found it (base seed 8156328092424311353): race: offer(0); call; ws drop.
//
// covers: RULE-CLIENT-10
func TestACustomMessageOnALostConnectionGoesAgain(t *testing.T) {
	for _, mode := range []Mode{ModePlatform, ModeCore} {
		t.Run(modeName(mode), func(t *testing.T) { customMessageOnALostConnectionGoesAgain(t, mode) })
	}
}

type lostCustomServer struct {
	mu      sync.Mutex
	conns   int
	got     []*protobufs.AgentToServer
	first   chan *pipeEnd // the first connection's server end, once its hello is answered
	release chan struct{} // the first connection: push, then drop
}

func (s *lostCustomServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	end := pipeOf(r)
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	s.mu.Lock()
	s.conns++
	n := s.conns
	s.mu.Unlock()
	answer := func(m *protobufs.AgentToServer) {
		b, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.GetInstanceUid()})
		_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...))
	}
	read := func() *protobufs.AgentToServer {
		_, data, err := c.ReadMessage()
		if err != nil || len(data) == 0 {
			return nil
		}
		m := &protobufs.AgentToServer{}
		if proto.Unmarshal(data[1:], m) != nil {
			return nil
		}
		s.mu.Lock()
		s.got = append(s.got, m)
		s.mu.Unlock()
		return m
	}
	if n == 1 {
		// The hello, answered; then read no more.
		if m := read(); m != nil {
			answer(m)
		}
		s.first <- end
		<-s.release
		// A push (an unsolicited ServerToAgent: an offer, say), then the connection drops with
		// what the client wrote since unread.
		b, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: modelUID[:]})
		_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...))
		_ = end.Close()
		return
	}
	for {
		m := read()
		if m == nil {
			return
		}
		answer(m)
	}
}

func (s *lostCustomServer) customs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.got {
		if cm := m.GetCustomMessage(); cm != nil {
			out = append(out, string(cm.GetData()))
		}
	}
	return out
}

func customMessageOnALostConnectionGoesAgain(t *testing.T, mode Mode) {
	synctest.Test(t, func(t *testing.T) {
		n := newPipeNet()
		srv := &lostCustomServer{first: make(chan *pipeEnd, 1), release: make(chan struct{})}
		hs := &http.Server{Handler: srv, ConnContext: pipeConnContext}
		go func() { _ = hs.Serve(n) }()
		cfg := Config{Endpoint: "ws://opamp.test/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: 30 * time.Second,
			Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)}
		tr := onPipes(&cfg, mode, n)
		var mu sync.Mutex
		notSent := 0
		c, err := Start(context.Background(), cfg, Status{
			CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{propCapability}},
		}, func(_ *Client, n Notification) {
			if n.Kind == CustomMessageNotSent {
				mu.Lock()
				notSent++
				mu.Unlock()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		<-srv.first
		synctest.Wait()
		// The custom message goes on the first connection, which the server no longer reads.
		c.SendCustomMessage(&protobufs.CustomMessage{Capability: propCapability, Type: "t", Data: []byte(strconv.Itoa(7))})
		synctest.Wait()
		close(srv.release)
		// Reconnected (after the backoff), everything answered: the message must arrive, or the
		// application be told it won't.
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		mu.Lock()
		told := notSent
		mu.Unlock()
		srv.mu.Lock()
		conns := srv.conns
		srv.mu.Unlock()
		if got := srv.customs(); len(got) == 0 && told == 0 {
			t.Errorf("custom message 7 lost: never arrived (%d connections), and no CustomMessageNotSent", conns)
		}
		_ = c.Stop(context.Background())
		closeIdle(tr)
		n.shutdown()
		_ = hs.Close()
	})
}
