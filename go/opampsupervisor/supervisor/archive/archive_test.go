// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: tests for
// zip archives, a binary in the archive's top directory, and an entry that isn't a regular file.

package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewExtractor(t *testing.T) {
	testCases := []struct {
		name      string
		format    Format
		expectErr string
	}{
		{
			name:   "no archive returns raw extractor",
			format: FormatNone,
		},
		{
			name:   "tar.gz returns tar.gz installer",
			format: FormatTarGzip,
		},
		{
			name:   "zip returns zip installer",
			format: FormatZip,
		},
		{
			name:      "unsupported archive format",
			format:    Format("rar"),
			expectErr: "unsupported archive format",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			extractor, err := NewExtractor(tc.format)
			if tc.expectErr != "" {
				require.ErrorContains(t, err, tc.expectErr)
				assert.Nil(t, extractor)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, extractor)
		})
	}
}

func TestRawExtractor_Extract(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "otelcol-contrib")
	contents := []byte("raw collector binary")

	extractor, err := NewExtractor(FormatNone)
	require.NoError(t, err)

	require.NoError(t, extractor.Extract(t.Context(), contents, "otelcol-contrib", destination))

	written, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, contents, written)
}

func TestRawExtractor_Extract_Size(t *testing.T) {
	const maxBytes = 16

	testCases := []struct {
		name      string
		size      int
		expectErr string
	}{
		{
			name: "under limit",
			size: maxBytes - 1,
		},
		{
			name: "at limit",
			size: maxBytes,
		},
		{
			name:      "over limit is rejected",
			size:      maxBytes + 1,
			expectErr: "binary exceeds maximum size",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "otelcol-contrib")
			contents := bytes.Repeat([]byte("a"), tc.size)

			extractor := rawExtractor{maxBytes: maxBytes}
			err := extractor.Extract(t.Context(), contents, "otelcol-contrib", destination)
			if tc.expectErr != "" {
				require.ErrorContains(t, err, tc.expectErr)
				return
			}
			require.NoError(t, err)

			written, err := os.ReadFile(destination)
			require.NoError(t, err)
			assert.Equal(t, contents, written)
		})
	}
}

func TestTarGzipExtractor_Extract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol-contrib")
		contents := []byte("collector binary inside a tarball")
		archive := createTarGzArchive(t, map[string][]byte{
			"README.md":       []byte("not the binary"),
			"otelcol-contrib": contents,
		})

		extractor, err := NewExtractor(FormatTarGzip)
		require.NoError(t, err)

		require.NoError(t, extractor.Extract(t.Context(), archive, "otelcol-contrib", destination))

		written, err := os.ReadFile(destination)
		require.NoError(t, err)
		assert.Equal(t, contents, written)
	})

	t.Run("missing binary name", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol-contrib")
		archive := createTarGzArchive(t, map[string][]byte{"otelcol-contrib": []byte("binary")})

		err := tarGzipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), archive, "", destination)
		require.ErrorContains(t, err, "agent binary name is required")
	})

	t.Run("binary not in archive", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol-contrib")
		archive := createTarGzArchive(t, map[string][]byte{"some-other-file": []byte("binary")})

		err := tarGzipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), archive, "otelcol-contrib", destination)
		require.ErrorContains(t, err, `read tarball looking for "otelcol-contrib"`)
	})

	t.Run("not a gzip archive", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol-contrib")

		err := tarGzipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), []byte("not gzip data"), "otelcol-contrib", destination)
		require.ErrorContains(t, err, "create gzip reader")
	})

	t.Run("binary exceeds max size", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol-contrib")
		archive := createTarGzArchive(t, map[string][]byte{
			"otelcol-contrib": []byte("contents larger than the configured cap"),
		})

		err := tarGzipExtractor{maxBytes: 8}.Extract(t.Context(), archive, "otelcol-contrib", destination)
		require.ErrorContains(t, err, "binary exceeds maximum size of 8 bytes")

		// The size check happens before the destination is opened.
		_, statErr := os.Stat(destination)
		require.True(t, os.IsNotExist(statErr))
	})
}

// createTarGzArchive builds an in-memory gzipped tarball containing the given
// files (name -> contents).
func createTarGzArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)

	for name, contents := range files {
		require.NoError(t, tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(contents)),
		}))
		_, err := tarWriter.Write(contents)
		require.NoError(t, err)
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())

	return buf.Bytes()
}

// This repository's release archives hold the binary in one top directory: a .tar.gz for Linux
// and macOS, a .zip for Windows.
func TestReleaseArchiveLayouts(t *testing.T) {
	contents := []byte("the collector")
	for _, tc := range []struct {
		name    string
		format  Format
		archive func(*testing.T, map[string][]byte) []byte
	}{
		{"tar.gz", FormatTarGzip, createTarGzArchive},
		{"zip", FormatZip, createZipArchive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extractor, err := NewExtractor(tc.format)
			require.NoError(t, err)
			for _, name := range []string{"otelcol", "pydantic-opamp-supervisor_0.2.0_linux_amd64/otelcol", "./otelcol"} {
				destination := filepath.Join(t.TempDir(), "otelcol")
				archive := tc.archive(t, map[string][]byte{"README.md": []byte("not it"), name: contents})
				require.NoError(t, extractor.Extract(t.Context(), archive, "otelcol", destination), name)
				written, err := os.ReadFile(destination)
				require.NoError(t, err)
				assert.Equal(t, contents, written, name)
			}

			// Deeper, or another name: not the binary.
			for _, name := range []string{"a/b/otelcol", "../otelcol", "/otelcol", "dir/otelcol.old", "otelcol-contrib"} {
				archive := tc.archive(t, map[string][]byte{name: contents})
				require.Error(t, extractor.Extract(t.Context(), archive, "otelcol", filepath.Join(t.TempDir(), "otelcol")), name)
			}
		})
	}
}

func TestZipExtractor_Extract(t *testing.T) {
	t.Run("missing binary name", func(t *testing.T) {
		err := zipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), createZipArchive(t, map[string][]byte{"otelcol": nil}), "", filepath.Join(t.TempDir(), "x"))
		require.ErrorContains(t, err, "agent binary name is required")
	})
	t.Run("not a zip archive", func(t *testing.T) {
		err := zipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), []byte("not zip data"), "otelcol", filepath.Join(t.TempDir(), "x"))
		require.ErrorContains(t, err, "read zip archive")
	})
	t.Run("binary exceeds max size", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "otelcol")
		archive := createZipArchive(t, map[string][]byte{"otelcol": []byte("contents larger than the configured cap")})
		err := zipExtractor{maxBytes: 8}.Extract(t.Context(), archive, "otelcol", destination)
		require.ErrorContains(t, err, "binary exceeds maximum size of 8 bytes")
		require.NoFileExists(t, destination)
	})
	t.Run("a symlink", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: "otelcol"}
		h.SetMode(os.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		_, err = w.Write([]byte("/bin/sh"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		err = zipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), buf.Bytes(), "otelcol", filepath.Join(t.TempDir(), "otelcol"))
		require.ErrorContains(t, err, "isn't a regular file")
	})
}

// A tarball's entry named as the binary that is a link, not a file, is refused.
func TestTarGzipExtractorRefusesALink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "otelcol", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh", Mode: 0o777}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	err := tarGzipExtractor{maxBytes: maxAgentBytes}.Extract(t.Context(), buf.Bytes(), "otelcol", filepath.Join(t.TempDir(), "otelcol"))
	require.ErrorContains(t, err, "isn't a regular file")
}

// createZipArchive builds an in-memory zip archive containing the given files (name -> contents).
func createZipArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, contents := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		_, err = w.Write(contents)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
