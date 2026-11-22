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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// However a run ends after it had the saved credentials set aside (cut short, with nobody reading what it
// prints, beside another run), they are there, whole, and the service is left started.
func TestEnrollLosesNoCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		wait  string // what happens in the wait after they were set aside
		start string // how the helper is run: "$@"
	}{
		"refused":                  {},
		"terminated":               {wait: `kill -TERM "$PPID"; sleep 1`},
		"interrupted":              {wait: `kill -INT "$PPID"; sleep 1`},
		"hung up":                  {wait: `kill -HUP "$PPID"; sleep 1`},
		"a broken pipe":            {wait: `kill -PIPE "$PPID"; sleep 1`},
		"quit":                     {wait: `kill -QUIT "$PPID"; sleep 1`},
		"killed":                   {wait: `kill -KILL "$PPID"; sleep 1`},
		"hung up, its output gone": {wait: `kill -HUP "$PPID"; sleep 1`, start: `"$@" 2>&1 | :`},
		"its output closed":        {start: `"$@" >&- 2>&-`},
		"its errors closed":        {start: `"$@" 2>&-`},
		"nobody reads it":          {start: `"$@" 2>&1 | :`},
		"nobody reads its errors":  {start: `{ "$@" 2>&1 >/dev/null; } | :`},
		"nobody reads its end":     {start: `"$@" 2>&1 | head -n 1`},
		"a name that isn't one":    {wait: `: >"$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state.refused-9\$(touch PWNED)-$PPID"; ln -s x "$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state.refused-99999999T000000Z-$PPID"`},
		"another run restarts it":  {wait: `"$SYSTEMCTL" restart pydantic-opamp-supervisor.service`},
		"another run asks too":     {wait: `mkdir "$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state.set-aside-1"; "$SYSTEMCTL" restart pydantic-opamp-supervisor.service`},
	} {
		for _, shell := range []string{"/bin/sh", "/bin/dash"} {
			t.Run(name+" "+shell, func(t *testing.T) {
				e := newEnrollment(t)
				if _, err := os.Stat(shell); err != nil {
					t.Skip(err)
				}
				storage := filepath.Join(e.root, "var/lib/pydantic-opamp")
				require.NoError(t, os.MkdirAll(storage, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(storage, "opamp-state"), []byte("sealed"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(e.tools, "json"), []byte(revokedJSON), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(e.tools, "status"), []byte("1"), 0o600))
				if tc.wait != "" {
					require.NoError(t, os.WriteFile(filepath.Join(e.tools, "wait"), []byte(tc.wait+"\n"), 0o600))
				}
				start := tc.start
				if start == "" {
					start = `"$@"`
				}
				cmd := exec.Command("/bin/sh", "-c", start, "sh", shell, "./pydantic-opamp-enroll", "--endpoint", "wss://opamp.example.com/v1/opamp")
				cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+e.root, "SYSTEMD_CREDS="+e.creds, "SYSTEMCTL="+e.systemctl, "OPAMP_SUPERVISOR="+e.supervisor)
				cmd.Stdin = strings.NewReader("bootstrap-token")
				out, _ := cmd.CombinedOutput()
				// Closed from the start, its output stops the helper before it changes anything.
				found := ""
				paths, err := filepath.Glob(filepath.Join(storage, "opamp-state*"))
				require.NoError(t, err)
				for _, path := range paths {
					if b, err := os.ReadFile(path); err == nil && string(b) == "sealed" {
						found = path
					}
				}
				assert.NotEmpty(t, found, "%v: %s", paths, out)
				log, err := os.ReadFile(filepath.Join(e.tools, "systemctl.log"))
				require.NoError(t, err)
				assert.NotContains(t, string(log), "stop pydantic-opamp-supervisor.service")
			})
		}
	}
}

// A 401 is this start's only if the status is: one left by a run before it says nothing.
func TestEnrollSetsAsideOnlyForThisStart(t *testing.T) {
	e := newEnrollment(t)
	storage := filepath.Join(e.root, "var/lib/pydantic-opamp")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(storage, "opamp-state"), []byte("sealed"), 0o600))
	require.NoError(t, os.WriteFile(e.supervisor, []byte("#!/bin/sh\necho \"$@\" >>'"+filepath.Join(e.tools, "supervisor.log")+`'
case " $* " in *" --since "*) exit 1 ;; esac
printf '{\n  "stopped": true,\n  "server": {\n    "http_status": 401\n  }\n}\n'
`), 0o755))
	out, err := e.run(t, "bootstrap-token", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err)
	assert.NotContains(t, out, "the server refused this host's saved credentials")
	calls, err := os.ReadFile(filepath.Join(e.tools, "supervisor.log"))
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^status .* --wait 1s --since \d+ --require-accepted --json$`, string(calls))
}

// What is printed names the copy of this run, also beside another's made meanwhile, and no name that isn't
// the Supervisor's.
func TestEnrollNamesItsOwnCopy(t *testing.T) {
	e := newEnrollment(t)
	storage := filepath.Join(e.root, "var/lib/pydantic-opamp")
	state := filepath.Join(storage, "opamp-state")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	require.NoError(t, os.WriteFile(state, []byte("sealed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "json"), []byte(revokedJSON), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "status"), []byte("1"), 0o600))
	require.NoError(t, os.WriteFile(state+".refused-29991122T000000Z-1", []byte("an older run's, by a clock ahead"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "wait"), []byte(`S=$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state
: >"$S.refused-29991122T000001Z-2"
: >"$S.refused-9\$(touch PWNED)-$PPID"
[ -L "$S.refused-29991122T000002Z-$PPID" ] || ln -s x "$S.refused-29991122T000002Z-$PPID"
`), 0o600))
	cmd := exec.Command("./pydantic-opamp-enroll", "--endpoint", "wss://opamp.example.com/v1/opamp")
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+e.root, "SYSTEMD_CREDS="+e.creds, "SYSTEMCTL="+e.systemctl, "OPAMP_SUPERVISOR="+e.supervisor)
	cmd.Stdin = strings.NewReader("bootstrap-token")
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	_, named, ok := strings.Cut(string(out), "are set aside in ")
	require.True(t, ok, string(out))
	named, _, _ = strings.Cut(named, ",")
	assert.True(t, strings.HasSuffix(named, fmt.Sprintf("-%d", cmd.Process.Pid)), named)
	kept, err := os.ReadFile(named)
	require.NoError(t, err)
	assert.Equal(t, "sealed", string(kept))
	assert.NotContains(t, string(out), "PWNED")
}
