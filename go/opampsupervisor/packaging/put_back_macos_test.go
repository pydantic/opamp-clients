// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// macHost is enrolledHost for macos/pydantic-opamp-enroll.
type macHost struct{ root, tools, storage, etc string }

func enrolledMac(t *testing.T) macHost {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the enroll script is for macOS")
	}
	h := macHost{root: t.TempDir(), tools: t.TempDir()}
	h.storage, h.etc = filepath.Join(h.root, "opt/pydantic-opamp/var"), filepath.Join(h.root, "opt/pydantic-opamp/etc")
	require.NoError(t, os.MkdirAll(h.storage, 0o700))
	require.NoError(t, os.MkdirAll(h.etc, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(h.storage, "opamp-state"), []byte("issued to old"), 0o600))
	for name, body := range map[string]string{"enroll-token": "old", "supervisor.yaml": "old config", "enroll.options": "--endpoint\nold\n", "supervisor.yaml.sha256": "old sum\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(h.etc, name), []byte(body), 0o600))
	}
	h.put(t, "loaded", "")
	h.put(t, "pid", "100")
	h.put(t, "json", spentJSON)
	h.put(t, "accepted", acceptedJSON)
	require.NoError(t, os.WriteFile(filepath.Join(h.tools, "launchctl"), []byte(`#!/bin/sh
T="`+h.tools+`"
echo "$@" >>"$T/launchctl.log"
case $1 in
bootout) [ -e "$T/stays" ] || rm -f "$T/loaded"; [ ! -e "$T/stopping" ] || . "$T/stopping" ;;
bootstrap)
	[ ! -e "$T/loaded" ] || exit 5
	: >"$T/loaded"
	[ -e "$T/fail-start" ] && ! grep -q old "`+h.etc+`/enroll-token" && exit 0
	echo $(($(cat "$T/pid") + 1)) >"$T/pid" ;;
print)
	[ -e "$T/loaded" ] || exit 113
	if [ -e "$T/fail-start" ] && ! grep -q old "`+h.etc+`/enroll-token"; then printf '\tstate = spawn scheduled\n\truns = 4\n'; else printf '\tstate = running\n\truns = 1\n'; fi ;;
esac
`), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.tools, "sleep"), []byte("#!/bin/sh\nexec /bin/sleep \"$([ \"$1\" = 1 ] && echo 0.02 || echo \"$1\")\"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.tools, "opampsupervisor"), []byte(`#!/bin/sh
T="`+h.tools+`"
echo "$@" >>"$T/supervisor.log"
old=; [ "$(cat "`+h.etc+`/enroll-token")" != old ] || [ "$(cat "`+h.storage+`/opamp-state")" != "issued to old" ] || [ -e "$T/old-gone" ] || old=yes
case " $* " in
*" --json "*)
	if [ -n "$old" ] || [ -e "$T/late" ] || [ -e "$T/taken" ]; then sed "s/PID/$(cat "$T/pid")/" "$T/accepted"; else cat "$T/json"; fi ;;
*" --wait 30s "*)
	[ -z "$old" ] || exit 0
	[ ! -e "$T/taken" ] || exit 0
	[ ! -e "$T/with" ] || . "$T/with"
	echo "OpAMP server   why"
	exit 1 ;;
esac
`), 0o755))
	return h
}

func (h macHost) put(t *testing.T, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(h.tools, name), []byte(body), 0o600))
}

func (h macHost) run(stdin string, args ...string) (string, error) {
	cmd := exec.Command("./macos/pydantic-opamp-enroll", append([]string{"--endpoint", "wss://opamp.example.com/v1/opamp"}, args...)...)
	cmd.Env = append(os.Environ(), "OPAMP_ENROLL_ROOT="+h.root, "LAUNCHCTL="+filepath.Join(h.tools, "launchctl"),
		"OPAMP_SUPERVISOR="+filepath.Join(h.tools, "opampsupervisor"), "PATH="+h.tools+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h macHost) assertOld(t *testing.T) {
	t.Helper()
	for name, body := range map[string]string{"enroll-token": "old", "supervisor.yaml": "old config", "enroll.options": "--endpoint\nold\n", "supervisor.yaml.sha256": "old sum\n"} {
		assert.Equal(t, body, readFile(t, filepath.Join(h.etc, name)), name)
	}
	assert.NoFileExists(t, filepath.Join(h.etc, "telemetry-token"))
	assert.Equal(t, "issued to old", readFile(t, filepath.Join(h.storage, "opamp-state")))
	assert.Empty(t, copies(t, h.etc))
	left, err := os.ReadDir(h.storage)
	require.NoError(t, err)
	assert.Len(t, left, 1)
	assert.FileExists(t, filepath.Join(h.tools, "loaded"))
}

// TestEnrollPutsTheEnrollmentBack, on macOS.
func TestMacEnrollPutsTheEnrollmentBack(t *testing.T) {
	state := `echo offered >"$OPAMP_ENROLL_ROOT/opt/pydantic-opamp/var/opamp-state"` + "\n"
	for name, tc := range map[string]struct{ json, with, file, says string }{
		"refused":                {json: spentJSON, says: "took the connection and not the enrollment token"},
		"unreachable":            {json: "{}", says: "not enrolled: the Supervisor can't connect to"},
		"unreachable offer":      {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"unreachable\": \"other.example.com\"", 1), with: state, says: "tells this host to connect to other.example.com, which can't be reached"},
		"offered and not taken":  {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"x\": true", 1), with: state, says: "answered this host and hasn't taken it"},
		"identity taken":         {json: notTakenJSON, with: state, says: "holds this host's identity for another installation"},
		"does not start":         {json: "{}", file: "fail-start", says: "failed to start"},
		"terminated":             {json: spentJSON, with: state + `kill -TERM "$PPID"; sleep 2` + "\n"},
		"interrupted":            {json: spentJSON, with: state + `kill -INT "$PPID"; sleep 2` + "\n"},
		"hung up":                {json: spentJSON, with: `kill -HUP "$PPID"; sleep 2` + "\n"},
		"terminated as it stops": {json: spentJSON, file: "stopping"},
	} {
		t.Run(name, func(t *testing.T) {
			h := enrolledMac(t)
			h.put(t, "json", tc.json)
			if tc.with != "" {
				h.put(t, "with", tc.with)
			}
			if tc.file != "" {
				h.put(t, tc.file, `[ -e "$T/once" ] || { : >"$T/once"; kill -TERM "$PPID"; sleep 2; }`+"\n")
			}
			out, err := h.run("new")
			require.Error(t, err, out)
			assert.Contains(t, out, tc.says)
			if tc.file != "stopping" {
				assert.Contains(t, out, "not enrolled with this token: this host is back on the enrollment it had, and connected")
			}
			assert.NotContains(t, out, "purge")
			assert.NotContains(t, out, "keeps retrying")
			h.assertOld(t)
		})
	}
}

func TestMacEnrollThatWorksKeepsNoCopies(t *testing.T) {
	h := enrolledMac(t)
	h.put(t, "taken", "")
	out, err := h.run("new")
	require.NoError(t, err, out)
	assert.Equal(t, "new", readFile(t, filepath.Join(h.etc, "enroll-token")))
	assert.Empty(t, copies(t, h.etc))
}

func TestMacEnrollTakenLate(t *testing.T) {
	h := enrolledMac(t)
	h.put(t, "with", `[ -e "$T/late" ] && exit 0; : >"$T/late"`+"\n")
	out, err := h.run("new")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Enrolled with wss://opamp.example.com/v1/opamp, late")
	assert.Equal(t, "new", readFile(t, filepath.Join(h.etc, "enroll-token")))
	assert.Empty(t, copies(t, h.etc))
}

func TestMacEnrollPutsBackOnceOnly(t *testing.T) {
	h := enrolledMac(t)
	h.put(t, "with", `: >"$T/old-gone"`+"\n")
	out, err := h.run("new")
	require.Error(t, err, out)
	assert.Contains(t, out, "with the enrollment it had, which is back, this host isn't accepted now either")
	assert.Equal(t, "old", readFile(t, filepath.Join(h.etc, "enroll-token")))
	assert.Empty(t, copies(t, h.etc))
	assert.Equal(t, 2, strings.Count(readFile(t, filepath.Join(h.tools, "supervisor.log")), "--wait 30s"))
}

func TestMacEnrollThatCanNotStopTheSupervisor(t *testing.T) {
	h := enrolledMac(t)
	h.put(t, "stays", "")
	out, err := h.run("new")
	require.Error(t, err, out)
	assert.Contains(t, out, "is still stopping after 30 seconds")
	h.assertOld(t)

	h = enrolledMac(t)
	h.put(t, "with", `: >"$T/stays"`+"\n")
	out, err = h.run("new")
	require.Error(t, err, out)
	require.Len(t, copies(t, h.etc), 1)
	assert.Contains(t, out, "put it back with: sudo sh "+copies(t, h.etc)[0]+"/put-back")
}

func TestMacEnrollAfterOneThatWasKilled(t *testing.T) {
	h := enrolledMac(t)
	h.put(t, "with", `echo offered >"$OPAMP_ENROLL_ROOT/opt/pydantic-opamp/var/opamp-state"; kill -KILL "$PPID"; sleep 2`+"\n")
	out, err := h.run("new")
	require.Error(t, err, out)
	require.Len(t, copies(t, h.etc), 1)
	kept := copies(t, h.etc)[0]
	info, err := os.Stat(kept)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o077)
	h.put(t, "with", "")
	out, err = h.run("other")
	require.Error(t, err, out)
	assert.Contains(t, out, "Put it back with: sudo sh "+kept+"/put-back")
	assert.NotContains(t, out, "remove its data", "a purge removes the copies")
	assert.Equal(t, []string{kept}, copies(t, h.etc))
	assert.Equal(t, "other", readFile(t, filepath.Join(h.etc, "enroll-token")))
	b, err := exec.Command("/bin/sh", filepath.Join(kept, "put-back")).CombinedOutput()
	require.NoError(t, err, string(b))
	replaced, err := filepath.Glob(filepath.Join(h.storage, "opamp-state.replaced-*"))
	require.NoError(t, err)
	require.Len(t, replaced, 1)
	require.NoError(t, os.Remove(replaced[0]))
	h.assertOld(t)
}
