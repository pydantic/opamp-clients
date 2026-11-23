// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && windows

// The Windows upgrade runs with one of the package's files held open, as an antivirus or a log
// viewer holds one. A package up to 0.10.3 sets no MSIRESTARTMANAGERCONTROL, and removed first its
// removal shut that program down and the upgrade failed (1601).

package packaging

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdForUpgrade starts a process that keeps otelcol.exe open as a reader that shares everything,
// and returns the check, after the upgrade, that the upgrade didn't end it.
func holdForUpgrade(t *testing.T) func() {
	t.Helper()
	script := `$f = [IO.File]::Open($env:HELD, 'Open', 'Read', 'ReadWrite,Delete'); [Console]::Out.WriteLine('held'); while ($true) { Start-Sleep 1; [void]$f.Length }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "HELD="+filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "otelcol.exe"))
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "the holder didn't open otelcol.exe")
	require.Equal(t, "held", strings.TrimSpace(line))
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})
	return func() {
		select {
		case err := <-ended:
			ended <- err
			t.Errorf("the upgrade ended the program holding otelcol.exe open (%v)", err)
		default:
		}
	}
}

// installUpgrade installs OPAMP_MSI as a host upgrades: msiexec with /norestart and no property
// on its command line. With a file held open it may exit 3010: the held file is replaced at the
// next restart.
func installUpgrade(t *testing.T) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "msiexec.log")
	out, err := timed(t, "", "msiexec", "/i", os.Getenv("OPAMP_MSI"), "/qn", "/norestart", "/l*v", log)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3010 {
		t.Log("msiexec exited 3010: a file in use is replaced at the next restart")
		return
	}
	if err != nil {
		t.Fatalf("msiexec: %v: %s\n%s", err, out, notable(msiLog(log)))
	}
}
