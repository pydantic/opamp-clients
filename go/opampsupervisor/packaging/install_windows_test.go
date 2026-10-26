// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && windows

// Windows: the .msi OPAMP_MSI installed with msiexec, the Supervisor run by the Service Control
// Manager as LocalSystem, its state key protected by DPAPI.

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const service = "pydantic-opamp-supervisor"

var (
	programData   = filepath.Join(os.Getenv("ProgramData"), "pydantic-opamp")
	storage       = filepath.Join(programData, "var")
	installedDirs = []string{programData, filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp")}
)

func requireAdmin(t *testing.T) {
	require.True(t, windows.GetCurrentProcessToken().IsElevated(), "it installs a package: run it as an administrator")
}

func powershell(t *testing.T, stdin, script string, args ...string) {
	t.Helper()
	run(t, stdin, "powershell", append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}, args...)...)
}

func install(t *testing.T) {
	t.Helper()
	msi := os.Getenv("OPAMP_MSI")
	require.NotEmpty(t, msi, "OPAMP_MSI names the installer to run")
	log := filepath.Join(t.TempDir(), "msiexec.log")
	out, err := exec.Command("msiexec", "/i", msi, "/qn", "/l*v", log).CombinedOutput()
	if err != nil {
		text, _ := os.ReadFile(log)
		t.Fatalf("msiexec: %v: %s\n%s", err, out, text)
	}
}

func enroll(t *testing.T, endpoint string) {
	t.Helper()
	script := filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "pydantic-opamp-enroll.ps1")
	run(t, "enroll-token\n", "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Endpoint", endpoint, "-Plaintext", "-NoStart")
}

// startService starts the service; stopping it (Stop-Service waits for it) is the returned
// function, which logs what the Supervisor wrote to the Application event log.
func startService(t *testing.T) func() {
	t.Helper()
	powershell(t, "", "Start-Service "+service)
	return func() {
		powershell(t, "", "Stop-Service "+service)
		out, _ := exec.Command("powershell", "-NoProfile", "-Command",
			"Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='"+service+"'} -MaxEvents 50 | Sort-Object TimeCreated | Format-List TimeCreated, Message").CombinedOutput()
		t.Logf("Supervisor output:\n%s", out)
	}
}

func uninstall(t *testing.T, purge bool) {
	t.Helper()
	helper := filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "pydantic-opamp-uninstall.ps1")
	if _, err := os.Stat(helper); os.IsNotExist(err) {
		helper = filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "uninstall.ps1")
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", helper}
	if purge {
		args = append(args, "-Purge")
	}
	run(t, "", "powershell", args...)
}
