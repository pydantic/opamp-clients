// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// winFakes stands in for the service's cmdlets, the Supervisor setting its state aside as it starts
// (supervisor/set_aside.go) among them; the helper runs in its session, as a function wins over a cmdlet.
const winFakes = `param([string]$Helper, [string]$Root, [string]$Wait)
$ErrorActionPreference = 'Stop'
$global:Var = Join-Path $Root 'var'
$global:Log = Join-Path $Root '..\svc.log'
$global:Fail = Join-Path $Root '..\fail-start'
function global:Get-Service { [CmdletBinding()]param([string]$Name) [pscustomobject]@{ Name = $Name; Status = 'Running' } }
function global:Set-Service { [CmdletBinding()]param([string]$Name, [string]$StartupType) }
function global:Start-Service { [CmdletBinding()]param([string]$Name) Add-Content -LiteralPath $global:Log -Value 'start' }
function global:Stop-Service { [CmdletBinding()]param([string]$Name, [switch]$Force) Add-Content -LiteralPath $global:Log -Value 'stop' }
function global:Restart-Service { [CmdletBinding()]param([string]$Name, [switch]$Force)
    Add-Content -LiteralPath $global:Log -Value 'restart'
    foreach ($r in @(Get-ChildItem -LiteralPath $global:Var -Directory -Filter 'opamp-state.set-aside-*')) {
        if (Test-Path -LiteralPath $global:Fail) { throw 'fake: it does not start' }
        [IO.Directory]::Delete($r.FullName)
        $by = $r.Name.Substring('opamp-state.set-aside-'.Length)
        Move-Item -LiteralPath (Join-Path $global:Var 'opamp-state') -Destination (Join-Path $global:Var "opamp-state.refused-20261122T000000Z-$by")
        [IO.File]::WriteAllText((Join-Path $global:Var 'opamp-state'), 'new')
        if ($global:Wait -eq 'unsaved') { [IO.File]::WriteAllText((Join-Path $env:OPAMP_ENROLL_ROOT '..\json'), '{ "save_error": "no space left" }') }
        if ($global:Wait -eq 'plant') { [IO.File]::WriteAllText((Join-Path $global:Var "opamp-state.refused-9x'; exit 7-$by"), '') }
    }
}
$global:Wait = $Wait
$env:OPAMP_ENROLL_ROOT = $Root
$env:OPAMP_SUPERVISOR = Join-Path $Root '..\sup.ps1'
'token' | Set-Content -LiteralPath (Join-Path $Root '..\token') -NoNewline
& $Helper -Endpoint wss://opamp.example.com/v1/opamp -TokenFile (Join-Path $Root '..\token')
exit $LASTEXITCODE
`

// winSup is 'opampsupervisor status': ..\json, and 1, until the state is a new one and ..\heal is there.
const winSup = `$d = Split-Path -Parent $MyInvocation.MyCommand.Path
Add-Content -LiteralPath (Join-Path $d 'sup.log') -Value ($args -join ' ')
if ((Test-Path (Join-Path $d 'heal')) -and [IO.File]::ReadAllText((Join-Path $d 'pydantic-opamp\var\opamp-state')) -eq 'new' -and $args -notcontains '--json') { exit 0 }
if ($args -contains '--json') { [IO.File]::ReadAllText((Join-Path $d 'json')) }
exit 1
`

// A host whose saved credentials the server refused: the Supervisor is asked to set them aside, once, and
// when the new try fails it is said where they are, and the command that puts them back.
func TestWinEnrollSetsAside(t *testing.T) {
	for name, tc := range map[string]struct {
		json, file, wait string
		asked, ok        bool
	}{
		"refused, then taken":      {json: revokedJSON, file: "heal", asked: true, ok: true},
		"refused, and again":       {json: revokedJSON, asked: true},
		"a name that isn't one":    {json: revokedJSON, wait: "plant", asked: true},
		"taken, and not saved":     {json: revokedJSON, file: "heal", wait: "unsaved", asked: true},
		"refused twice":            {json: strings.ReplaceAll(revokedJSON, ": 5", ": 2")},
		"another token refused":    {json: strings.ReplaceAll(revokedJSON, ",\n    \"issued_refused\": 5", "")},
		"the new start fails":      {json: revokedJSON, file: "fail-start"},
		"connected and unanswered": {json: refusedJSON},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "pydantic-opamp")
			storage := filepath.Join(root, "var")
			state := filepath.Join(storage, "opamp-state")
			require.NoError(t, os.MkdirAll(storage, 0o700))
			for path, content := range map[string]string{
				state: "sealed", filepath.Join(dir, "json"): tc.json, filepath.Join(dir, "sup.ps1"): winSup, filepath.Join(dir, "fakes.ps1"): winFakes,
			} {
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			if tc.file != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.file), nil, 0o600))
			}
			helper, err := filepath.Abs(filepath.Join("windows", "pydantic-opamp-enroll.ps1"))
			require.NoError(t, err)
			cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(dir, "fakes.ps1"), "-Helper", helper, "-Root", root, "-Wait", tc.wait)
			b, err := cmd.CombinedOutput()
			out := string(b)
			assert.Equal(t, tc.ok, err == nil, out)
			aside, err := filepath.Glob(state + ".refused-20261122*")
			require.NoError(t, err)
			requests, err := filepath.Glob(state + ".set-aside-*")
			require.NoError(t, err)
			assert.Empty(t, requests)
			if !tc.asked {
				assert.Empty(t, aside)
				kept, err := os.ReadFile(state)
				require.NoError(t, err)
				assert.Equal(t, "sealed", string(kept))
				assert.Equal(t, tc.file == "fail-start", strings.Contains(out, "the saved credentials are not set aside"), out)
				return
			}
			require.Len(t, aside, 1, out)
			kept, err := os.ReadFile(aside[0])
			require.NoError(t, err)
			assert.Equal(t, "sealed", string(kept))
			calls, err := os.ReadFile(filepath.Join(dir, "sup.log"))
			require.NoError(t, err)
			assert.Regexp(t, `(?m)^status .* --wait 1s --since \d+ --require-accepted --json\r?$`, string(calls))
			if tc.ok {
				return
			}
			assert.Equal(t, 1, strings.Count(out, "the server refused this host's saved credentials"), out)
			assert.Equal(t, tc.wait == "unsaved", strings.Contains(out, "could not be saved"), out)
			assert.NotContains(t, out, "exit 7")
			assert.NotContains(t, out, "-Purge")
			// The command, run: the old state is back, and what was there is set aside instead.
			_, command, ok := strings.Cut(strings.NewReplacer("\r", "", "\n", " ").Replace(out), "put them back with: ")
			require.True(t, ok, out)
			command, _, _ = strings.Cut(command, " } finally { Start-Service")
			command = regexp.MustCompile(`Stop-Service \S+ -ErrorAction Stop; `).ReplaceAllString(command, "") + " } finally { }"
			b, err = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
			require.NoError(t, err, "%s: %s", command, b)
			kept, err = os.ReadFile(state)
			require.NoError(t, err)
			assert.Equal(t, "sealed", string(kept))
			aside, err = filepath.Glob(state + ".refused-2*")
			require.NoError(t, err)
			require.Len(t, aside, 1)
			kept, err = os.ReadFile(aside[0])
			require.NoError(t, err)
			assert.Equal(t, "new", string(kept))
			// Enrolled again, with nothing to set aside: the one before is said.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "json"), []byte(strings.ReplaceAll(revokedJSON, ",\n    \"issued_refused\": 5", "")), 0o600))
			b, err = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(dir, "fakes.ps1"), "-Helper", helper, "-Root", root).CombinedOutput()
			require.Error(t, err, string(b))
			again := strings.NewReplacer("\r", "", "\n", " ").Replace(string(b))
			assert.Contains(t, again, "saved credentials the server refused in an earlier run, at 20")
			assert.Regexp(t, `at 20\d\d-\d\d-\d\d \d\d:\d\d:\d\d UTC`, again)
			assert.Contains(t, again, filepath.Base(aside[0]))
			// Run again: nothing changes.
			_, _ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
			kept, err = os.ReadFile(state)
			require.NoError(t, err)
			assert.Equal(t, "sealed", string(kept))
		})
	}
}
