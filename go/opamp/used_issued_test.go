// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"errors"
	"net/http"
	"testing"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// UsedIssued is whether a request or connection went out with other values of the configured headers.
func TestUsedIssued(t *testing.T) {
	header := func(kv ...string) (hs []*corepb.Header) {
		for i := 0; i < len(kv); i += 2 {
			hs = append(hs, &corepb.Header{Name: kv[i], Value: kv[i+1]})
		}
		return hs
	}
	for name, tc := range map[string]struct {
		configured http.Header
		sent       []*corepb.Header
		want       bool
	}{
		"the configured ones":        {http.Header{"Authorization": {"Bearer a"}}, header("Authorization", "Bearer a", "X", "y"), false},
		"in another case":            {http.Header{"authorization": {"Bearer a"}}, header("authorization", "Bearer a"), false},
		"issued ones":                {http.Header{"Authorization": {"Bearer a"}}, header("Authorization", "Bearer b"), true},
		"issued ones, another case":  {http.Header{"authorization": {"Bearer a"}}, header("AUTHORIZATION", "Bearer b"), true},
		"none of them":               {http.Header{"Authorization": {"Bearer a"}}, header("X", "y"), true},
		"none configured":            {nil, header("Authorization", "Bearer b"), false},
		"one of two, issued":         {http.Header{"A": {"1"}, "B": {"2"}}, header("A", "1", "B", "3"), true},
		"two values, the same order": {http.Header{"A": {"1", "2"}}, header("A", "1", "A", "2"), false},
	} {
		t.Run(name, func(t *testing.T) {
			x := newExecutor(&connection{})
			c := &Client{used: &x.used, usedIssued: &x.usedIssued}
			x.configure(tc.configured)
			if c.UsedIssued() {
				t.Fatal("before any outcome")
			}
			x.tried("ws://x", tc.sent)
			if got := c.UsedIssued(); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
	if (&Client{}).UsedIssued() {
		t.Fatal("in core mode")
	}
}

// SaveError is how the last save ended, and Saves says that one did.
func TestSaveError(t *testing.T) {
	x := newExecutor(&connection{})
	c := &Client{saveError: &x.saveError, saves: x.saves}
	if c.SaveError() != "" {
		t.Fatal("before any save")
	}
	x.saved(storeFailed(1, errors.New("no space left")))
	x.saved(storeFailed(2, errors.New("no space left")))
	<-c.Saves()
	if c.SaveError() != "no space left" {
		t.Fatal(c.SaveError())
	}
	x.saved(&corepb.Input{Kind: &corepb.Input_Stored{Stored: &corepb.Id{Id: 3}}})
	<-c.Saves()
	if c.SaveError() != "" {
		t.Fatal(c.SaveError())
	}
	select {
	case <-c.Saves():
		t.Fatal("nothing ended since")
	default:
	}
}
