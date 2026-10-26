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
// With --wait DURATION it waits, for at most that long, for a Supervisor started at or after --since
// (Unix seconds) to connect, and then says how it is: 0 once it is connected, 1 when it isn't by
// then. The enrollment helpers run it after they start the service, so an install that can't
// reach its server (an untrusted certificate, say) says so instead of succeeding.
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
	wait := flags.Duration("wait", 0, "Wait this long for the Supervisor to connect to its OpAMP server")
	since := flags.Int64("since", 0, "With --wait: only a Supervisor started at or after this Unix time counts")
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
	if *wait > 0 {
		return waitConnected(dir, *wait, time.Unix(*since, 0), *jsonOut, stdout, stderr)
	}
	st, err := supervisor.ReadStatus(dir)
	if err != nil {
		return readFailed(dir, err, stderr)
	}
	if code := printStatus(st, *jsonOut, stdout, stderr); code != 0 {
		return code
	}
	if !st.OK() {
		return 1
	}
	return 0
}

// readFailed says why the status in dir can't be read, and returns the exit code for it (2).
func readFailed(dir string, err error, stderr io.Writer) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stderr, "no status in %s: the Supervisor hasn't run on this config yet\n", dir)
	case errors.Is(err, fs.ErrPermission):
		fmt.Fprintf(stderr, "can't read the status in %s: run this as the Supervisor's user, or as root or an administrator\n", dir)
	default:
		fmt.Fprintf(stderr, "can't read the status in %s: %v\n", dir, err)
	}
	return 2
}

// printStatus prints the status read once, as text or JSON, so what is printed is what the exit
// code says; it returns 2 when the JSON can't be written.
func printStatus(st *supervisor.Status, jsonOut bool, stdout, stderr io.Writer) int {
	if !jsonOut {
		st.Print(stdout, time.Now())
		return 0
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "can't print the status: %v\n", err)
		return 2
	}
	_, _ = stdout.Write(append(b, '\n'))
	return 0
}

// waitConnected polls the status file until a Supervisor started at or after since is connected,
// or wait is up; it then prints the status as the plain command does. A status file that exists
// but can't be read (permissions, say) fails at once, since waiting won't change it.
func waitConnected(dir string, wait time.Duration, since time.Time, jsonOut bool, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(wait)
	for {
		st, err := supervisor.ReadStatus(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return readFailed(dir, err, stderr)
		}
		current := err == nil && !st.Started.Before(since) && st.Running()
		connected := current && st.Server != nil && st.Server.Connected
		late := time.Now().After(deadline)
		switch {
		case connected:
			return printStatus(st, jsonOut, stdout, stderr)
		case late && !current:
			fmt.Fprintf(stderr, "the Supervisor hasn't reported its status in %s within %s: check its log\n", dir, wait)
			return 1
		case late:
			if code := printStatus(st, jsonOut, stdout, stderr); code != 0 {
				return code
			}
			return 1
		}
		time.Sleep(250 * time.Millisecond)
	}
}
