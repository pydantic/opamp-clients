package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func newTestServer(faults []Fault) *server {
	start := time.Now()
	return &server{sc: &Scenario{Faults: faults}, start: start, log: &eventLog{w: &bytes.Buffer{}, start: start},
		lastSeq: -1, open: map[int]*wsConn{}}
}

func roundTrip(t *testing.T, c *websocket.Conn, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
	t.Helper()
	out, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, out...)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	resp := &protobufs.ServerToAgent{}
	if err := proto.Unmarshal(data[1:], resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A message answered UNAVAILABLE over a WebSocket was not taken in, as one answered 503 over HTTP
// isn't: the next message's sequence number is a gap, and the server asks for the full state. A
// client that doesn't resend (opamp-go's WebSocket client ignores the error) gets its status to
// the server that way; otherwise the status that message carried is lost until the next change
// (torture, go/ws, nightly 2026-10-10: "at 1680s the server holds change 89, the application set
// 90").
func TestUnavailableOverWebSocketIsNotTakenIn(t *testing.T) {
	second := 2
	s := newTestServer([]Fault{{OnMessage: &second, Action: "unavailable", RetryAfterS: 1}})
	hs := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer hs.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid := bytes.Repeat([]byte{7}, 16)
	description := func(change string) *protobufs.AgentDescription {
		return &protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{{Key: "driver.change",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: change}}}}}
	}

	first := roundTrip(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 0, AgentDescription: description("0")})
	if first.ErrorResponse != nil || first.Flags&uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState) != 0 {
		t.Fatalf("the first, full message: %v", first)
	}
	turnedAway := roundTrip(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, AgentDescription: description("1")})
	if turnedAway.GetErrorResponse().GetType() != protobufs.ServerErrorResponseType_ServerErrorResponseType_Unavailable {
		t.Fatalf("the second message is answered UNAVAILABLE: %v", turnedAway)
	}
	// A heartbeat, numbered on from the message turned away.
	next := roundTrip(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2})
	if next.Flags&uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState) == 0 {
		t.Fatalf("after a message turned away, the next is a gap and the server asks for the full state: %v", next)
	}
}
