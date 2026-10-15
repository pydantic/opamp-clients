// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build repo

// The apt and yum repositories repo.sh makes from the packages build.sh made, with the key both
// were given: apt and dnf install from them with that key alone, and refuse them when they're
// changed or signed by another. Needs gpg, apt-utils, createrepo_c, rpm and docker (dnf runs in
// an AlmaLinux 9 container, systemd 252 and rpm 4.16), not root:
//
//	SIGNING_KEY_FILE=key.asc ./build.sh && SIGNING_KEY_FILE=key.asc go test -tags=repo -run Repo .

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pkg = "pydantic-opamp-supervisor"

func command(t *testing.T, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func must(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	out, err := command(t, env, name, args...)
	require.NoError(t, err, "%s %v: %s", name, args, out)
	return out
}

// makeRepo runs repo.sh into dir, signing with key, for serving from dir itself.
func makeRepo(t *testing.T, dir, key string) { makeRepoAt(t, dir, "file://"+dir, key) }

func makeRepoAt(t *testing.T, dir, url, key string) {
	t.Helper()
	must(t, []string{"REPO_DIR=" + dir, "REPO_URL=" + url, "SIGNING_KEY_FILE=" + key}, "./repo.sh")
}

// newKey makes another signing key, armored, in dir.
func newKey(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Join(dir, "gnupg")
	require.NoError(t, os.Mkdir(home, 0o700))
	env := []string{"GNUPGHOME=" + home}
	must(t, env, "gpg", "--batch", "--passphrase", "", "--quick-gen-key", "Another <another@invalid>", "rsa3072", "sign", "never")
	key := filepath.Join(dir, "another.asc")
	require.NoError(t, os.WriteFile(key, []byte(must(t, env, "gpg", "--batch", "--armor", "--export-secret-keys")), 0o600))
	return key
}

// apt is apt-get or apt-cache against repo alone, with a state of its own (also where downloads
// land): repo's pydantic-opamp.sources, its Signed-By pointed at keyring, and no other trusted key.
func apt(t *testing.T, repo, keyring string) (run func(name string, args ...string) (string, error), root string) {
	t.Helper()
	root = t.TempDir()
	sources := filepath.Join(root, "sources.list.d")
	for _, dir := range []string{sources, filepath.Join(root, "trusted.gpg.d"), filepath.Join(root, "lists", "partial"),
		filepath.Join(root, "cache", "archives", "partial")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	src, err := os.ReadFile(filepath.Join(repo, "pydantic-opamp.sources"))
	require.NoError(t, err)
	require.Contains(t, string(src), "Signed-By: /etc/apt/keyrings/pydantic-opamp.gpg\n")
	src = []byte(strings.Replace(string(src), "/etc/apt/keyrings/pydantic-opamp.gpg", keyring, 1))
	require.NoError(t, os.WriteFile(filepath.Join(sources, "pydantic-opamp.sources"), src, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "status"), nil, 0o644))
	opts := []string{
		"-o", "Dir::Etc::SourceList=/dev/null", "-o", "Dir::Etc::SourceParts=" + sources,
		"-o", "Dir::Etc::Trusted=/dev/null", "-o", "Dir::Etc::TrustedParts=" + filepath.Join(root, "trusted.gpg.d"),
		"-o", "Dir::State::Lists=" + filepath.Join(root, "lists"), "-o", "Dir::State::status=" + filepath.Join(root, "status"),
		"-o", "Dir::Cache=" + filepath.Join(root, "cache"), "-o", "Debug::NoLocking=1",
		"-o", "APT::Architecture=amd64", "-o", "APT::Sandbox::User=" + os.Getenv("USER"),
	}
	return func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, append(opts, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}, root
}

func signingKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("SIGNING_KEY_FILE")
	require.NotEmpty(t, key, "SIGNING_KEY_FILE names the key build.sh signed the rpm with")
	return key
}

func TestRepoAptInstallsWithTheKey(t *testing.T) {
	repo := t.TempDir()
	// An earlier release, already published: adding this one keeps it. (Lower than any version
	// build.sh is given: 0.0.1 outranked CI's 0.0.0~ci, and apt fetched it as the newest.)
	old := filepath.Join(t.TempDir(), "old")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "DEBIAN"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "DEBIAN", "control"), []byte("Package: "+pkg+
		"\nVersion: 0~earlier\nArchitecture: amd64\nMaintainer: Pydantic <hello@pydantic.dev>\nDescription: an earlier release\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "deb", "pool", "main"), 0o755))
	must(t, nil, "dpkg-deb", "--root-owner-group", "--build", old, filepath.Join(repo, "deb", "pool", "main", pkg+"_0~earlier_amd64.deb"))
	makeRepo(t, repo, signingKey(t))

	get, root := apt(t, repo, filepath.Join(repo, "pydantic-opamp.gpg"))
	out, err := get("apt-get", "update")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "W:", "apt warned about the repository")
	versions, err := get("apt-cache", "madison", pkg)
	require.NoError(t, err, versions)
	assert.Contains(t, versions, " 0~earlier ")
	built, err := filepath.Glob("dist/" + pkg + "_*_amd64.deb")
	require.NoError(t, err)
	require.Len(t, built, 1)
	version := strings.Split(filepath.Base(built[0]), "_")[1]
	assert.Contains(t, versions, " "+version+" ")
	// The newest is the candidate, and its file arrives with the hash the signed index has.
	out, err = get("apt-get", "download", pkg)
	require.NoError(t, err, out)
	got, err := filepath.Glob(filepath.Join(root, pkg+"_*_amd64.deb"))
	require.NoError(t, err)
	require.Len(t, got, 1, out)
	assert.Contains(t, got[0], version)
}

// As the README runs it: relative paths are the caller's, not repo.sh's directory.
func TestRepoTakesPathsFromTheCallersDirectory(t *testing.T) {
	dir := t.TempDir()
	key, err := os.ReadFile(signingKey(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "key.asc"), key, 0o600))
	script, err := filepath.Abs("repo.sh")
	require.NoError(t, err)
	cmd := exec.Command(script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "REPO_DIR=public", "REPO_URL=https://packages.example.com", "SIGNING_KEY_FILE=key.asc")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.FileExists(t, filepath.Join(dir, "public", "deb", "dists", "stable", "InRelease"))
	assert.FileExists(t, filepath.Join(dir, "public", "pydantic-opamp.repo"))
}

func TestRepoAptRefusesAChangedIndex(t *testing.T) {
	repo := t.TempDir()
	makeRepo(t, repo, signingKey(t))
	packages := filepath.Join(repo, "deb", "dists", "stable", "main", "binary-amd64", "Packages")
	index, err := os.ReadFile(packages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(packages, []byte(strings.Replace(string(index), "SHA256: ", "SHA256: 00", 1)), 0o644))
	require.NoError(t, os.Remove(packages+".gz"))
	require.NoError(t, exec.Command("gzip", "-9nk", packages).Run())

	get, _ := apt(t, repo, filepath.Join(repo, "pydantic-opamp.gpg"))
	out, err := get("apt-get", "update")
	assert.Error(t, err, "apt took the changed index: %s", out)
	assert.Contains(t, out, "Hash Sum mismatch")
}

func TestRepoAptRefusesAnotherKey(t *testing.T) {
	repo := t.TempDir()
	makeRepo(t, repo, newKey(t, t.TempDir()))
	theirs := filepath.Join(t.TempDir(), "theirs.gpg")
	ours := t.TempDir()
	makeRepo(t, ours, signingKey(t))
	data, err := os.ReadFile(filepath.Join(ours, "pydantic-opamp.gpg"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(theirs, data, 0o644))

	get, _ := apt(t, repo, theirs)
	out, err := get("apt-get", "update")
	assert.Error(t, err, "apt took a repository signed by another key: %s", out)
	assert.Contains(t, out, "NO_PUBKEY")
}

func TestRepoRpmIsSignedWithTheKey(t *testing.T) {
	repo := t.TempDir()
	makeRepo(t, repo, signingKey(t))
	rpms, err := filepath.Glob(filepath.Join(repo, "rpm", pkg+"-*.rpm"))
	require.NoError(t, err)
	require.Len(t, rpms, 1)

	db := t.TempDir()
	out, err := command(t, nil, "rpmkeys", "--dbpath", db, "--checksig", rpms[0])
	assert.Error(t, err, "rpm checked the signature without the key: %s", out)
	must(t, nil, "rpmkeys", "--dbpath", db, "--import", filepath.Join(repo, "pydantic-opamp.asc"))
	out = must(t, nil, "rpmkeys", "--dbpath", db, "--checksig", rpms[0])
	assert.Contains(t, out, "signatures OK")

	keyring := filepath.Join(repo, "pydantic-opamp.gpg")
	repomd := filepath.Join(repo, "rpm", "repodata", "repomd.xml")
	must(t, nil, "gpgv", "--keyring", keyring, repomd+".asc", repomd)
	data, err := os.ReadFile(repomd)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(repomd, append(data, '\n'), 0o644))
	out, err = command(t, nil, "gpgv", "--keyring", keyring, repomd+".asc", repomd)
	assert.Error(t, err, "a changed repomd.xml passed: %s", out)

	repoFile, err := os.ReadFile(filepath.Join(repo, "pydantic-opamp.repo"))
	require.NoError(t, err)
	for _, line := range []string{"baseurl=file://" + repo + "/rpm", "gpgcheck=1", "repo_gpgcheck=1", "gpgkey=file://" + repo + "/pydantic-opamp.asc"} {
		assert.Contains(t, string(repoFile), line+"\n")
	}
}

// almaLinux is AlmaLinux 9 with systemd, which the package depends on, pinned.
const almaLinux = "almalinux/9-init@sha256:85b089b501411649d76607b81c55fb18044493d0fed589f53bb2fad3e7307bb2"

// dnf installs the package from the repository a user would: repo.sh's pydantic-opamp.repo, its
// key fetched and imported by dnf. Signed by another key, with ours as gpgkey, it is refused.
func TestRepoDnfInstallsWithTheKey(t *testing.T) {
	ours, theirs := t.TempDir(), t.TempDir()
	makeRepoAt(t, ours, "file:///ours", signingKey(t))
	makeRepoAt(t, theirs, "file:///theirs", newKey(t, t.TempDir()))
	data, err := os.ReadFile(filepath.Join(ours, "pydantic-opamp.asc"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(theirs, "pydantic-opamp.asc"), data, 0o644))
	repoFile := filepath.Join(theirs, "pydantic-opamp.repo")
	data, err = os.ReadFile(repoFile)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), "file:///theirs/pydantic-opamp.asc", "file:///ours/pydantic-opamp.asc", 1))
	require.NoError(t, os.WriteFile(repoFile, data, 0o644))

	// Only our repository: the container has no network to need.
	script := `install() {
	cp "/$1/pydantic-opamp.repo" /etc/yum.repos.d/
	dnf install --assumeyes --disablerepo='*' --enablerepo=pydantic-opamp ` + pkg + `
	status=$?
	dnf clean all >/dev/null
	return $status
}
if install theirs; then exit 1; fi
install ours && rpm -q ` + pkg
	out := must(t, nil, "docker", "run", "--rm", "--network=none", "-v", ours+":/ours:ro", "-v", theirs+":/theirs:ro",
		almaLinux, "sh", "-c", script)
	assert.Contains(t, out, "repomd.xml GPG signature verification error", "their repository failed for another reason")
	assert.Contains(t, out, "Importing GPG key")
	assert.Contains(t, out, "Complete!")
}
