// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: a signal to
// stop that arrives while the Supervisor starts stops it and the Collector it started, where it
// ended the Supervisor at once and left that Collector running (run_signals.go).

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.opentelemetry.io/collector/featuregate"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func runInteractive() error {
	configFlag := flag.String("config", "", "Path to a supervisor configuration file")
	featuregate.GlobalRegistry().RegisterFlags(flag.CommandLine)
	flag.Parse()

	cfg, err := config.Load(*configFlag)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	logger, err := telemetry.NewLogger(cfg.Telemetry.Logs)
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	supervisor, err := supervisor.NewSupervisor(ctx, logger.Named("supervisor"), cfg)
	if err != nil {
		return fmt.Errorf("failed to create supervisor: %w", err)
	}

	// Changed here: the signals are taken before Start, not after it, and one that arrives
	// while it starts stops the Supervisor and the Collector it started (run_signals.go).
	interrupt := make(chan os.Signal, 2)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	stopped, err := startUnlessInterrupted(ctx, cancel, supervisor.Start, interrupt)
	if stopped {
		// Said in the Supervisor's own log: a service manager's quick stop is no silent exit.
		logger.Info("Stopped while starting", zap.Error(err))
		if err == nil {
			supervisor.Shutdown()
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to start supervisor: %w", err)
	}

	<-interrupt
	supervisor.Shutdown()

	return nil
}
