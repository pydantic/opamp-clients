// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !windows

package packaging

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A signal while the daemon is unloaded ends the helper once it is loaded again: ended at once, it left it
// unloaded until the Mac started again. The signal goes to the whole group, as a Ctrl-C's or a lost ssh's does.
func TestMacEnrollEndedWhileTheDaemonIsUnloaded(t *testing.T) {
	for _, at := range []string{"bootout", "print", "bootstrap"} {
		for signal, code := range map[syscall.Signal]int{syscall.SIGHUP: 129, syscall.SIGINT: 130, syscall.SIGTERM: 143} {
			for _, refresh := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/refresh=%v", at, signal, refresh), func(t *testing.T) {
					t.Parallel()
					root, tools := t.TempDir(), t.TempDir()
					args := []string{"--endpoint", "wss://opamp.example.com/v1/opamp"}
					out, err := macEnroll(t, root, tools, "t", args...)
					require.NoError(t, err, out)
					require.FileExists(t, filepath.Join(tools, "loaded"))
					if refresh {
						args = []string{"--refresh"}
					}
					require.NoError(t, os.WriteFile(filepath.Join(tools, "stopping"), []byte("1\n"), 0o600))
					stub := filepath.Join(tools, "launchctl")
					script, err := os.ReadFile(stub)
					require.NoError(t, err)
					wrapped := filepath.Join(tools, "wrapped")
					require.NoError(t, os.WriteFile(wrapped, script, 0o755))
					require.NoError(t, os.WriteFile(stub, []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = %s ] && [ ! -e "%s/sent" ]; then : >"%[2]s/sent"; kill -%d 0; sleep 0.3; fi
exec "%[2]s/wrapped" "$@"
`, at, tools, int(signal))), 0o755))
					cmd := exec.Command("./macos/pydantic-opamp-enroll", args...)
					cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+root, "LAUNCHCTL="+stub, "OPAMP_SUPERVISOR="+fakeSupervisor(t, tools), "LAUNCHCTL_STOPPING="+filepath.Join(tools, "stopping"))
					cmd.Stdin = strings.NewReader("t")
					cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					b, err := cmd.CombinedOutput()
					require.FileExists(t, filepath.Join(tools, "sent"), string(b))
					assert.FileExists(t, filepath.Join(tools, "loaded"), "left unloaded: %s", b)
					var exit *exec.ExitError
					require.ErrorAs(t, err, &exit, string(b))
					assert.Equal(t, code, exit.ExitCode(), string(b))
				})
			}
		}
	}
}
