// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
)

// StatusFile is where a running Supervisor says how it is, in its storage directory:
// `opampsupervisor status` reads it. It holds no credentials.
const StatusFile = "supervisor-status.json"

// Status is what the status file holds: the OpAMP connection, the collector's health and the
// last remote config, as the Supervisor reports them to its server.
type Status struct {
	PID         int       `json:"pid"`
	Started     time.Time `json:"started"`
	Updated     time.Time `json:"updated"`
	Stopped     bool      `json:"stopped,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	InstanceUID string    `json:"instance_uid,omitempty"`
	// Server is the connection to the OpAMP server: nil before the first try.
	Server *ServerStatus `json:"server,omitempty"`
	// Collector is its health as last reported: nil before the first report.
	Collector *CollectorStatus `json:"collector,omitempty"`
	// RemoteConfig is the last one's status: nil before the server sent one.
	RemoteConfig *RemoteConfigState `json:"remote_config,omitempty"`
	// Accepted is when the server first answered this Supervisor in this run: nil while it hasn't.
	// A server that takes the connection and then closes it unanswered, as one does for
	// credentials it refuses (an enrollment key used up or revoked, say), leaves it nil while the
	// connection reads connected.
	Accepted *AcceptedState `json:"accepted,omitempty"`
	// OwnTelemetry is what became of the server's last own telemetry settings: nil before it sent
	// any.
	OwnTelemetry *OwnTelemetryState `json:"own_telemetry,omitempty"`
	// SetAside are the files of the saved states set aside, the oldest first, as the status is read: the
	// credentials in them are ones the server refused (set_aside.go).
	SetAside []string `json:"set_aside,omitempty"`
}

// OwnTelemetryState is the outcome of the server's last own telemetry settings (where the
// collector sends its own metrics and logs), since Since: "APPLIED" (in the collector's config;
// whether it runs is the collector's health), "REFUSED" by agent::remote_config_policy, or
// "FAILED" otherwise, with why (its URLs without user info, query or fragment). It doesn't make
// the status unwell: the collector runs, without those settings.
type OwnTelemetryState struct {
	Status string    `json:"status"`
	Since  time.Time `json:"since"`
	Error  string    `json:"error,omitempty"`
}

// AcceptedState is when the server first answered the Supervisor in this run: the first
// ServerToAgent from it, not an error response, to reach the client (go/opamp's
// Client.Answered). A server answers at once a host whose credentials it takes; one that takes
// the connection and closes it unanswered doesn't. A server that over WebSocket sends nothing
// until it has something to say leaves it nil too. It is the Supervisor's own run: a restart
// starts without it, whatever was saved, and it isn't taken back in the run (credentials refused
// at a later connection with an HTTP error make the connection fail, which Server says).
type AcceptedState struct {
	Since time.Time `json:"since"`
}

type ServerStatus struct {
	Connected  bool      `json:"connected"`
	Since      time.Time `json:"since"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Error      string    `json:"error,omitempty"`
	// IssuedRefused is how many of the last tries in a row the server refused as not valid (HTTP 401) with
	// the credentials it issued this host.
	IssuedRefused int `json:"issued_refused,omitempty"`
}

type CollectorStatus struct {
	Healthy bool      `json:"healthy"`
	Since   time.Time `json:"since"`
	Error   string    `json:"error,omitempty"`
}

type RemoteConfigState struct {
	Hash   string    `json:"hash"`
	Status string    `json:"status"`
	Since  time.Time `json:"since"`
	Error  string    `json:"error,omitempty"`
}

// statusReport keeps the status file up to date as the client's status changes. Its methods do
// nothing on a nil statusReport.
type statusReport struct {
	path string
	mu   sync.Mutex
	st   Status
	now  func() time.Time
	// closed: the client stopped, and the file says so for good.
	closed bool
}

func newStatusReport(dir string) *statusReport {
	r := &statusReport{path: filepath.Join(dir, StatusFile), now: time.Now}
	r.st = Status{PID: os.Getpid(), Started: r.now()}
	return r
}

// update changes the status and writes it. A status that can't be written is logged: the
// Supervisor runs on without it.
func (r *statusReport) update(logger *zap.Logger, change func(*Status, time.Time)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	now := r.now()
	change(&r.st, now)
	r.st.Updated = now
	err := writeStatus(r.path, &r.st)
	// On Windows the file can't be replaced while a reader (`status --wait`) has it open: tried
	// again for a moment, since a change such as the server's first answer is written once.
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 10; i++ {
		time.Sleep(20 * time.Millisecond)
		err = writeStatus(r.path, &r.st)
	}
	if err != nil && logger != nil {
		logger.Debug("The status file wasn't written", zap.String("path", r.path), zap.Error(err))
	}
}

func (r *statusReport) started(logger *zap.Logger, endpoint string, uid [16]byte) {
	r.update(logger, func(st *Status, _ time.Time) {
		st.Endpoint, st.InstanceUID, st.Stopped = endpoint, uuid.UUID(uid).String(), false
	})
}

func (r *statusReport) instanceUID(logger *zap.Logger, uid [16]byte) {
	r.update(logger, func(st *Status, _ time.Time) { st.InstanceUID = uuid.UUID(uid).String() })
}

func (r *statusReport) connected(logger *zap.Logger) {
	r.update(logger, func(st *Status, now time.Time) {
		if st.Server == nil || !st.Server.Connected {
			st.Server = &ServerStatus{Connected: true, Since: now}
		}
	})
}

// connectFailed keeps the time of the first failure of a run, and the latest failure's detail.
func (r *statusReport) connectFailed(logger *zap.Logger, status int, detail string, issued bool) {
	r.update(logger, func(st *Status, now time.Time) {
		since, refused := now, 0
		if st.Server != nil && !st.Server.Connected {
			since, refused = st.Server.Since, st.Server.IssuedRefused
		}
		if refused++; status != http.StatusUnauthorized || !issued {
			refused = 0
		}
		st.Server = &ServerStatus{Since: since, HTTPStatus: status, Error: detail, IssuedRefused: refused}
	})
}

func (r *statusReport) health(logger *zap.Logger, h *protobufs.ComponentHealth) {
	r.update(logger, func(st *Status, now time.Time) {
		if st.Collector != nil && st.Collector.Healthy == h.GetHealthy() && st.Collector.Error == h.GetLastError() {
			return
		}
		st.Collector = &CollectorStatus{Healthy: h.GetHealthy(), Since: now, Error: h.GetLastError()}
	})
}

func (r *statusReport) remoteConfig(logger *zap.Logger, s *protobufs.RemoteConfigStatus) {
	r.update(logger, func(st *Status, now time.Time) {
		rc := &RemoteConfigState{Hash: hex.EncodeToString(s.GetLastRemoteConfigHash()),
			Status: remoteConfigStatusName(s.GetStatus()), Since: now, Error: s.GetErrorMessage()}
		if old := st.RemoteConfig; old != nil && old.Hash == rc.Hash && old.Status == rc.Status && old.Error == rc.Error {
			return
		}
		st.RemoteConfig = rc
	})
}

// accepted records the server's first answer of the run.
func (r *statusReport) accepted(logger *zap.Logger) {
	if r == nil {
		return
	}
	r.mu.Lock()
	done := r.st.Accepted != nil
	r.mu.Unlock()
	if done {
		return
	}
	r.update(logger, func(st *Status, now time.Time) {
		if st.Accepted == nil {
			st.Accepted = &AcceptedState{Since: now}
		}
	})
}

// ownTelemetry records the outcome of the server's own telemetry settings: refused with err, or
// applied. The time is that of the first of a run of the same outcome.
func (r *statusReport) ownTelemetry(logger *zap.Logger, err error) {
	r.update(logger, func(st *Status, now time.Time) {
		ot := &OwnTelemetryState{Status: ownTelemetryOutcome(err), Since: now}
		if err != nil {
			ot.Error = shownURLs(err.Error())
		}
		if old := st.OwnTelemetry; old != nil && old.Status == ot.Status && old.Error == ot.Error {
			return
		}
		st.OwnTelemetry = ot
	})
}

// stopped: the core's client stopped on its own.
func (r *statusReport) stopped(logger *zap.Logger) {
	r.update(logger, func(st *Status, _ time.Time) { st.Stopped = true })
}

// close: the Supervisor stopped the client. A notification still on its way changes nothing.
func (r *statusReport) close(logger *zap.Logger) {
	r.update(logger, func(st *Status, _ time.Time) { st.Stopped = true })
	if r != nil {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}
}

func remoteConfigStatusName(s protobufs.RemoteConfigStatuses) string {
	switch s {
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
		return "APPLIED"
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING:
		return "APPLYING"
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
		return "FAILED"
	}
	return "UNSET"
}

// writeStatus replaces the file whole (a reader never sees half of it), readable by the
// Supervisor's user alone, as the rest of the storage directory.
func writeStatus(path string, st *Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), StatusFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(b, '\n')); err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadStatus reads the status file in a storage directory.
func ReadStatus(dir string) (*Status, error) {
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", StatusFile, err)
	}
	st.SetAside = statesSetAside(dir)
	return &st, nil
}

// AcceptGrace is how long after connecting a server is given to answer before the status reads
// it as not having taken the Supervisor: a server answers an agent's first message at once.
const AcceptGrace = 10 * time.Second

// IsAccepted is whether the server has taken this Supervisor, as Accepted says.
func (st *Status) IsAccepted() bool { return st.Accepted != nil }

// Running is whether the Supervisor that wrote the status still runs: it didn't stop, and its
// process is there (as this machine sees it; another PID namespace, a container's say, can't).
func (st *Status) Running() bool {
	return !st.Stopped && processAlive(st.PID)
}

// OK is whether all is well: running, connected, the collector healthy, and the last remote
// config not FAILED.
func (st *Status) OK() bool {
	return st.Running() && st.Server != nil && st.Server.Connected &&
		st.Collector != nil && st.Collector.Healthy &&
		(st.RemoteConfig == nil || st.RemoteConfig.Status != "FAILED")
}

// Print writes the status for a person, now being the time to count ages from.
func (st *Status) Print(w io.Writer, now time.Time) {
	ago := func(t time.Time) string {
		return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.DateTime), now.Sub(t).Round(time.Second))
	}
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-14s %s\n", label, fmt.Sprintf(format, args...))
	}
	switch {
	case st.Stopped:
		line("Supervisor", "stopped, as of %s", ago(st.Updated))
	case !processAlive(st.PID):
		line("Supervisor", "not running: process %d isn't there; last update %s", st.PID, ago(st.Updated))
	default:
		line("Supervisor", "running (process %d) since %s", st.PID, ago(st.Started))
	}
	if st.InstanceUID != "" {
		line("Instance UID", "%s", st.InstanceUID)
	}
	switch s := st.Server; {
	case s == nil:
		line("OpAMP server", "%s: not tried yet", st.Endpoint)
	case s.Connected:
		line("OpAMP server", "%s: connected since %s", st.Endpoint, ago(s.Since))
	default:
		why := s.Error
		if why == "" {
			why = fmt.Sprintf("HTTP %d", s.HTTPStatus)
		}
		line("OpAMP server", "%s: can't reach it since %s", st.Endpoint, ago(s.Since))
		line("", "%s", why)
		line("", "%s", connectHint(s.HTTPStatus, false, s.Error))
	}
	switch c := st.Collector; {
	case c == nil:
		line("Collector", "no health reported yet")
	case c.Healthy:
		line("Collector", "healthy since %s", ago(c.Since))
	case c.Error == "" && st.RemoteConfig == nil:
		// A managed host's Collector is unhealthy, with no reason, until its first config arrives.
		line("Collector", "waiting for its first remote config since %s", ago(c.Since))
	case c.Error == "":
		line("Collector", "unhealthy since %s", ago(c.Since))
	default:
		line("Collector", "unhealthy since %s: %s", ago(c.Since), c.Error)
	}
	switch a := st.Accepted; {
	case a != nil:
		line("Accepted", "yes: the server answered this host at %s", ago(a.Since))
	case st.Server != nil && st.Server.Connected && now.Sub(st.Server.Since) < AcceptGrace:
		line("Accepted", "not yet: waiting for the server's first answer since it connected")
	default:
		line("Accepted", "not yet: the server hasn't answered this host since it started")
		if st.Server != nil && st.Server.Connected {
			line("", "A server that takes the connection but not the credentials (a used-up or revoked enrollment key, say) looks like this: check the host in the server's UI, and the key")
		}
	}
	switch ot := st.OwnTelemetry; {
	case ot == nil:
	case ot.Error != "":
		line("Own telemetry", "%s since %s: %s", ot.Status, ago(ot.Since), ot.Error)
	default:
		line("Own telemetry", "%s since %s", ot.Status, ago(ot.Since))
	}
	for _, path := range st.SetAside {
		line("Set aside", "credentials the server refused, in %s", path)
	}
	switch rc := st.RemoteConfig; {
	case rc == nil:
		line("Remote config", "none received yet")
	case rc.Error != "":
		line("Remote config", "%s since %s (hash %s): %s", rc.Status, ago(rc.Since), short(rc.Hash), rc.Error)
	default:
		line("Remote config", "%s since %s (hash %s)", rc.Status, ago(rc.Since), short(rc.Hash))
	}
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		// FindProcess opens the process there: it fails for one that isn't.
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
