// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
)

// Upstream's main took SIGINT and SIGTERM only once Start had returned. Start runs the Collector
// (to ask what it is) and waits for it, for as long as agent::bootstrap_timeout, and a signal in
// that time ended the Supervisor on the spot, by the Go runtime's default: nothing stopped the
// Collector, which stayed behind with no parent, and the next start ran a second one beside it.
// A service manager stops a service whenever it is told to, also seconds after it started it.

// startUnlessInterrupted runs start, and says whether a signal arrived before it returned. On a
// signal it cancels ctx, which start's waits watch, and waits for start to unwind (it stops the
// Collector it started). With stopped and a nil error, start had finished all the same: the
// caller shuts down what it started.
func startUnlessInterrupted(ctx context.Context, cancel context.CancelFunc, start func(context.Context) error, interrupt <-chan os.Signal) (stopped bool, err error) {
	done := make(chan error, 1)
	go func() { done <- start(ctx) }()
	select {
	case err := <-done:
		return false, err
	case <-interrupt:
		cancel()
		return true, <-done
	}
}
