// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// winBackFakes stands in for the service's cmdlets: ..\running while the service runs, ..\pid its process ID.
const winBackFakes = `param([string]$Helper, [string]$Root, [string]$Token)
$ErrorActionPreference = 'Stop'
$global:D = Split-Path -Parent $Root
function global:Up { $n = [int][IO.File]::ReadAllText("$global:D\pid") + 1; [IO.File]::WriteAllText("$global:D\pid", "$n")
    if ((Test-Path "$global:D\fail-start") -and [IO.File]::ReadAllText("$global:D\pydantic-opamp\enroll-token") -ne 'old') { throw 'fake: it does not start' }
    [IO.File]::WriteAllText("$global:D\running", '') }
function global:Get-Service { [CmdletBinding()]param([string]$Name) [pscustomobject]@{ Name = $Name; Status = $(if (Test-Path "$global:D\running") { 'Running' } else { 'Stopped' }) } }
function global:Set-Service { [CmdletBinding()]param([string]$Name, [string]$StartupType) }
function global:Start-Service { [CmdletBinding()]param([string]$Name) if (-not (Test-Path "$global:D\running")) { Up } }
function global:Stop-Service { [CmdletBinding()]param([string]$Name, [switch]$Force) if (-not (Test-Path "$global:D\stays")) { Remove-Item "$global:D\running" -ErrorAction SilentlyContinue } }
function global:Restart-Service { [CmdletBinding()]param([string]$Name, [switch]$Force) Remove-Item "$global:D\running" -ErrorAction SilentlyContinue; Up }
$env:OPAMP_ENROLL_ROOT = $Root
$env:OPAMP_SUPERVISOR = "$global:D\sup.ps1"
if ($Helper) {
    $Token | Set-Content -LiteralPath "$global:D\token" -NoNewline
    & $Helper -Endpoint wss://opamp.example.com/v1/opamp -TokenFile "$global:D\token"
    exit $LASTEXITCODE
}
& (Get-ChildItem -LiteralPath $Root -Directory -Filter 'enrollment-before-*' | Select-Object -First 1 | ForEach-Object { Join-Path $_.FullName 'put-back.ps1' })
`

// winBackSup is 'opampsupervisor status': accepted with the old token and the state issued for it, or with
// ..\taken; otherwise ..\with runs as each wait starts, and ..\json is the status.
const winBackSup = `$d = Split-Path -Parent $MyInvocation.MyCommand.Path
Add-Content -LiteralPath (Join-Path $d 'sup.log') -Value ($args -join ' ')
$r = Join-Path $d 'pydantic-opamp'
$old = [IO.File]::ReadAllText("$r\enroll-token") -eq 'old' -and [IO.File]::ReadAllText("$r\var\opamp-state") -eq 'issued to old' -and -not (Test-Path "$d\old-gone")
if ($args -contains '--json') {
    if ($old -or (Test-Path "$d\taken") -or (Test-Path "$d\late")) { [IO.File]::ReadAllText("$d\accepted").Replace('PID', [IO.File]::ReadAllText("$d\pid")) } else { [IO.File]::ReadAllText("$d\json") }
    exit 1
}
if ($old -or (Test-Path "$d\taken")) { exit 0 }
if (Test-Path "$d\with") { . ([scriptblock]::Create([IO.File]::ReadAllText("$d\with"))) }
'OpAMP server   why'
exit 1
`

type winHost struct{ dir, root, state string }

func enrolledWin(t *testing.T) winHost {
	t.Helper()
	h := winHost{dir: t.TempDir()}
	h.root = filepath.Join(h.dir, "pydantic-opamp")
	h.state = filepath.Join(h.root, "var", "opamp-state")
	require.NoError(t, os.MkdirAll(filepath.Dir(h.state), 0o700))
	for path, body := range map[string]string{
		h.state: "issued to old", filepath.Join(h.root, "enroll-token"): "old", filepath.Join(h.root, "supervisor.yaml"): "old config",
		filepath.Join(h.root, "enroll.options"): "{}", filepath.Join(h.root, "supervisor.yaml.sha256"): "old sum",
		filepath.Join(h.dir, "fakes.ps1"): winBackFakes, filepath.Join(h.dir, "sup.ps1"): winBackSup, filepath.Join(h.dir, "accepted"): acceptedJSON,
		filepath.Join(h.dir, "json"): spentJSON, filepath.Join(h.dir, "pid"): "100", filepath.Join(h.dir, "running"): "",
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	return h
}

func (h winHost) put(t *testing.T, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, name), []byte(body), 0o600))
}

func (h winHost) run(t *testing.T, token string) (string, error) {
	t.Helper()
	helper, err := filepath.Abs(filepath.Join("windows", "pydantic-opamp-enroll.ps1"))
	require.NoError(t, err)
	b, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(h.dir, "fakes.ps1"), "-Helper", helper, "-Root", h.root, "-Token", token).CombinedOutput()
	return strings.NewReplacer("\r", "", "\n", " ").Replace(string(b)), err
}

func (h winHost) assertOld(t *testing.T) {
	t.Helper()
	for name, body := range map[string]string{"enroll-token": "old", "supervisor.yaml": "old config", "enroll.options": "{}", "supervisor.yaml.sha256": "old sum"} {
		assert.Equal(t, body, readFile(t, filepath.Join(h.root, name)), name)
	}
	assert.Equal(t, "issued to old", readFile(t, h.state))
	assert.Empty(t, copies(t, h.root))
	left, err := os.ReadDir(filepath.Dir(h.state))
	require.NoError(t, err)
	assert.Len(t, left, 1)
	assert.FileExists(t, filepath.Join(h.dir, "running"))
}

// TestEnrollPutsTheEnrollmentBack, on Windows.
func TestWinEnrollPutsTheEnrollmentBack(t *testing.T) {
	state := `[IO.File]::WriteAllText("$r\var\opamp-state", 'offered')`
	for name, tc := range map[string]struct{ json, with, file, says string }{
		"refused":               {json: spentJSON, says: "took the connection and not the enrollment token"},
		"unreachable":           {json: "{}", says: "not enrolled: the Supervisor can't connect to"},
		"unreachable offer":     {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"unreachable\": \"other.example.com\"", 1), with: state, says: "tells this host to connect to other.example.com, which can't be reached"},
		"offered and not taken": {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"x\": true", 1), with: state, says: "answered this host and hasn't taken it"},
		"identity taken":        {json: notTakenJSON, with: state, says: "holds this host's identity for another installation"},
		"does not start":        {json: "{}", file: "fail-start", says: "failed to start"},
		"an error":              {json: spentJSON, with: state + `; throw 'fake: an error of its own'`, says: "fake: an error of its own"},
	} {
		t.Run(name, func(t *testing.T) {
			h := enrolledWin(t)
			h.put(t, "json", tc.json)
			if tc.with != "" {
				h.put(t, "with", tc.with)
			}
			if tc.file != "" {
				h.put(t, tc.file, "")
			}
			out, err := h.run(t, "new")
			require.Error(t, err, out)
			assert.Contains(t, out, tc.says)
			assert.Contains(t, out, "not enrolled with this token: this host is back on the enrollment it had, and connected")
			assert.NotContains(t, out, "-Purge")
			assert.NotContains(t, out, "keeps retrying")
			h.assertOld(t)
		})
	}
}

func TestWinEnrollThatWorksKeepsNoCopies(t *testing.T) {
	h := enrolledWin(t)
	h.put(t, "taken", "")
	out, err := h.run(t, "new")
	require.NoError(t, err, out)
	assert.Equal(t, "new", readFile(t, filepath.Join(h.root, "enroll-token")))
	assert.Empty(t, copies(t, h.root))
}

func TestWinEnrollTakenLate(t *testing.T) {
	h := enrolledWin(t)
	h.put(t, "with", `if (Test-Path "$d\late") { exit 0 }; [IO.File]::WriteAllText("$d\late", '')`)
	out, err := h.run(t, "new")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Enrolled with wss://opamp.example.com/v1/opamp, late")
	assert.Equal(t, "new", readFile(t, filepath.Join(h.root, "enroll-token")))
	assert.Empty(t, copies(t, h.root))
}

func TestWinEnrollPutsBackOnceOnly(t *testing.T) {
	h := enrolledWin(t)
	h.put(t, "with", `[IO.File]::WriteAllText("$d\old-gone", '')`)
	out, err := h.run(t, "new")
	require.Error(t, err, out)
	assert.Contains(t, out, "with the enrollment it had, which is back, this host isn't accepted now either")
	assert.Equal(t, "old", readFile(t, filepath.Join(h.root, "enroll-token")))
	assert.Empty(t, copies(t, h.root))
	assert.Equal(t, 2, strings.Count(readFile(t, filepath.Join(h.dir, "sup.log")), "--wait 30s"))
}

func TestWinEnrollThatCanNotStopTheSupervisor(t *testing.T) {
	h := enrolledWin(t)
	h.put(t, "stays", "")
	out, err := h.run(t, "new")
	require.Error(t, err, out)
	assert.Contains(t, out, "can't be stopped: nothing is changed")
	h.assertOld(t)

	h = enrolledWin(t)
	h.put(t, "with", `[IO.File]::WriteAllText("$d\stays", '')`)
	out, err = h.run(t, "new")
	require.Error(t, err, out)
	require.Len(t, copies(t, h.root), 1)
	assert.Contains(t, out, "put it back with: powershell")
}

func TestWinEnrollAfterOneThatWasKilled(t *testing.T) {
	h := enrolledWin(t)
	h.put(t, "with", `[IO.File]::WriteAllText("$r\var\opamp-state", 'offered'); Stop-Process -Id $PID -Force`)
	out, err := h.run(t, "new")
	require.Error(t, err, out)
	require.Len(t, copies(t, h.root), 1)
	kept := copies(t, h.root)[0]
	acl, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-Acl -LiteralPath '"+filepath.Join(kept, "enroll-token")+"').Access | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value }").CombinedOutput()
	require.NoError(t, err, string(acl))
	assert.ElementsMatch(t, []string{"S-1-5-18", "S-1-5-32-544"}, strings.Fields(string(acl)))
	h.put(t, "with", "")
	out, err = h.run(t, "other")
	require.Error(t, err, out)
	assert.Contains(t, out, "an earlier pydantic-opamp-enroll was cut short")
	assert.Equal(t, []string{kept}, copies(t, h.root))
	assert.Equal(t, "other", readFile(t, filepath.Join(h.root, "enroll-token")))
	b, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(h.dir, "fakes.ps1"), "-Root", h.root).CombinedOutput()
	require.NoError(t, err, string(b))
	replaced, err := filepath.Glob(h.state + ".replaced-*")
	require.NoError(t, err)
	require.Len(t, replaced, 1)
	require.NoError(t, os.Remove(replaced[0]))
	h.assertOld(t)
}
