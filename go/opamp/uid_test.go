// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// With no instance UID, the client leaves it to the core, with 32 bytes of the operating
// system's randomness and the wall clock.
func TestTheCoreConfig(t *testing.T) {
	a, err := coreConfig(Config{Endpoint: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := coreConfig(Config{Endpoint: "http://x"})
	if len(a.InstanceUid) != 0 {
		t.Fatalf("a UID of the client's own: %x", a.InstanceUid)
	}
	if len(a.Entropy) != 32 || bytes.Equal(a.Entropy, b.Entropy) {
		t.Fatalf("entropy %x, then %x", a.Entropy, b.Entropy)
	}
	// No clock seed: the core makes the backoff's jitter seed from the entropy, so two clients
	// started in the same tick don't retry in step.
	if a.Seed != 0 {
		t.Fatalf("a jitter seed of the client's own: %d", a.Seed)
	}
	if d := time.Now().UnixMilli() - int64(a.UnixMs); d < 0 || d > 5000 {
		t.Fatalf("unix_ms %d ms off", d)
	}
	c, _ := coreConfig(Config{Endpoint: "http://x", InstanceUID: [16]byte{1}, StopTimeout: time.Second, ConnectTimeout: 2 * time.Second})
	if c.InstanceUid[0] != 1 || c.StopTimeoutMs != 1000 || c.ConnectTimeoutMs != 2000 {
		t.Fatalf("got %v", c)
	}
}

// The UID in use is the core's: one it made (a UUIDv7) when the application chose none, else
// the application's; the server sees the same.
// covers: OPAMP-UID-01, OPAMP-UID-03
func TestTheUIDComesFromTheCore(t *testing.T) {
	for _, chosen := range [][16]byte{{}, {0x01, 0x92, 0x8f, 0x3a, 0x7c, 0x3e, 0x7a, 0x1b, 0x8c, 0x2d, 0x3e, 0x4f, 0x5a, 0x6b, 0x7c, 0x8d}} {
		srv := &server{}
		ts := httptest.NewServer(srv)
		c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: ts.URL, InstanceUID: chosen, HeartbeatInterval: time.Hour}, Status{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		uid := c.InstanceUID()
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		ts.Close()
		if chosen != ([16]byte{}) && uid != chosen {
			t.Fatalf("chose %x, in use %x", chosen, uid)
		}
		if uid[6]>>4 != 7 || uid[8]>>6 != 2 {
			t.Fatalf("not a UUIDv7: %x", uid)
		}
		if ms := int64(binary.BigEndian.Uint64(append([]byte{0, 0}, uid[:6]...))); chosen == ([16]byte{}) && time.Now().UnixMilli()-ms > 5000 {
			t.Fatalf("made %d ms ago", time.Now().UnixMilli()-ms)
		}
		srv.mu.Lock()
		if len(srv.msgs) == 0 || !bytes.Equal(srv.msgs[0].InstanceUid, uid[:]) {
			t.Fatalf("the server saw %v, in use %x", srv.msgs, uid)
		}
		srv.mu.Unlock()
	}
}

// RequestInstanceUID reaches the core: the first message asks the server for a UID, and without
// the option it doesn't.
// covers: OPAMP-UID-04
func TestRequestInstanceUIDSetsTheFlag(t *testing.T) {
	for _, on := range []bool{true, false} {
		srv := &server{}
		ts := httptest.NewServer(srv)
		c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: ts.URL, HeartbeatInterval: time.Hour,
			RequestInstanceUID: on}, Status{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		ts.Close()
		srv.mu.Lock()
		if len(srv.msgs) == 0 {
			t.Fatal("no messages")
		}
		asked := srv.msgs[0].Flags&uint64(protobufs.AgentToServerFlags_AgentToServerFlags_RequestInstanceUid) != 0
		srv.mu.Unlock()
		if asked != on {
			t.Fatalf("option %v: first message asks for a UID: %v", on, asked)
		}
	}
}

// covers: OPAMP-SIZE-02, OPAMP-SIZE-08
func TestSizeLimitsReachTheCore(t *testing.T) {
	c, _ := coreConfig(Config{Endpoint: "http://x", MaxSendBytes: 4096, MaxReceiveBytes: 8192})
	if c.MaxSendBytes != 4096 || c.MaxReceiveBytes != 8192 {
		t.Fatalf("send %d, receive %d", c.MaxSendBytes, c.MaxReceiveBytes)
	}
	c, _ = coreConfig(Config{Endpoint: "http://x"})
	if c.MaxSendBytes != 0 || c.MaxReceiveBytes != 0 {
		t.Fatalf("unset limits sent as send %d, receive %d, not 0 (the core's defaults)", c.MaxSendBytes, c.MaxReceiveBytes)
	}
}
