// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestFetch(t *testing.T) {
	body := archive(t, map[string]string{"README.md": "readme", "otelcol-contrib": "the collector", "sub/otelcol-contrib": "not it"})
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.2.3/otelcol-contrib_1.2.3_linux_amd64.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeLock := func(sha string) string {
		raw, err := json.Marshal(Lock{Version: "1.2.3", URL: srv.URL + "/v{version}/otelcol-contrib_{version}_{os}_{arch}.tar.gz",
			SHA256: map[string]string{"linux_amd64": sha, "windows_amd64": sha}})
		require.NoError(t, err)
		path := filepath.Join(dir, "contrib.json")
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		return path
	}
	lock := writeLock(hex.EncodeToString(sum[:]))
	out := filepath.Join(dir, "dist", "otelcol")
	require.NoError(t, run(srv.Client(), lock, "linux", "amd64", out))
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "the collector", string(got))

	assert.ErrorContains(t, run(srv.Client(), lock, "linux", "arm64", out), "no pinned otelcol-contrib for linux/arm64")
	assert.ErrorContains(t, run(srv.Client(), lock, "windows", "amd64", out), "HTTP 404")
	assert.ErrorContains(t, run(srv.Client(), writeLock("00"), "linux", "amd64", out), "not the pinned 00")
}

func TestExtractNeedsTheBinary(t *testing.T) {
	assert.ErrorContains(t, extract(archive(t, map[string]string{"README.md": "x"}), "otelcol-contrib", filepath.Join(t.TempDir(), "o")), "has no otelcol-contrib")
}

func TestFetchTriesAgainAfterAServerError(t *testing.T) {
	retryWait = time.Millisecond
	body := archive(t, map[string]string{"otelcol-contrib": "the collector"})
	sum := sha256.Sum256(body)
	var gets, missing atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			missing.Add(1)
			http.NotFound(w, r)
			return
		}
		if gets.Add(1) < 3 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	got, err := download(srv.Client(), srv.URL+"/archive", hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, int32(3), gets.Load(), "two 504s, then the archive")

	_, err = download(srv.Client(), srv.URL+"/missing", "00")
	assert.ErrorContains(t, err, "HTTP 404")
	assert.Equal(t, int32(1), missing.Load(), "a 404 is not tried again")

	gets.Store(-10)
	_, err = download(srv.Client(), srv.URL+"/archive", "00")
	assert.ErrorContains(t, err, "HTTP 504", "the last try's error, after every attempt")
	assert.Equal(t, int32(-10+attempts), gets.Load())
}
