// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// `opampsupervisor status --config supervisor.yaml` (or --storage DIR) says how the Supervisor running on that
// config is: its OpAMP connection (and what to check when it can't connect), the collector's
// health and the last remote config's status, from the status file it keeps in its storage
// directory. It exits 0 when all is well, 1 when something isn't, and 2 when there is no status
// to read.
//
// It is a new file rather than a change to upstream's main.go: the command is taken here, before
// main runs.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(statusCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
}

func statusCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("opampsupervisor status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "Path to the Supervisor's configuration file")
	storage := flags.String("storage", "", "The Supervisor's storage directory, in place of --config")
	jsonOut := flags.Bool("json", false, "Print the status as JSON, as the status file holds it")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	dir := *storage
	if dir == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			// A config can read files only the running service can (a systemd credential, say).
			fmt.Fprintf(stderr, "failed to load config: %v\n(--storage DIR reads the status without it)\n", err)
			return 2
		}
		dir = cfg.Storage.Directory
	}
	st, err := supervisor.ReadStatus(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stderr, "no status in %s: the Supervisor hasn't run on this config yet\n", dir)
		return 2
	case errors.Is(err, fs.ErrPermission):
		fmt.Fprintf(stderr, "can't read the status in %s: run this as the Supervisor's user, or as root or an administrator\n", dir)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "can't read the status in %s: %v\n", dir, err)
		return 2
	}
	if *jsonOut {
		// The status read once: what is printed is what the exit code says.
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "can't print the status: %v\n", err)
			return 2
		}
		_, _ = stdout.Write(append(b, '\n'))
	} else {
		st.Print(stdout, time.Now())
	}
	if !st.OK() {
		return 1
	}
	return 0
}
