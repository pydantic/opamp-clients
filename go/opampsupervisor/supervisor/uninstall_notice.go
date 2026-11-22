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
// package's uninstaller writes uninstallRequestFile in the storage directory before it stops the
// service, and the Supervisor, stopping, sends uninstallPath at its server's origin with the
// credentials it connects with (those the server issued it, or, before it issued any, the
// enrollment token in its config: the server refuses that one), then writes what came of it to
// uninstallNoticeFile, for the uninstaller to say. The credentials never leave the Supervisor:
// the uninstaller can't read them (they are sealed in the client's state), and needs nothing but
// the two files. The request is bounded by uninstallNoticeTimeout, runs while the Supervisor stops
// its collector (so it adds little to a stop that has a limit: the service manager's, or the
// uninstaller's own wait), and the stop goes on whatever came of it.
const (
	uninstallRequestFile   = "uninstall-requested"
	uninstallNoticeFile    = "uninstall-notice"
	uninstallPath          = "/v1/fleet/installations/self/uninstall/"
	uninstallNoticeTimeout = 10 * time.Second
	// uninstallRequestMaxAge bounds how old a request may be: the uninstaller makes it and stops the
	// service at once, so an older one is an uninstall that failed, and this stop an ordinary one.
	uninstallRequestMaxAge = 2 * time.Minute
)

// uninstallSender is the started OpAMP client (go/opamp's Client): Do sends to the server it uses
// now, at Endpoint, which is another than the configured one once the server moved this host.
type uninstallSender interface {
	Do(req *http.Request) (*http.Response, error)
	Endpoint() string
}

// noticeUninstall starts telling the server this host is being removed, when the uninstaller asked
// for it, and returns what waits for it to end (at most uninstallNoticeTimeout after the start).
func (s *Supervisor) noticeUninstall() (wait func()) {
	dir := s.config.Storage.Directory
	request := filepath.Join(dir, uninstallRequestFile)
	info, err := os.Lstat(request)
	if err != nil {
		return func() {}
	}
	if err := os.Remove(request); err != nil {
		// It would ask again at every later stop.
		s.telemetrySettings.Logger.Warn("The uninstall request can't be removed: the OpAMP server isn't told", zap.Error(err))
		return func() {}
	}
	if age := time.Since(info.ModTime()); age > uninstallRequestMaxAge || age < -uninstallRequestMaxAge {
		s.telemetrySettings.Logger.Warn("An uninstall was asked for and not made: the OpAMP server isn't told", zap.Duration("asked", age))
		return func() {}
	}
	var sender uninstallSender
	if c, ok := s.opampClient.(*coreClient); ok {
		sender = c.uninstallSender()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), uninstallNoticeTimeout)
		defer cancel()
		result := sendUninstallNotice(ctx, sender)
		s.telemetrySettings.Logger.Info("This host is being removed: the notice to the OpAMP server", zap.String("result", result))
		if err := writeNotice(filepath.Join(dir, uninstallNoticeFile), result); err != nil {
			s.telemetrySettings.Logger.Warn("Could not write the uninstall notice's result", zap.Error(err))
		}
	}()
	return func() { <-done }
}

// sendUninstallNotice sends the notice and says what came of it: "told", or "not told: why". The
// why names the HTTP status or the error, never a credential.
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
	case http.StatusNoContent, http.StatusOK:
		return "told"
	case http.StatusUnauthorized:
		return "not told: the server didn't take this host's credentials (HTTP 401)"
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
