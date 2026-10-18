// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: a .zip (the
// Windows release archive) is read too; the binary may sit in the archive's top directory, as in
// this repository's release archives; and only a regular file is taken.

// Package archive extracts collector executable updates from downloaded
// packages, dispatching on the archive format.
package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxAgentBytes is the maximum size of an agent binary the supervisor will write
// to disk during an install. It guards against unbounded writes.
const maxAgentBytes = 1 << 30 // 1 GiB

// Format is the format of the package downloaded by the supervisor.
type Format string

const (
	// FormatNone treats the downloaded package as a raw collector binary with no
	// archive wrapping.
	FormatNone Format = ""
	// FormatTarGzip treats the downloaded package as a gzipped tarball containing
	// the collector binary.
	FormatTarGzip Format = "tar.gz"
	// FormatZip treats the downloaded package as a zip archive containing the
	// collector binary.
	FormatZip Format = "zip"
)

// isAgentBinary says whether an archive entry named name is the binary named binaryName: at the
// archive's root, or in its one top directory (pydantic-opamp-supervisor_<version>_<os>_<arch>/).
func isAgentBinary(name, binaryName string) bool {
	if name == binaryName {
		return true
	}
	dir, file, ok := strings.Cut(name, "/")
	return ok && dir != "" && dir != ".." && file == binaryName
}

// Extractor extracts the agent binary from a downloaded package.
type Extractor interface {
	// Extract writes the agent binary contained in pkg to destination. binaryName
	// identifies which file inside the package is the agent binary for formats that
	// bundle multiple files; it is ignored for raw binaries.
	Extract(ctx context.Context, pkg []byte, binaryName, destination string) error
}

// NewExtractor returns the Extractor for the given archive format, or an error if
// the format is not supported.
func NewExtractor(format Format) (Extractor, error) {
	switch format {
	case FormatNone:
		return rawExtractor{maxBytes: maxAgentBytes}, nil
	case FormatTarGzip:
		return tarGzipExtractor{maxBytes: maxAgentBytes}, nil
	case FormatZip:
		return zipExtractor{maxBytes: maxAgentBytes}, nil
	default:
		return nil, fmt.Errorf("unsupported archive format: %q", string(format))
	}
}

var _ Extractor = rawExtractor{}

// rawExtractor treats the package bytes as a raw agent binary and writes them
// directly to destination.
type rawExtractor struct {
	// maxBytes is the maximum binary size the extractor will write. Inputs larger
	// than this are rejected rather than silently truncated.
	maxBytes int64
}

// Extract writes the raw bytes by creating or truncating the file at destination.
// It is the responsibility of the caller to ensure nothing of importance is overwritten.
// If an error occurs during write, the file at destination will be removed.
func (r rawExtractor) Extract(_ context.Context, pkg []byte, _, destination string) error {
	if int64(len(pkg)) > r.maxBytes {
		return fmt.Errorf("binary exceeds maximum size of %d bytes", r.maxBytes)
	}

	// Create or truncate the destination with executable permissions.
	if err := os.WriteFile(destination, pkg, 0o700); err != nil { //nolint:gosec // G306: the agent binary must be executable
		_ = os.Remove(destination)
		return fmt.Errorf("write binary to destination: %w", err)
	}

	return nil
}

var _ Extractor = tarGzipExtractor{}

// tarGzipExtractor extracts the file named binaryName from a gzipped tarball and
// writes it to destination.
type tarGzipExtractor struct {
	// maxBytes is the maximum binary size the extractor will write. The size a
	// tar header declares is the decompressed size of its entry, and tar.Reader
	// returns at most that many bytes, so entries declaring more than maxBytes
	// are rejected before anything is written.
	maxBytes int64
}

func (e tarGzipExtractor) Extract(_ context.Context, pkg []byte, binaryName, destination string) error {
	if binaryName == "" {
		return errors.New("agent binary name is required for tar.gz archives")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err != nil {
			return fmt.Errorf("read tarball looking for %q: %w", binaryName, err)
		}
		if !isAgentBinary(header.Name, binaryName) {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("%q in the tarball isn't a regular file", header.Name)
		}
		if header.Size > e.maxBytes {
			return fmt.Errorf("binary exceeds maximum size of %d bytes", e.maxBytes)
		}
		break
	}

	if err := writeBinaryToDestination(tarReader, destination); err != nil {
		return fmt.Errorf("write binary to destination: %w", err)
	}

	return nil
}

var _ Extractor = zipExtractor{}

// zipExtractor extracts the file named binaryName from a zip archive and writes it to
// destination.
type zipExtractor struct {
	// maxBytes is the maximum binary size the extractor will write. An entry declaring more is
	// refused before anything is written, and one that inflates to more than it declared fails.
	maxBytes int64
}

func (e zipExtractor) Extract(_ context.Context, pkg []byte, binaryName, destination string) error {
	if binaryName == "" {
		return errors.New("agent binary name is required for zip archives")
	}
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return fmt.Errorf("read zip archive: %w", err)
	}
	for _, f := range zr.File {
		if !isAgentBinary(f.Name, binaryName) {
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("%q in the zip archive isn't a regular file", f.Name)
		}
		if f.UncompressedSize64 > uint64(e.maxBytes) {
			return fmt.Errorf("binary exceeds maximum size of %d bytes", e.maxBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open %q in the zip archive: %w", f.Name, err)
		}
		defer rc.Close()
		// archive/zip fails a read past the declared size (ErrFormat), so the bound holds.
		if err := writeBinaryToDestination(rc, destination); err != nil {
			return fmt.Errorf("write binary to destination: %w", err)
		}
		return nil
	}
	return fmt.Errorf("read zip archive looking for %q: not found", binaryName)
}

// writeBinaryToDestination writes binary to destination, creating or truncating
// the file with executable permissions. If an error occurs during write, the
// file at destination will be removed.
func writeBinaryToDestination(binary io.Reader, destination string) error {
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return fmt.Errorf("open destination file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, binary); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("write binary to destination: %w", err)
	}

	return nil
}
