// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && windows

// Windows: the .msi OPAMP_MSI installed with msiexec, the Supervisor run by the Service Control
// Manager as LocalSystem, its state key protected by DPAPI.

package packaging

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

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
	// /norestart: a quiet install that wants a restart (a file in use) otherwise restarts the
	// machine at once, which takes the CI runner down with no word of why.
	out, err := timed(t, "", "msiexec", "/i", msi, "/qn", "/norestart", "/l*v", log)
	if err != nil {
		t.Fatalf("msiexec: %v: %s\n%s", err, out, notable(msiLog(log)))
	}
}

// notable is the lines of an msiexec log that say why it failed or wants a restart.
func notable(log string) string {
	var b strings.Builder
	for _, line := range strings.Split(log, "\n") {
		if regexp.MustCompile(`(?i)reboot|restart|in use|return value 3|error|1603|3010`).MatchString(line) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// msiLog is msiexec's verbose log as text (it writes UTF-16).
func msiLog(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		units := make([]uint16, (len(raw)-2)/2)
		for i := range units {
			units[i] = uint16(raw[2+2*i]) | uint16(raw[3+2*i])<<8
		}
		return string(utf16.Decode(units))
	}
	return string(raw)
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
		out, _ := timed(t, "", "powershell", "-NoProfile", "-Command",
			"Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='"+service+"'} -MaxEvents 50 | Sort-Object TimeCreated | Format-List TimeCreated, Message")
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

// diagnose reports what holds the service up: its state, the processes of the package (and any
// msiexec), the Supervisor's and the Service Control Manager's recent events, and the logs under
// the Supervisor's storage.
func diagnose() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := `
sc.exe queryex ` + service + `
Get-CimInstance Win32_Process | Where-Object { $_.Name -match 'opamp|otelcol|msiexec' } |
    Format-Table -AutoSize -Wrap ProcessId, ParentProcessId, Name, CommandLine | Out-String -Width 400
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='` + service + `'} -MaxEvents 200 -ErrorAction SilentlyContinue |
    Sort-Object TimeCreated | Format-List TimeCreated, Message | Out-String -Width 400
Get-WinEvent -FilterHashtable @{LogName='System'; ProviderName='Service Control Manager'} -MaxEvents 20 -ErrorAction SilentlyContinue |
    Sort-Object TimeCreated | Format-List TimeCreated, Message | Out-String -Width 400
Get-ChildItem -Recurse -File '` + programData + `' -ErrorAction SilentlyContinue | Format-Table -AutoSize Length, LastWriteTime, FullName | Out-String -Width 400
Get-ChildItem -Recurse -File -Filter *.log '` + programData + `' -ErrorAction SilentlyContinue | ForEach-Object {
    "==> $($_.FullName)"; Get-Content -Tail 200 $_.FullName }
`
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s\n(diagnose: %v)", out, err)
	}
	return string(out)
}
