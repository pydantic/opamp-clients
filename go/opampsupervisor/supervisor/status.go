// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

type ServerStatus struct {
	Connected  bool      `json:"connected"`
	Since      time.Time `json:"since"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Error      string    `json:"error,omitempty"`
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
	if err := writeStatus(r.path, &r.st); err != nil && logger != nil {
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
func (r *statusReport) connectFailed(logger *zap.Logger, status int, detail string) {
	r.update(logger, func(st *Status, now time.Time) {
		since := now
		if st.Server != nil && !st.Server.Connected {
			since = st.Server.Since
		}
		st.Server = &ServerStatus{Since: since, HTTPStatus: status, Error: detail}
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
	return &st, nil
}

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
	switch rc := st.RemoteConfig; {
	case rc == nil:
		line("Remote config", "none received")
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
