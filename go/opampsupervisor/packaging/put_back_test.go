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

// acceptedJSON is `status --json` for a Supervisor its server took: %s is its process ID.
const acceptedJSON = "{\n  \"pid\": PID,\n  \"server\": {\n    \"connected\": true\n  },\n  \"accepted\": {\n    \"since\": \"2026-11-21T12:00:00Z\"\n  }\n}\n"

// enrolledHost is a host its server takes, with the token "old" and a CA, and a service that runs: the server
// takes it with that token and the state it was issued. With another, dir/with is sourced as each wait starts,
// and dir/json is the status.
func enrolledHost(t *testing.T) (e enrollment, storage, etc string) {
	t.Helper()
	e = newEnrollment(t)
	storage, etc = filepath.Join(e.root, "var/lib/pydantic-opamp"), filepath.Join(e.root, "etc/pydantic-opamp")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	require.NoError(t, os.MkdirAll(etc, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(storage, "opamp-state"), []byte("issued to old"), 0o600))
	for name, body := range map[string]string{"enroll-token.cred": "SEALED:--name=opamp-enroll-token:old", "state-key.cred": "key",
		"supervisor.yaml": "old config", "enroll.options": "--endpoint\nold\n", "supervisor.yaml.sha256": "old sum\n", "ca.pem": "old CA"} {
		require.NoError(t, os.WriteFile(filepath.Join(etc, name), []byte(body), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "running"), []byte("100"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "pid"), []byte("100"), 0o600))
	require.NoError(t, os.WriteFile(e.systemctl, []byte(`#!/bin/sh
T="`+e.tools+`"
echo "$@" >>"$T/systemctl.log"
up() {
	pid=$(($(cat "$T/pid") + 1)); echo $pid >"$T/pid"
	[ -e "$T/fail-start" ] && ! grep -q old "`+etc+`/enroll-token.cred" && return 0
	echo $pid >"$T/running"
	[ ! -e "$T/started" ] || . "$T/started"
}
case $1 in
is-active) [ -e "$T/running" ] || exit 3 ;;
stop) [ -e "$T/stays" ] || rm -f "$T/running"; [ ! -e "$T/stopping" ] || . "$T/stopping" ;;
start) [ -e "$T/running" ] || up ;;
restart) up ;;
esac
exit 0
`), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "accepted"), []byte(acceptedJSON), 0o600))
	require.NoError(t, os.WriteFile(e.supervisor, []byte(`#!/bin/sh
T="`+e.tools+`"
echo "$@" >>"$T/supervisor.log"
old=; ! grep -q 'token:old$' "`+etc+`/enroll-token.cred" || [ "$(cat "`+storage+`/opamp-state")" != "issued to old" ] || [ -e "$T/old-gone" ] || old=yes
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
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, "json"), []byte(spentJSON), 0o600))
	return e, storage, etc
}

func (e enrollment) put(t *testing.T, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(e.tools, name), []byte(body), 0o600))
}

// assertOld is that everything of the old enrollment is there, whole, and nothing else of any.
func assertOld(t *testing.T, e enrollment, storage, etc string) {
	t.Helper()
	for name, body := range map[string]string{"enroll-token.cred": "SEALED:--name=opamp-enroll-token:old", "supervisor.yaml": "old config",
		"enroll.options": "--endpoint\nold\n", "supervisor.yaml.sha256": "old sum\n", "ca.pem": "old CA"} {
		assert.Equal(t, body, e.read(t, name), name)
	}
	assert.NoFileExists(t, filepath.Join(etc, "telemetry-token.cred"))
	assert.NoFileExists(t, filepath.Join(e.root, "etc/systemd/system/pydantic-opamp-supervisor.service.d/telemetry.conf"))
	b, err := os.ReadFile(filepath.Join(storage, "opamp-state"))
	require.NoError(t, err)
	assert.Equal(t, "issued to old", string(b))
	assert.Empty(t, copies(t, etc))
	left, err := os.ReadDir(storage)
	require.NoError(t, err)
	assert.Len(t, left, 1)
	assert.FileExists(t, filepath.Join(e.tools, "running"))
}

func copies(t *testing.T, etc string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(etc, "enrollment-before-*"))
	require.NoError(t, err)
	return names
}

// A host its server takes, enrolled with a token that doesn't work, however it doesn't, is back on the
// enrollment it had.
func TestEnrollPutsTheEnrollmentBack(t *testing.T) {
	state := `echo offered >"$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state"` + "\n"
	for name, tc := range map[string]struct {
		json, with, file, body, says string
		args                         []string
		stdin                        string
	}{
		"refused":               {json: spentJSON, says: "took the connection and not the enrollment token"},
		"refused, 0.10.4's":     {json: refusedJSON, says: "took the connection and not the enrollment token"},
		"unreachable":           {json: "{}", says: "not enrolled: the Supervisor can't connect to"},
		"unreachable offer":     {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"unreachable\": \"other.example.com\"", 1), with: state, says: "tells this host to connect to other.example.com, which can't be reached"},
		"offered and not taken": {json: strings.Replace(notTakenJSON, "\"identity_taken\": true", "\"x\": true", 1), with: state, says: "answered this host and hasn't taken it"},
		"identity taken":        {json: notTakenJSON, with: state, says: "holds this host's identity for another installation"},
		"does not start":        {json: "{}", file: "fail-start", says: "failed to start"},
		"terminated":            {json: spentJSON, with: state + `kill -TERM "$PPID"; sleep 1` + "\n"},
		"interrupted":           {json: spentJSON, with: state + `kill -INT "$PPID"; sleep 1` + "\n"},
		"hung up":               {json: spentJSON, with: `kill -HUP "$PPID"; sleep 1` + "\n"},
		"a broken pipe":         {json: spentJSON, with: `kill -PIPE "$PPID"; sleep 1` + "\n"},
		"a telemetry token":     {json: spentJSON, args: []string{"--telemetry-token-stdin"}, stdin: "new\nwrite\n", says: "not the enrollment token"},
		"terminated as it stops": {json: spentJSON, file: "stopping",
			body: `[ -e "$T/once" ] || { : >"$T/once"; kill -TERM "$PPID"; sleep 1; }` + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			e, storage, etc := enrolledHost(t)
			e.put(t, "json", tc.json)
			if tc.with != "" {
				e.put(t, "with", tc.with)
			}
			if tc.file != "" {
				e.put(t, tc.file, tc.body)
			}
			stdin := tc.stdin
			if stdin == "" {
				stdin = "new"
			}
			out, err := e.run(t, stdin, append([]string{"--endpoint", "wss://opamp.example.com/v1/opamp"}, tc.args...)...)
			require.Error(t, err, out)
			assert.Contains(t, out, tc.says)
			if name != "terminated as it stops" {
				assert.Contains(t, out, "not enrolled with this token: this host is back on the enrollment it had, and connected")
			}
			assert.NotContains(t, out, "purge")
			assert.NotContains(t, out, "keeps retrying")
			assertOld(t, e, storage, etc)
		})
	}
}

// One that works is kept, and no copy of the other is.
func TestEnrollThatWorksKeepsNoCopies(t *testing.T) {
	e, _, etc := enrolledHost(t)
	e.put(t, "taken", "")
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:new", e.read(t, "enroll-token.cred"))
	assert.Empty(t, copies(t, etc))
}

// The server takes the host after the wait was over, and before the Supervisor was stopped: it stays.
func TestEnrollTakenLate(t *testing.T) {
	e, storage, etc := enrolledHost(t)
	e.put(t, "with", `[ -e "$T/late" ] && exit 0; : >"$T/late"`+"\n")
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Enrolled with wss://opamp.example.com/v1/opamp, late")
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:new", e.read(t, "enroll-token.cred"))
	assert.Empty(t, copies(t, etc))
	assert.FileExists(t, filepath.Join(storage, "opamp-state"))
}

// A status the Supervisor before left isn't the new one's: a service that never started took nothing.
func TestEnrollThatDoesNotStartIsNotTakenLate(t *testing.T) {
	e, storage, etc := enrolledHost(t)
	e.put(t, "fail-start", "")
	e.put(t, "late", "")
	e.put(t, "pid", "100")
	require.NoError(t, os.WriteFile(e.systemctl, []byte(strings.Replace(readFile(t, e.systemctl), "echo $pid >\"$T/pid\"", ":", 1)), 0o755))
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	assert.Contains(t, out, "this host is back on the enrollment it had")
	assertOld(t, e, storage, etc)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// Neither is taken: both are said, and nothing is tried a third time.
func TestEnrollPutsBackOnceOnly(t *testing.T) {
	e, _, etc := enrolledHost(t)
	e.put(t, "with", `: >"$T/old-gone"`+"\n")
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	assert.Contains(t, out, "took the connection and not the enrollment token")
	assert.Contains(t, out, "not enrolled with this token, and with the enrollment it had, which is back, this host isn't accepted now either")
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:old", e.read(t, "enroll-token.cred"))
	assert.Empty(t, copies(t, etc))
	log := readFile(t, filepath.Join(e.tools, "supervisor.log"))
	assert.Equal(t, 2, strings.Count(log, "--wait 30s"), log)
}

// A Supervisor that can't be stopped is left alone, with the copies and how to put them back.
func TestEnrollThatCanNotStopTheSupervisor(t *testing.T) {
	e, storage, etc := enrolledHost(t)
	e.put(t, "stays", "")
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	assert.Contains(t, out, "can't be stopped: nothing is changed")
	assertOld(t, e, storage, etc)

	e, _, etc = enrolledHost(t)
	e.put(t, "with", `: >"$T/stays"`+"\n")
	out, err = e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	require.Len(t, copies(t, etc), 1)
	assert.Contains(t, out, "put it back with: sudo sh "+copies(t, etc)[0]+"/put-back")
}

// A run that was killed leaves the copies for a person: root's alone, whole, and the next run says how to put
// them back, and changes nothing of them.
func TestEnrollAfterOneThatWasKilled(t *testing.T) {
	e, storage, etc := enrolledHost(t)
	e.put(t, "with", `echo offered >"$OPAMP_ENROLL_ROOT/var/lib/pydantic-opamp/opamp-state"; kill -KILL "$PPID"; sleep 1`+"\n")
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	require.Len(t, copies(t, etc), 1)
	kept := copies(t, etc)[0]
	info, err := os.Stat(kept)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o077)
	e.put(t, "with", "")
	out, err = e.run(t, "other", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.Error(t, err, out)
	assert.Contains(t, out, "Put it back with: sudo sh "+kept+"/put-back")
	assert.NotContains(t, out, "remove its data", "a purge removes the copies")
	assert.Equal(t, []string{kept}, copies(t, etc))
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:other", e.read(t, "enroll-token.cred"), "a host that isn't taken is enrolled as before")
	_, err = runPutBack(e, kept)
	require.NoError(t, err)
	replaced, err := filepath.Glob(filepath.Join(storage, "opamp-state.replaced-*"))
	require.NoError(t, err)
	require.Len(t, replaced, 1)
	assert.Equal(t, "offered\n", readFile(t, replaced[0]))
	require.NoError(t, os.Remove(replaced[0]))
	assertOld(t, e, storage, etc)
}

// What a run that was killed while it copied left goes; another run's that still runs stays.
func TestEnrollRemovesUnfinishedCopies(t *testing.T) {
	e, _, etc := enrolledHost(t)
	e.put(t, "taken", "")
	dead := filepath.Join(etc, "enrollment-before-20261122T000000Z-999999.tmp")
	alive := filepath.Join(etc, fmt.Sprintf("enrollment-before-20261122T000000Z-%d.tmp", os.Getpid()))
	require.NoError(t, os.Mkdir(dead, 0o700))
	require.NoError(t, os.Mkdir(alive, 0o700))
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
	require.NoError(t, err, out)
	assert.Equal(t, []string{alive}, copies(t, etc))
}

// A host the server doesn't take has nothing to go back to.
func TestEnrollOfAHostThatIsNotTaken(t *testing.T) {
	for name, file := range map[string]string{"not taken": "old-gone", "stopped": "running"} {
		t.Run(name, func(t *testing.T) {
			e, _, etc := enrolledHost(t)
			if name == "stopped" {
				require.NoError(t, os.Remove(filepath.Join(e.tools, file)))
			} else {
				e.put(t, file, "")
			}
			out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp")
			require.Error(t, err, out)
			assert.NotContains(t, out, "is back")
			assert.Equal(t, "SEALED:--name=opamp-enroll-token:new", e.read(t, "enroll-token.cred"))
			assert.Empty(t, copies(t, etc))
		})
	}
	e, _, etc := enrolledHost(t)
	out, err := e.run(t, "new", "--endpoint", "wss://opamp.example.com/v1/opamp", "--no-start")
	require.NoError(t, err, out)
	assert.Equal(t, "SEALED:--name=opamp-enroll-token:new", e.read(t, "enroll-token.cred"))
	assert.Empty(t, copies(t, etc))
}

func runPutBack(e enrollment, kept string) (string, error) {
	out, err := exec.Command("/bin/sh", filepath.Join(kept, "put-back")).CombinedOutput()
	return string(out), err
}
