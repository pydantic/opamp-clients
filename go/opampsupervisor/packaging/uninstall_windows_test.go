// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const windowsUninstallMocks = `
# Windows PowerShell initializes ProgramFiles at startup. Isolate it after startup.
$env:ProgramFiles = $env:TEST_PROGRAM_FILES
function New-Object {
 if ($args[0] -eq '-ComObject') {
  $object = [PSCustomObject]@{}
  $object | Add-Member ScriptMethod RelatedProducts {
   param($code)
   if ($code -ne '{A7C976D2-A6DB-4393-86BC-E59B0FB90C66}') { throw 'wrong upgrade code' }
   if ($env:TEST_PRODUCT -eq 'yes') { return '{11111111-1111-1111-1111-111111111111}' }
  }
  return $object
 }
 Microsoft.PowerShell.Utility\New-Object @args
}
function Get-Service {
 if ($env:TEST_SERVICE -eq 'failure') { throw 'service inspection failed' }
 if ($env:TEST_SERVICE -eq 'yes') {
  $object = [PSCustomObject]@{}
  $object | Add-Member ScriptMethod WaitForStatus { param($status, $timeout) }
  return $object
 }
}
function Stop-Service { Add-Content -LiteralPath $env:TEST_LOG -Value 'stop' }
function Start-Process {
 param([string]$FilePath, [string[]]$ArgumentList, [switch]$Wait, [switch]$PassThru)
 Add-Content -LiteralPath $env:TEST_LOG -Value "msi $FilePath $($ArgumentList -join ' ')"
 return [PSCustomObject]@{ExitCode = [int]$env:TEST_EXIT}
}
& $env:TEST_SCRIPT @TestArguments
`

func TestWindowsUninstallRetainsAndPurges(t *testing.T) {
	for _, purge := range []bool{false, true} {
		for _, exit := range []string{"0", "3010", "1603"} {
			t.Run(fmt.Sprintf("purge=%t/msi=%s", purge, exit), func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "pydantic-opamp")
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				token := filepath.Join(root, "token")
				if err := os.WriteFile(token, []byte("private-data"), 0600); err != nil {
					t.Fatal(err)
				}
				script, err := filepath.Abs("windows/pydantic-opamp-uninstall.ps1")
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
					"$TestArguments = @{"+map[bool]string{false: "", true: "Purge=$true"}[purge]+"}\n"+windowsUninstallMocks)
				cmd.Env = append(os.Environ(), "OPAMP_UNINSTALL_ROOT="+root, "TEST_PROGRAM_FILES="+filepath.Join(filepath.Dir(root), "programs"), "TEST_SCRIPT="+script,
					"TEST_LOG="+filepath.Join(root, "..", "calls"), "TEST_PRODUCT=yes", "TEST_SERVICE=yes", "TEST_EXIT="+exit)
				output, err := cmd.CombinedOutput()
				if (err != nil) != (exit == "1603") {
					t.Fatalf("%v: %s", err, output)
				}
				calls, readErr := os.ReadFile(filepath.Join(root, "..", "calls"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, expected := range []string{"stop", "msiexec.exe", "/x", "{11111111-1111-1111-1111-111111111111}", "/norestart"} {
					if !strings.Contains(string(calls), expected) {
						t.Fatalf("missing %s: %s", expected, calls)
					}
				}
				_, err = os.Stat(token)
				if purge && exit != "1603" {
					if !os.IsNotExist(err) {
						t.Fatalf("purge left token: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				recovery := filepath.Join(filepath.Dir(root), "programs", "pydantic-opamp", "uninstall.ps1")
				_, recoveryErr := os.Stat(recovery)
				if purge && exit != "1603" {
					if !os.IsNotExist(recoveryErr) {
						t.Fatalf("purge left recovery helper: %v", recoveryErr)
					}
				} else if recoveryErr != nil {
					t.Fatalf("recovery: %v; output: %s", recoveryErr, output)
				}
				if _, err := os.Stat(filepath.Join(root, "uninstall.ps1")); !os.IsNotExist(err) {
					t.Fatal("executable recovery code must not be stored with Collector data")
				}
				if !purge && exit == "0" {
					repeat := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
						"$TestArguments = @{}\n"+windowsUninstallMocks)
					repeat.Env = append(os.Environ(), "OPAMP_UNINSTALL_ROOT="+root,
						"TEST_PROGRAM_FILES="+filepath.Join(filepath.Dir(root), "programs"), "TEST_SCRIPT="+recovery,
						"TEST_LOG="+filepath.Join(filepath.Dir(root), "repeat-calls"), "TEST_PRODUCT=no", "TEST_SERVICE=no", "TEST_EXIT=0")
					if output, err := repeat.CombinedOutput(); err != nil {
						t.Fatalf("recovery removal: %v: %s", err, output)
					}
					if value, err := os.ReadFile(token); err != nil || string(value) != "private-data" {
						t.Fatal("repeat removal changed retained data")
					}
					purgeRecovery := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
						"$TestArguments = @{Purge=$true}\n"+windowsUninstallMocks)
					purgeRecovery.Env = repeat.Env
					if output, err := purgeRecovery.CombinedOutput(); err != nil {
						t.Fatalf("recovery purge: %v: %s", err, output)
					}
					for _, path := range []string{root, recovery} {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("recovery purge left %s: %v", path, err)
						}
					}
				}
				if strings.Contains(string(output), "private-data") {
					t.Fatal("credential leaked")
				}
			})
		}
	}
}

func TestWindowsUninstallRefusesServiceErrorsAndLeavesJunctionTargetsAlone(t *testing.T) {
	for _, mode := range []string{"failure", "unregistered", "dry-run", "junction"} {
		t.Run(mode, func(t *testing.T) {
			root, outside := filepath.Join(t.TempDir(), "pydantic-opamp"), t.TempDir()
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(root, "token")
			if err := os.WriteFile(token, []byte("private-data"), 0600); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(outside, "keep")
			if err := os.WriteFile(canary, []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
			script, err := filepath.Abs("windows/pydantic-opamp-uninstall.ps1")
			if err != nil {
				t.Fatal(err)
			}
			setup := "$TestArguments = @{Purge=$true}\n"
			if mode == "dry-run" {
				setup = "$TestArguments = @{Purge=$true;DryRun=$true}\n"
			}
			if mode == "junction" {
				setup += "New-Item -ItemType Junction -Path (Join-Path $env:OPAMP_UNINSTALL_ROOT 'link') -Target $env:TEST_OUTSIDE | Out-Null\n"
			}
			service, product := "yes", "yes"
			if mode == "failure" {
				service = "failure"
			}
			if mode == "unregistered" {
				product = "no"
			}
			cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", setup+windowsUninstallMocks)
			cmd.Env = append(os.Environ(), "OPAMP_UNINSTALL_ROOT="+root, "TEST_PROGRAM_FILES="+filepath.Join(filepath.Dir(root), "programs"), "TEST_SCRIPT="+script, "TEST_OUTSIDE="+outside,
				"TEST_LOG="+filepath.Join(root, "..", "calls"), "TEST_PRODUCT="+product, "TEST_SERVICE="+service, "TEST_EXIT=0")
			output, err := cmd.CombinedOutput()
			failing := mode == "failure" || mode == "unregistered"
			if (err != nil) != failing {
				t.Fatalf("%v: %s", err, output)
			}
			if _, err := os.Stat(canary); err != nil {
				t.Fatal("purge followed a junction: ", err)
			}
			if mode != "junction" {
				if _, err := os.Stat(token); err != nil {
					t.Fatal("data was removed: ", err)
				}
			}
		})
	}
}
