// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// A shop that takes an order a second, for trying Logfire Fleet (see ../README.md).
//
// Each order is a span with an INFO log record inside it; every fifth order is slow and logs a
// WARN as well. Set the policy's minimum log level to Warning in Fleet and, in Live view, the
// INFO records stop while the warnings and spans keep coming.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"

	"github.com/pydantic/opamp-clients/go/opampotel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Builds the SDK the OTEL_* variables describe and connects it to Fleet (OTEL_OPAMP_*).
	sdk, err := opampotel.Setup(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(context.Background()) // tells Fleet goodbye, then flushes

	tracer := otel.Tracer("fleet-quickstart")
	logger := otelslog.NewLogger("fleet-quickstart")

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for order := 1; ; order++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		spanCtx, span := tracer.Start(ctx, fmt.Sprintf("checkout order %d", order))
		logger.InfoContext(spanCtx, fmt.Sprintf("order %d paid", order))
		if order%5 == 0 {
			logger.WarnContext(spanCtx, fmt.Sprintf("order %d took longer than 2 s", order))
		}
		span.End()
		slog.Info("sent", "order", order)
	}
}
