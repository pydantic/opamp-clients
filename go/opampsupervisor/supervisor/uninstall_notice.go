// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// An uninstall tells the server, so it can tell an uninstalled host from one that is down: the
// package's uninstaller makes the directory uninstallRequest plus its process ID (and, on Linux, when
// that process started) in the storage directory before it stops the service, and the Supervisor, stopping, sends uninstallPath at its server's origin with the
// credentials it connects with (those the server issued it, or, before it issued any, the
// enrollment token in its config: the server refuses that one), then writes what came of it to
// uninstallNoticeFile, for the uninstaller to say. The credentials never leave the Supervisor:
// the uninstaller can't read them (they are sealed in the client's state), and needs nothing but
// the two files. The request is bounded by uninstallNoticeTimeout, runs while the Supervisor stops
// its collector (so it adds little to a stop that has a limit: the service manager's, or the
// uninstaller's own wait), and the stop goes on whatever came of it.
const (
	uninstallRequest       = "uninstall-requested-"
	uninstallNoticeFile    = "uninstall-notice"
	uninstallPath          = "/v1/fleet/installations/self/uninstall/"
	uninstallNoticeTimeout = 10 * time.Second
	// uninstallTold and uninstallRefused are the result line when the server was told, and when it
	// refused the credentials; any other starts with "not told: ".
	uninstallTold    = "told"
	uninstallRefused = "refused"
	// uninstallRequestFor is how long a request stands: the uninstaller makes it, stops the service at
	// once and waits for that, so another is an uninstall that failed, and this stop an ordinary one.
	uninstallRequestFor = 2 * time.Minute
)

// uninstallSender is the started OpAMP client (go/opamp's Client): Do sends to the server it uses
// now, at Endpoint, which is another than the configured one once the server moved this host.
type uninstallSender interface {
	Do(req *http.Request) (*http.Response, error)
	Endpoint() string
}

// noticeUninstall starts telling the server this host is being removed, when the uninstaller asked
// for it, and returns what waits for it to end (at most uninstallNoticeTimeout after the start) and
// says whether the server was told.
func (s *Supervisor) noticeUninstall() (wait func() (told bool)) {
	dir := s.config.Storage.Directory
	if _, ok := asked(s.telemetrySettings.Logger, dir, uninstallRequest, uninstallRequestFor, time.Now()); !ok {
		return func() bool { return false }
	}
	var sender uninstallSender
	if c, ok := s.opampClient.(*coreClient); ok {
		sender = c.uninstallSender()
	}
	done := make(chan bool, 1)
	go func() {
		told := false
		defer func() { done <- told }()
		ctx, cancel := context.WithTimeout(context.Background(), uninstallNoticeTimeout)
		defer cancel()
		result := sendUninstallNotice(ctx, sender)
		told = result == uninstallTold
		s.telemetrySettings.Logger.Info("This host is being removed: the notice to the OpAMP server", zap.String("result", result))
		if err := writeNotice(filepath.Join(dir, uninstallNoticeFile), result); err != nil {
			s.telemetrySettings.Logger.Warn("Could not write the uninstall notice's result", zap.Error(err))
		}
	}()
	return func() bool { return <-done }
}

// forgetClient removes the OpAMP client's saved state (the host's identity and the credentials the
// server issued it), once the client has stopped. The server has ended them with the uninstall, and
// it refuses a reinstall that comes with that identity and another token (it belongs to the
// uninstalled host): without the state, the reinstall enrolls as a new host.
func (s *Supervisor) forgetClient() {
	err := os.Remove(filepath.Join(s.config.Storage.Directory, coreClientStateFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.telemetrySettings.Logger.Warn("The saved state of the uninstalled host can't be removed: a reinstall with another token is refused until it is", zap.Error(err))
	}
}

// sendUninstallNotice sends the notice and says what came of it: "told", "refused", or "not told: why".
// Told is the route's own answer (204) and no other: anything else on the way to a server may answer any
// path with a page, and a host that takes that for it drops credentials that still work. The why names the HTTP status or the error, never a credential.
func sendUninstallNotice(ctx context.Context, sender uninstallSender) string {
	if sender == nil || sender.Endpoint() == "" {
		return "not told: the Supervisor wasn't connected to its server"
	}
	target, err := uninstallURL(sender.Endpoint())
	if err != nil {
		return "not told: " + err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, http.NoBody)
	if err != nil {
		return "not told: " + err.Error()
	}
	resp, err := sender.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Sprintf("not told: no answer within %s", uninstallNoticeTimeout)
		default:
			return "not told: " + redactURLError(err)
		}
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return uninstallTold
	case http.StatusUnauthorized:
		return uninstallRefused // revoked just before the uninstall, say
	case http.StatusNotFound:
		return "not told: the server has no such endpoint (HTTP 404), or Fleet is off there"
	default:
		return fmt.Sprintf("not told: HTTP %d", resp.StatusCode)
	}
}

// uninstallURL is uninstallPath at the endpoint's origin: http for ws, https for wss.
func uninstallURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", errors.New("the OpAMP endpoint isn't a URL")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("the OpAMP endpoint's scheme %q isn't one the notice can take", u.Scheme)
	}
	return (&url.URL{Scheme: scheme, Host: u.Host, Path: uninstallPath}).String(), nil
}

// redactURLError is err's text without a URL's user info (the endpoint as configured may hold
// credentials).
func redactURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.User = nil
			ue = &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
			return ue.Error()
		}
	}
	return err.Error()
}

// writeNotice replaces the file with the line, readable by the Supervisor's user alone.
func writeNotice(path, line string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(line+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
