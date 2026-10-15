// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build repo

// publish.sh against buckets fake-gcs-server and moto serve over HTTP, as a CDN would serve the
// real one: apt installs from it by URL, earlier releases stay, and the published key and packages
// can't be replaced. Needs what repo_test.go needs, plus gcloud, fake-gcs-server, moto_server and
// the aws CLI (to make moto's bucket) on PATH.

package packaging

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stores are the kinds of bucket publish.sh publishes to: GCS, as packages.pydantic.dev is, and
// S3, for the S3-compatible stores (MinIO, R2) an on-prem import writes to.
var stores = []string{"gs", "s3"}

// bucket is an empty bucket on a fake server of the test's own: fake-gcs-server for gs, moto for s3.
type bucket struct {
	env []string // gcloud's settings for it, gcloud itself logging each call to log
	url string   // where it is served, unauthenticated
	at  string   // its gs:// or s3:// name
	log string
}

// serve starts a server listening on the port it is given, and returns its address once it answers.
func serve(t *testing.T, name string, args ...string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "PORT", port)
	}
	server := exec.Command(name, args...)
	require.NoError(t, server.Start())
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	endpoint := "http://127.0.0.1:" + port
	require.Eventually(t, func() bool {
		resp, err := http.Get(endpoint)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil
	}, 30*time.Second, 100*time.Millisecond, "%s didn't start", name)
	return endpoint
}

func newBucket(t *testing.T, store string) bucket {
	t.Helper()
	// gcloud, as publish.sh finds it on PATH, logs each call before making it.
	real, err := exec.LookPath("gcloud")
	require.NoError(t, err)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "gcloud.log")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gcloud"),
		[]byte("#!/bin/sh\necho \"$*\" >>'"+log+"'\nexec '"+real+"' \"$@\"\n"), 0o755))
	b := bucket{at: store + "://packages", log: log, env: []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "CLOUDSDK_CONFIG=" + t.TempDir()}}
	if store == "gs" {
		endpoint := serve(t, "fake-gcs-server", "-scheme", "http", "-host", "127.0.0.1", "-port", "PORT",
			"-backend", "memory", "-public-host", "127.0.0.1:PORT")
		token := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(token, []byte("test"), 0o600))
		b.env = append(b.env, "CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE="+endpoint+"/storage/v1/",
			"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE="+token, "CLOUDSDK_CORE_PROJECT=test",
			// gcloud sends resumable uploads to a path of its own under an overridden endpoint,
			// which the fake doesn't serve: these all go up whole.
			"CLOUDSDK_STORAGE_RESUMABLE_THRESHOLD=1GiB")
		b.url = endpoint + "/packages"
		must(t, b.env, "gcloud", "storage", "buckets", "create", b.at)
	} else {
		endpoint := serve(t, "moto_server", "-H", "127.0.0.1", "-p", "PORT")
		b.env = append(b.env, "CLOUDSDK_STORAGE_S3_ENDPOINT_URL="+endpoint,
			"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_DEFAULT_REGION=us-east-1")
		b.url = endpoint + "/packages"
		// gcloud can't make an S3 bucket, or open one to anyone; the aws CLI can.
		aws := append([]string{"AWS_ENDPOINT_URL=" + endpoint}, b.env...)
		must(t, aws, "aws", "s3", "mb", b.at)
		// Readable by anyone, as the real bucket (or the CDN in front of it) is.
		must(t, aws, "aws", "s3api", "put-bucket-policy", "--bucket", "packages", "--policy",
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::packages/*"}]}`)
	}
	return b
}

func (b bucket) publish(t *testing.T, key string) (string, error) {
	t.Helper()
	require.NoError(t, os.WriteFile(b.log, nil, 0o644))
	return command(t, append(b.env, "REPO_BUCKET="+b.at, "REPO_URL="+b.url, "SIGNING_KEY_FILE="+key), "./publish.sh")
}

// get is an object's contents, as a client fetches it.
func (b bucket) get(t *testing.T, path string) (string, http.Header) {
	t.Helper()
	resp, err := http.Get(b.url + "/" + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, path)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body), resp.Header
}

func (b bucket) put(t *testing.T, file, path string) {
	t.Helper()
	must(t, b.env, "gcloud", "storage", "cp", file, b.at+"/"+path)
}

func builtDeb(t *testing.T) string {
	t.Helper()
	built, err := filepath.Glob("dist/" + pkg + "_*_amd64.deb")
	require.NoError(t, err)
	require.Len(t, built, 1)
	return built[0]
}

func TestPublishAddsToThePublishedRepositories(t *testing.T) {
	for _, store := range stores {
		t.Run(store, func(t *testing.T) {
			b := newBucket(t, store)
			// An earlier release, already published.
			old := filepath.Join(t.TempDir(), "old")
			require.NoError(t, os.MkdirAll(filepath.Join(old, "DEBIAN"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(old, "DEBIAN", "control"), []byte("Package: "+pkg+
				"\nVersion: 0~earlier\nArchitecture: amd64\nMaintainer: Pydantic <hello@pydantic.dev>\nDescription: an earlier release\n"), 0o644))
			earlier := filepath.Join(t.TempDir(), pkg+"_0~earlier_amd64.deb")
			must(t, nil, "dpkg-deb", "--root-owner-group", "--build", old, earlier)
			b.put(t, earlier, "deb/pool/main/"+filepath.Base(earlier))

			out, err := b.publish(t, signingKey(t))
			require.NoError(t, err, out)
			// Published again, as a re-run release would: the same packages are no change.
			out, err = b.publish(t, signingKey(t))
			require.NoError(t, err, out)

			// apt, from the bucket's URL with the key published there.
			keyring := filepath.Join(t.TempDir(), "pydantic-opamp.gpg")
			key, _ := b.get(t, "pydantic-opamp.gpg")
			require.NoError(t, os.WriteFile(keyring, []byte(key), 0o644))
			repo := t.TempDir()
			sources, _ := b.get(t, "pydantic-opamp.sources")
			assert.Contains(t, sources, "URIs: "+b.url+"/deb\n")
			require.NoError(t, os.WriteFile(filepath.Join(repo, "pydantic-opamp.sources"), []byte(sources), 0o644))
			get, root := apt(t, repo, keyring)
			out, err = get("apt-get", "update")
			require.NoError(t, err, out)
			assert.NotContains(t, out, "W:", "apt warned about the repository")
			versions, err := get("apt-cache", "madison", pkg)
			require.NoError(t, err, versions)
			assert.Contains(t, versions, " 0~earlier ")
			version := strings.Split(filepath.Base(builtDeb(t)), "_")[1]
			assert.Contains(t, versions, " "+version+" ")
			out, err = get("apt-get", "download", pkg)
			require.NoError(t, err, out)
			got, err := filepath.Glob(filepath.Join(root, pkg+"_"+version+"_amd64.deb"))
			require.NoError(t, err)
			assert.Len(t, got, 1, out)

			// The yum repository's index is signed, and lists the rpm.
			repomd, _ := b.get(t, "rpm/repodata/repomd.xml")
			signature, _ := b.get(t, "rpm/repodata/repomd.xml.asc")
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "repomd.xml"), []byte(repomd), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "repomd.xml.asc"), []byte(signature), 0o644))
			must(t, nil, "gpgv", "--keyring", keyring, filepath.Join(dir, "repomd.xml.asc"), filepath.Join(dir, "repomd.xml"))

			// Packages may be cached for good; the indexes must be fetched fresh.
			_, header := b.get(t, "deb/pool/main/"+filepath.Base(builtDeb(t)))
			assert.Contains(t, header.Get("Cache-Control"), "immutable")
			for _, index := range []string{"deb/dists/stable/InRelease", "deb/dists/stable/main/binary-amd64/Packages", "rpm/repodata/repomd.xml"} {
				_, header = b.get(t, index)
				assert.Equal(t, "no-cache", header.Get("Cache-Control"), index)
			}
		})
	}
}

// Mid-publish, a client sees the old indexes or new ones whose packages are all uploaded: each
// index after what it lists, each signature after what it signs.
func TestPublishUploadsPackagesBeforeTheirIndexes(t *testing.T) {
	for _, store := range stores {
		t.Run(store, func(t *testing.T) {
			b := newBucket(t, store)
			out, err := b.publish(t, signingKey(t))
			require.NoError(t, err, out)
			log, err := os.ReadFile(b.log)
			require.NoError(t, err)
			calls := strings.Split(strings.TrimSpace(string(log)), "\n")
			// Each upload's destination is its last argument.
			at := func(path string) int {
				for i, call := range calls {
					if fields := strings.Fields(call); fields[len(fields)-1] == b.at+"/"+path {
						return i
					}
				}
				t.Fatalf("no upload to %s in:\n%s", path, log)
				return 0
			}
			for _, order := range [][2]string{
				{"deb/pool", "deb/dists"},
				{"deb/dists", "deb/dists/stable/Release"},
				{"deb/dists/stable/Release", "deb/dists/stable/Release.gpg"},
				{"deb/dists/stable/Release.gpg", "deb/dists/stable/InRelease"},
				{"rpm", "rpm/repodata"},
				{"rpm/repodata", "rpm/repodata/repomd.xml"},
				{"rpm/repodata/repomd.xml", "rpm/repodata/repomd.xml.asc"},
			} {
				assert.Less(t, at(order[0]), at(order[1]), "%s uploaded after %s", order[0], order[1])
			}
		})
	}
}

func TestPublishRefusesAnotherKey(t *testing.T) {
	for _, store := range stores {
		t.Run(store, func(t *testing.T) {
			b := newBucket(t, store)
			out, err := b.publish(t, signingKey(t))
			require.NoError(t, err, out)
			before, _ := b.get(t, "deb/dists/stable/InRelease")

			out, err = b.publish(t, newKey(t, t.TempDir()))
			assert.Error(t, err, out)
			assert.Contains(t, out, "refusing to re-sign them")
			after, _ := b.get(t, "deb/dists/stable/InRelease")
			assert.Equal(t, before, after, "the repositories were re-signed")
		})
	}
}

func TestPublishRefusesToReplaceAPublishedPackage(t *testing.T) {
	for _, store := range stores {
		t.Run(store, func(t *testing.T) {
			b := newBucket(t, store)
			other := filepath.Join(t.TempDir(), "other.deb")
			require.NoError(t, os.WriteFile(other, []byte("another build of this version"), 0o644))
			path := "deb/pool/main/" + filepath.Base(builtDeb(t))
			b.put(t, other, path)

			out, err := b.publish(t, signingKey(t))
			assert.Error(t, err, out)
			assert.Contains(t, out, "already published, with other contents")
			got, _ := b.get(t, path)
			assert.Equal(t, "another build of this version", got)
			log, err := os.ReadFile(b.log)
			require.NoError(t, err)
			assert.NotContains(t, string(log), "--cache-control", "it uploaded before refusing")
		})
	}
}
