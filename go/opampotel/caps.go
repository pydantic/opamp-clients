// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"math"
	"sync"
)

// bucket is a rate cap's token bucket, the algorithm every language runs
// (testdata/sdk-settings/caps.json): full at its first take; refilled at perSecond a second on a
// monotonic clock, up to max(perSecond, 1); a take of n gets the whole tokens there are, up to n.
// In float64, in the layer's order.
type bucket struct {
	perSecond float64
	mu        sync.Mutex // tokens, last, started
	tokens    float64
	last      int64
	started   bool
}

func newBucket(perSecond float64) *bucket {
	return &bucket{perSecond: perSecond, tokens: math.Max(perSecond, 1)}
}

// take takes up to n tokens at nowNs (a monotonic clock, in nanoseconds), and says how many it
// took. A clock that goes back refills nothing.
func (b *bucket) take(nowNs int64, n uint64) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		b.started, b.last = true, nowNs
	}
	elapsed := 0.0
	if nowNs > b.last {
		elapsed = float64(nowNs-b.last) / 1e9
		b.last = nowNs
	}
	// float64(...) keeps the product rounded on its own, as the layer's is: Go may otherwise fuse
	// a multiply and an add.
	b.tokens = math.Min(b.tokens+float64(elapsed*b.perSecond), math.Max(b.perSecond, 1))
	taken := math.Min(float64(n), math.Floor(b.tokens))
	b.tokens -= taken
	return uint64(taken)
}

// sameCap says whether a bucket is the cap per second (nil: none), so a setting re-applied keeps
// the bucket it has: only a change of n starts a full one.
func sameCap(b *bucket, perSecond *float64) bool {
	if b == nil || perSecond == nil {
		return b == nil && perSecond == nil
	}
	return b.perSecond == *perSecond
}

// capOf is a full bucket for the cap per second; nil: no cap.
func capOf(perSecond *float64) *bucket {
	if perSecond == nil {
		return nil
	}
	return newBucket(*perSecond)
}
