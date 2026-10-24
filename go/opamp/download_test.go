// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// A download follows no redirect and judges no status: whatever comes is written to the file and
// hashed, and reported with its status (and a redirect's Location resolved, with its origin). Too
// large, or no response at all: failed, and nothing left.
func TestDownloadsReportEveryStatus(t *testing.T) {
	body := bytes.Repeat([]byte("package"), 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.Header().Set("Location", "http://bad host:1/file")
			w.WriteHeader(http.StatusFound)
		case "/moved":
			w.Header().Set("Location", "/file")
			w.WriteHeader(http.StatusFound)
			w.Write([]byte("see /file"))
		case "/file":
			if r.Header.Get("Authorization") != "t" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("no such file"))
		}
	}))
	defer srv.Close()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(filepath.Join(t.TempDir(), "state"))
	auth := []*corepb.Header{{Name: "Authorization", Value: "t"}}
	get := func(id uint64, path string, max uint64) *corepb.Input {
		return x.download(context.Background(), &corepb.Download{Id: id, Url: srv.URL + path, Headers: auth, MaxBytes: max, TimeoutMs: 5000})
	}
	check := func(id uint64, in *corepb.Input, want *corepb.DownloadResponse, file []byte) {
		t.Helper()
		sum := sha256.Sum256(file)
		want.Id, want.Sha256 = id, sum[:]
		if !proto.Equal(in.GetDownloadResponse(), want) {
			t.Fatalf("download %d: %v, want %v", id, in, want)
		}
		if got, err := os.ReadFile(x.downloadPath(id)); err != nil || !bytes.Equal(got, file) {
			t.Fatalf("download %d: file %q, %v", id, got, err)
		}
	}

	check(1, get(1, "/file", 1<<20), &corepb.DownloadResponse{Status: 200}, body)
	// The user's only.
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{downloadsDir(x.stateFile): 0o700, x.downloadPath(1): 0o600} {
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
				t.Fatalf("%s: %v %v", path, fi.Mode().Perm(), err)
			}
		}
	}
	// A redirect is reported, not followed.
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	check(2, get(2, "/moved", 1<<20), &corepb.DownloadResponse{Status: 302, Location: srv.URL + "/file",
		LocationOrigin: &corepb.Origin{Scheme: "http", Host: u.Hostname(), Port: uint32(port)}}, []byte("see /file"))
	check(3, get(3, "/missing", 1<<20), &corepb.DownloadResponse{Status: 404}, []byte("no such file"))
	// A Location net/url can't read goes as given, with no origin: the core refuses it. (Not a
	// failed download, which the core would try again: http.Client fails on it even when it
	// follows no redirect.)
	check(6, get(6, "/bad", 1<<20), &corepb.DownloadResponse{Status: 302, Location: "http://bad host:1/file"}, nil)

	// Discard deletes it.
	x.do(&corepb.Effect{Kind: &corepb.Effect_Discard{Discard: &corepb.Id{Id: 1}}})
	if _, err := os.Stat(x.downloadPath(1)); !os.IsNotExist(err) {
		t.Fatalf("not discarded: %v", err)
	}
	// Too large, or no server: failed, and nothing left.
	for id, u := range map[uint64]string{4: srv.URL + "/file", 5: "http://127.0.0.1:1/file"} {
		in := x.download(context.Background(), &corepb.Download{Id: id, Url: u, Headers: auth, MaxBytes: 10, TimeoutMs: 5000})
		if in.GetDownloadFailed().GetId() != id {
			t.Fatalf("%d: %v", id, in)
		}
		if _, err := os.Stat(x.downloadPath(id)); !os.IsNotExist(err) {
			t.Fatalf("%d left a file: %v", id, err)
		}
	}
}

// A redirect's Location, resolved against the URL requested by net/url, and that URL's origin
// (port 0 for the scheme's default). One that doesn't resolve goes as given, with no origin.
func TestRedirectLocations(t *testing.T) {
	base, _ := url.Parse("https://files.example.com:8443/pkgs/v1/agent.tar.gz?sig=1")
	cases := []struct {
		given, want string
		origin      *corepb.Origin
	}{
		{"", "", nil},
		{"agent-2.tar.gz", "https://files.example.com:8443/pkgs/v1/agent-2.tar.gz",
			&corepb.Origin{Scheme: "https", Host: "files.example.com", Port: 8443}},
		{"../v2/./agent.tar.gz", "https://files.example.com:8443/pkgs/v2/agent.tar.gz",
			&corepb.Origin{Scheme: "https", Host: "files.example.com", Port: 8443}},
		{"/a/b/../c", "https://files.example.com:8443/a/c",
			&corepb.Origin{Scheme: "https", Host: "files.example.com", Port: 8443}},
		{"//mirror.example.org/agent.tar.gz", "https://mirror.example.org/agent.tar.gz",
			&corepb.Origin{Scheme: "https", Host: "mirror.example.org"}},
		{"http://mirror.example.org:80/x", "http://mirror.example.org:80/x",
			&corepb.Origin{Scheme: "http", Host: "mirror.example.org"}},
		{"http://[::1]:9000/x", "http://[::1]:9000/x", &corepb.Origin{Scheme: "http", Host: "::1", Port: 9000}},
		{"https://mirror.example.org:443/x", "https://mirror.example.org:443/x",
			&corepb.Origin{Scheme: "https", Host: "mirror.example.org"}},
		{"http://[bad", "http://[bad", nil},
		{"http://host:99999/x", "http://host:99999/x", nil},
		// Port 0 can't be told from the scheme's default in an origin: no place to go.
		{"http://host:0/x", "http://host:0/x", nil},
		{"http://host:000/x", "http://host:000/x", nil},
	}
	for _, c := range cases {
		got, origin := location(base, c.given)
		if got != c.want || !proto.Equal(origin, c.origin) {
			t.Errorf("%q: %q %v, want %q %v", c.given, got, origin, c.want, c.origin)
		}
	}
}

// The TLS settings the client keeps for the OpAMP server (its client certificate, skipping
// verification) go only to a file at the server's own origin, as the core says
// (Download.server_origin). A file anywhere else is fetched with neither, still checked against
// the CAs configured.
func TestOnlyTheServersOriginGetsItsTLSSettings(t *testing.T) {
	ca := newTestCA(t, "test CA")
	_, _, pair := ca.issue(t, "files", true)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			w.Write([]byte("certificate " + r.TLS.PeerCertificates[0].Subject.CommonName))
			return
		}
		w.Write([]byte("no certificate"))
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are expected
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: ca.pool}
	ts.StartTLS()
	defer ts.Close()
	certPEM, keyPEM, _ := ca.issue(t, "agent", false)

	fetch := func(t *testing.T, conn *connection, serverOrigin bool) (string, string) {
		t.Helper()
		x := newExecutor(conn)
		x.setStateFile(filepath.Join(t.TempDir(), "state"))
		in := x.download(context.Background(), &corepb.Download{Id: 1, Url: ts.URL + "/file", MaxBytes: 1 << 20,
			TimeoutMs: 5000, ServerOrigin: serverOrigin})
		if f := in.GetDownloadFailed(); f != nil {
			return "", f.Detail
		}
		got, err := os.ReadFile(x.downloadPath(1))
		if err != nil {
			t.Fatal(err)
		}
		return string(got), ""
	}
	mtls := TLS{CAPEM: ca.pem, CertPEM: certPEM, KeyPEM: keyPEM}
	insecure := TLS{Config: &tls.Config{InsecureSkipVerify: true}}
	transport := func(tl TLS) *connection {
		cfg, err := tl.tlsConfig()
		if err != nil {
			t.Fatal(err)
		}
		return connectionFrom(&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}})
	}
	for name, conn := range map[string]func(TLS) *connection{
		"options":                     func(tl TLS) *connection { return testConnection(t, tl, Proxy{Direct: true}) },
		"the application's transport": transport,
	} {
		t.Run(name, func(t *testing.T) {
			if got, why := fetch(t, conn(mtls), true); got != "certificate agent" {
				t.Fatalf("the server's origin: %q %s", got, why)
			}
			if got, why := fetch(t, conn(mtls), false); got != "no certificate" {
				t.Fatalf("elsewhere: %q %s, want no client certificate", got, why)
			}
			if got, why := fetch(t, conn(insecure), true); got != "no certificate" {
				t.Fatalf("the server's origin, unverified: %q %s", got, why)
			}
			if got, why := fetch(t, conn(insecure), false); why == "" || !strings.Contains(why, "certificate") {
				t.Fatalf("elsewhere, unverified: %q, want the server's certificate refused", got)
			}
		})
	}
	// A RoundTripper of the application's own is the application's: used as it is.
	own := connectionFrom(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		cfg, _ := mtls.tlsConfig()
		return (&http.Transport{TLSClientConfig: cfg}).RoundTrip(r)
	})})
	if got, why := fetch(t, own, false); got != "certificate agent" {
		t.Fatalf("the application's own RoundTripper: %q %s", got, why)
	}
}
