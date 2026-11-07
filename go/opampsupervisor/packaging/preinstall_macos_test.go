// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The real preinstall script must not let the package replace files while launchd still
// owns the service. Stand-ins make delayed unloads and failures deterministic, without root.
func TestMacPreinstallWaitsForUnload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the package script needs a POSIX shell")
	}
	for _, tt := range []struct {
		name, mode, want string
		ok               bool
	}{
		{"fresh install", "absent", "", true},
		{"delayed unload", "delayed", "", true},
		{"stop failure", "stop-error", "could not unload", false},
		{"initial inspection failure", "probe-error", "could not inspect", false},
		{"inspection failure after stop", "later-probe-error", "could not inspect", false},
		{"service never unloads", "stuck", "still loaded after 30 seconds", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("launchctl", `#!/bin/sh
echo "$*" >> "$FIXTURE/calls"
[ "$2" = system/dev.pydantic.opamp-supervisor ] || exit 99
case "$1" in
    bootout)
        [ "$MODE" != stop-error ] || exit 5
        touch "$FIXTURE/stopped"
        exit 0 ;;
    print)
        [ "$MODE" != absent ] || exit 113
        [ "$MODE" != probe-error ] || exit 5
        [ -f "$FIXTURE/stopped" ] || exit 0
        [ "$MODE" != later-probe-error ] || exit 5
        [ "$MODE" != delayed ] || [ ! -f "$FIXTURE/slept" ] || exit 113
        exit 0 ;;
esac
exit 99
`)
			write("sleep", "#!/bin/sh\necho sleep >> \"$FIXTURE/calls\"\ntouch \"$FIXTURE/slept\"\n")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "macos/scripts/preinstall")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "MODE="+tt.mode)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tt.ok || !strings.Contains(string(out), tt.want) {
				t.Fatalf("preinstall: %v, output %q; want success=%v and %q", err, out, tt.ok, tt.want)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			log := string(calls)
			if tt.mode == "delayed" {
				want := "print system/dev.pydantic.opamp-supervisor\nbootout system/dev.pydantic.opamp-supervisor\nprint system/dev.pydantic.opamp-supervisor\nsleep\nprint system/dev.pydantic.opamp-supervisor\n"
				if log != want {
					t.Fatalf("returned before confirmed unload: %s", log)
				}
			}
			if (tt.mode == "absent" || tt.mode == "probe-error") && strings.Contains(log, "bootout") {
				t.Fatalf("stopped a service not known to be loaded: %s", log)
			}
			if tt.mode == "stuck" && strings.Count(log, "sleep\n") != 30 {
				t.Fatalf("unload wait is not bounded to 30 polls: %s", log)
			}
		})
	}
}
