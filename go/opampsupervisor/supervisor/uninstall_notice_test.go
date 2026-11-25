// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

type fakeSender struct {
	endpoint string
	do       func(*http.Request) (*http.Response, error)
}

func (f fakeSender) Do(r *http.Request) (*http.Response, error) { return f.do(r) }
func (f fakeSender) Endpoint() string                           { return f.endpoint }

func sender(endpoint string, do func(*http.Request) (*http.Response, error)) fakeSender {
	return fakeSender{endpoint: endpoint, do: do}
}

func answer(status int) fakeSender {
	return sender("https://fleet.example/v1/opamp", func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: http.NoBody, Request: r}, nil
	})
}

// The uninstall notice is a POST to the server's origin, and its result line names the outcome,
// never a credential.
func TestTheUninstallNotice(t *testing.T) {
	var got *http.Request
	sent := sender("wss://fleet.example:8443/v1/opamp", func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	assert.Equal(t, "told", sendUninstallNotice(context.Background(), sent))
	require.NotNil(t, got)
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "https://fleet.example:8443/v1/fleet/installations/self/uninstall/", got.URL.String())

	for status, want := range map[int]string{
		http.StatusUnauthorized:        "refused",
		http.StatusOK:                  "not told: HTTP 200",
		http.StatusFound:               "not told: HTTP 302",
		http.StatusNotFound:            "not told: the server has no such endpoint (HTTP 404), or Fleet is off there",
		http.StatusInternalServerError: "not told: HTTP 500",
	} {
		assert.Equal(t, want, sendUninstallNotice(context.Background(), answer(status)))
	}
	assert.Equal(t, "not told: the Supervisor wasn't connected to its server", sendUninstallNotice(context.Background(), nil))
	assert.Equal(t, "not told: the Supervisor wasn't connected to its server", sendUninstallNotice(context.Background(), sender("", nil)), "no connection yet")

	// A transport error with the endpoint's user info: the result line leaves it out.
	failing := sender("https://fleet.example/v1/opamp", func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://user:secret@fleet.example/v1/fleet/installations/self/uninstall/", Err: errors.New("connection refused")}
	})
	line := sendUninstallNotice(context.Background(), failing)
	assert.Contains(t, line, "connection refused")
	assert.NotContains(t, line, "secret")

	// No answer in time.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	slow := sender("https://fleet.example/v1/opamp", func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	assert.Equal(t, "not told: no answer within 10s", sendUninstallNotice(ctx, slow))

	for endpoint, want := range map[string]string{
		"ws://127.0.0.1:55039/v1/opamp":     "http://127.0.0.1:55039/v1/fleet/installations/self/uninstall/",
		"https://user:pw@fleet.example/a/b": "https://fleet.example/v1/fleet/installations/self/uninstall/",
	} {
		got, err := uninstallURL(endpoint)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := uninstallURL("unix:///run/opamp.sock")
	assert.Error(t, err)
}

// The Supervisor sends it only when the uninstaller asked just now (the request, made beside its
// state, by its process ID), takes the request, and writes the
// result line where the uninstaller reads it, readable by its user alone.
func TestTheSupervisorNoticesAnUninstallOnlyWhenAsked(t *testing.T) {
	newSupervisor := func(t *testing.T) (*Supervisor, string) {
		dir := t.TempDir()
		s := &Supervisor{config: config.Supervisor{Storage: config.Storage{Directory: dir}, Server: config.OpAMPServer{Endpoint: "wss://fleet.example/v1/opamp"}}}
		s.telemetrySettings.Logger = zap.NewNop()
		return s, dir
	}
	notice := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, uninstallNoticeFile))
		if err != nil {
			return ""
		}
		return string(b)
	}

	s, dir := newSupervisor(t)
	s.noticeUninstall()()
	assert.Empty(t, notice(dir), "not asked: nothing written")

	ask := func(t *testing.T, dir string, pid int, ago time.Duration) string {
		request := filepath.Join(dir, uninstallRequest+strconv.Itoa(pid))
		require.NoError(t, os.Mkdir(request, 0o700))
		at := time.Now().Add(-ago)
		require.NoError(t, os.Chtimes(request, at, at))
		return request
	}
	s, dir = newSupervisor(t)
	request := ask(t, dir, os.Getpid(), 0)
	s.noticeUninstall()()
	assert.Equal(t, "not told: the Supervisor wasn't connected to its server\n", notice(dir), "no client here")
	assert.NoDirExists(t, request, "the request is taken")
	if info, err := os.Stat(filepath.Join(dir, uninstallNoticeFile)); assert.NoError(t, err) && os.PathSeparator == '/' {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	// A request whose uninstaller ended (killed before the stop, say), one too old, or one this can't remove, tells nothing: the stop is an ordinary one.
	for name, left := range map[string]func(t *testing.T, dir string) string{
		"an uninstaller that ended": func(t *testing.T, dir string) string { return ask(t, dir, deadPID(t), 0) },
		"an uninstaller that hangs": func(t *testing.T, dir string) string {
			return ask(t, dir, longRunning(t), uninstallRequestFor+time.Second)
		},
		"a name without an ID": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, uninstallRequest)
			require.NoError(t, os.Mkdir(p, 0o700))
			return p
		},
		"a request that isn't a directory": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, uninstallRequest+strconv.Itoa(os.Getpid()))
			require.NoError(t, os.WriteFile(p, nil, 0o600))
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := newSupervisor(t)
			request := left(t, dir)
			assert.False(t, s.noticeUninstall()())
			assert.Empty(t, notice(dir))
			_, err := os.Lstat(request)
			assert.ErrorIs(t, err, os.ErrNotExist, "taken away")
		})
	}
	t.Run("a request that can't be removed", func(t *testing.T) {
		s, dir := newSupervisor(t)
		require.NoError(t, os.WriteFile(filepath.Join(ask(t, dir, os.Getpid(), 0), "x"), nil, 0o600), "os.Remove refuses a directory that isn't empty")
		s.noticeUninstall()()
		assert.Empty(t, notice(dir), "it would be sent at every later stop")
	})
}

// Through a real client: the notice goes to the server's origin with the credentials the client
// connects with, and the file says told.
func TestTheUninstallNoticeThroughTheClient(t *testing.T) {
	ts := newCoreTestServer(t)
	target, err := url.Parse(ts.url)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var auths []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == uninstallPath && r.Method == http.MethodPost {
			mu.Lock()
			auths = append(auths, r.Header.Get("Authorization"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	dir := t.TempDir()
	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(dir, coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL: front.URL + "/v1/opamp",
		InstanceUid:    types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		Header:         http.Header{"Authorization": {"Bearer the-host-key"}},
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()
	require.Eventually(t, func() bool { return c.uninstallSender() != nil && c.uninstallSender().Endpoint() != "" }, 10*time.Second, 20*time.Millisecond)

	s := &Supervisor{config: config.Supervisor{Storage: config.Storage{Directory: dir}}, opampClient: c}
	s.telemetrySettings.Logger = zap.NewNop()
	require.NoError(t, os.Mkdir(filepath.Join(dir, uninstallRequest+strconv.Itoa(os.Getpid())), 0o700))
	assert.True(t, s.noticeUninstall()(), "told")
	b, err := os.ReadFile(filepath.Join(dir, uninstallNoticeFile))
	require.NoError(t, err)
	assert.Equal(t, "told\n", string(b))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"Bearer the-host-key"}, auths)
}

// A host the server was told about forgets its identity and credentials: they are the uninstalled
// host's, and the server refuses another token that comes with them. One it wasn't told about keeps
// them: they still work.
func TestAToldUninstallForgetsTheClientsState(t *testing.T) {
	setup := func(t *testing.T) (*Supervisor, string, func() bool) {
		dir := t.TempDir()
		s := &Supervisor{config: config.Supervisor{Storage: config.Storage{Directory: dir}}}
		s.telemetrySettings.Logger = zap.NewNop()
		state := filepath.Join(dir, coreClientStateFile)
		require.NoError(t, os.WriteFile(state, []byte("sealed"), 0o600))
		return s, dir, func() bool { _, err := os.Stat(state); return err == nil }
	}
	s, _, kept := setup(t)
	s.forgetClient()
	assert.False(t, kept(), "gone")
	s.forgetClient() // nothing left to forget

	s, dir, kept := setup(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, uninstallRequest+strconv.Itoa(os.Getpid())), 0o700))
	if s.noticeUninstall()() { // not connected: not told
		s.forgetClient()
	}
	assert.True(t, kept(), "not told: kept")
}

// The server may have acted on a notice whose answer never arrived (the connection was cut, or the
// process killed): the host can't know, so it says it wasn't told and keeps its state. Its next
// enrollment, refused as revoked (401), is the enroll helper's to start again from the token.
func TestAnUninstallNoticeWithoutAnAnswerKeepsTheState(t *testing.T) {
	acted := 0
	lost := fakeSender{endpoint: "https://fleet.example/v1/opamp", do: func(*http.Request) (*http.Response, error) {
		acted++
		return nil, io.ErrUnexpectedEOF
	}}
	assert.Equal(t, "not told: unexpected EOF", sendUninstallNotice(context.Background(), lost))
	assert.Equal(t, 1, acted, "the server was asked, and no more")
}
