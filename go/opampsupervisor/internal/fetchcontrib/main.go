// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Command fetchcontrib puts upstream's otelcol-contrib, the Collector the packages ship, at -out:
// it downloads the release archive packaging/contrib.json pins for -os and -arch, refuses it
// unless its SHA-256 is the pinned one, and extracts the binary. Run from go/opampsupervisor:
//
//	go run ./internal/fetchcontrib -os linux -arch amd64 -out packaging/dist/otelcol/otelcol
//
// A build host's GOOS and GOARCH are for the packages, so run it with them unset.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lock is packaging/contrib.json: the release, where its archives are, and each one's SHA-256.
type Lock struct {
	Version string            `json:"version"`
	URL     string            `json:"url"`
	SHA256  map[string]string `json:"sha256"`
}

// archiveLimit bounds a download: the archives are about 100 MB.
const archiveLimit = 512 << 20

func main() {
	lockPath := flag.String("lock", "packaging/contrib.json", "the pinned release")
	goos := flag.String("os", "", "linux, darwin or windows")
	goarch := flag.String("arch", "", "amd64 or arm64")
	out := flag.String("out", "", "where to write the Collector")
	flag.Parse()
	if err := run(http.DefaultClient, *lockPath, *goos, *goarch, *out); err != nil {
		fmt.Fprintln(os.Stderr, "fetchcontrib:", err)
		os.Exit(1)
	}
}

func run(client *http.Client, lockPath, goos, goarch, out string) error {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var lock Lock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return fmt.Errorf("%s: %w", lockPath, err)
	}
	want, ok := lock.SHA256[goos+"_"+goarch]
	if !ok || out == "" {
		return fmt.Errorf("no pinned otelcol-contrib for %s/%s (or no -out)", goos, goarch)
	}
	url := strings.NewReplacer("{version}", lock.Version, "{os}", goos, "{arch}", goarch).Replace(lock.URL)
	archive, err := download(client, url, want)
	if err != nil {
		return err
	}
	binary := "otelcol-contrib"
	if goos == "windows" {
		binary += ".exe"
	}
	return extract(archive, binary, out)
}

// download fetches url, refusing it unless its SHA-256 is want.
func download(client *http.Client, url, want string) ([]byte, error) {
	client.Timeout = 10 * time.Minute
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, archiveLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if len(body) > archiveLimit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, archiveLimit)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%s: SHA-256 %s, not the pinned %s", url, got, want)
	}
	return body, nil
}

// extract writes the archive's file named binary to out, executable.
func extract(archive []byte, binary, out string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive has no %s", binary)
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != binary || filepath.Dir(header.Name) != "." {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		tmp := out + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, archiveLimit)); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, out)
	}
}
