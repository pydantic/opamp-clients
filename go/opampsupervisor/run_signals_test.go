// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A start that finishes with no signal is not stopped, and its error is the caller's.
func TestStartUnlessInterruptedStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped, err := startUnlessInterrupted(ctx, cancel, func(context.Context) error { return nil }, make(chan os.Signal))
	require.NoError(t, err)
	assert.False(t, stopped)
	require.NoError(t, ctx.Err(), "the Supervisor runs on this context from here on")

	failed := errors.New("no collector")
	stopped, err = startUnlessInterrupted(ctx, cancel, func(context.Context) error { return failed }, make(chan os.Signal))
	assert.False(t, stopped)
	require.ErrorIs(t, err, failed)
}

// A signal while it starts cancels the start's context, and the start is waited for: what it
// started is stopped before the Supervisor exits.
func TestStartUnlessInterruptedStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupt := make(chan os.Signal, 1)
	waiting, unwound := make(chan struct{}), false
	start := func(ctx context.Context) error {
		close(waiting)
		<-ctx.Done()
		unwound = true
		return ctx.Err()
	}
	go func() {
		<-waiting
		interrupt <- syscall.SIGTERM
	}()
	stopped, err := startUnlessInterrupted(ctx, cancel, start, interrupt)
	assert.True(t, stopped)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, unwound, "it returned before the start had unwound")
}
