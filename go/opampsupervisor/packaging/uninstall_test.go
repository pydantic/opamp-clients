// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func uninstallFixture(t *testing.T, platform string) (string, string, string, func(...string) (string, error)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper")
	}
	root, tools := t.TempDir(), t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = resolved
	config, state := "etc/pydantic-opamp", "var/lib/pydantic-opamp"
	if platform == "Darwin" {
		config, state = "opt/pydantic-opamp/etc", "opt/pydantic-opamp/var"
	}
	config, state = filepath.Join(root, config), filepath.Join(root, state)
	for _, dir := range []string{config, state, filepath.Join(root, "var/lib/pydantic-otelcol"), filepath.Join(root, "run/systemd/system")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("private-data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"launchctl":  `echo "launchctl $*" >>"$LOG"; if [ "$1" = print ]; then exit 113; fi`,
		"pkgutil":    `echo "pkgutil $*" >>"$LOG"`,
		"security":   `echo "security $*" >>"$LOG"; exit 44`,
		"dpkg-query": `echo installed`,
		"dpkg":       `echo "dpkg $*" >>"$LOG"`,
		"systemctl":  `echo "systemctl $*" >>"$LOG"; if [ "$1" = show ]; then echo loaded; fi`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "calls")
	invoke := func(args ...string) (string, error) {
		script := "./pydantic-opamp-uninstall"
		recovery := filepath.Join(config, "uninstall")
		if _, err := os.Stat(recovery); err == nil {
			script = recovery
		}
		cmd := exec.Command(script, args...)
		cmd.Env = append(os.Environ(), "OPAMP_UNINSTALL_ROOT="+root, "OPAMP_UNINSTALL_OS="+platform, "PATH="+tools+":"+os.Getenv("PATH"), "LOG="+log)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return config, state, tools, invoke
}

func TestUninstallRetainsDataAndCanPurgeLater(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Run(platform, func(t *testing.T) {
			config, state, _, invoke := uninstallFixture(t, platform)
			if err := os.Chmod(config, 0750); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				out, err := invoke()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				// Fleet e2e 2026-10-28: the default run said only what pkgutil or dpkg printed.
				if !strings.Contains(out, "Kept "+config) || !strings.Contains(out, "run: sudo "+filepath.Join(config, "uninstall")+" --purge") {
					t.Fatalf("no summary of what was kept: %s", out)
				}
				for _, path := range []string{filepath.Join(config, "keep"), filepath.Join(state, "keep"), filepath.Join(config, "uninstall")} {
					if _, err := os.Stat(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			info, err := os.Stat(config)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0750 {
				t.Fatal("removal changed configuration access for reinstall")
			}
			for range 2 {
				if out, err := invoke("--purge"); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				for _, path := range []string{config, state} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("purge left %s: %v", path, err)
					}
				}
				if platform == "Darwin" {
					// The empty /opt/pydantic-opamp goes too.
					if _, err := os.Stat(filepath.Dir(config)); !os.IsNotExist(err) {
						t.Fatalf("purge left %s: %v", filepath.Dir(config), err)
					}
				}
			}
		})
	}
}

func TestUninstallDryRunDoesNotRunNativeCommands(t *testing.T) {
	config, state, _, invoke := uninstallFixture(t, "Darwin")
	if out, err := invoke("--purge", "--dry-run"); err != nil || !strings.Contains(out, "Delete") {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(config, "uninstall")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote helper")
	}
	if _, err := os.Stat(filepath.Join(state, "keep")); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallServiceFailurePreservesData(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		config, state, tools, invoke := uninstallFixture(t, platform)
		program := "systemctl"
		if platform == "Darwin" {
			program = "launchctl"
		}
		if err := os.WriteFile(filepath.Join(tools, program), []byte("#!/bin/sh\nexit 9\n"), 0755); err != nil {
			t.Fatal(err)
		}
		if out, err := invoke("--purge"); err == nil {
			t.Fatalf("expected service failure: %s", out)
		}
		for _, path := range []string{filepath.Join(config, "keep"), filepath.Join(state, "keep")} {
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestUninstallRefusesRedirectedParents(t *testing.T) {
	config, _, _, invoke := uninstallFixture(t, "Darwin")
	outside := t.TempDir()
	if err := os.RemoveAll(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, config); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("--purge"); err == nil || !strings.Contains(out, "symbolic link") {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallKeychainFailureKeepsSealedState(t *testing.T) {
	config, state, tools, invoke := uninstallFixture(t, "Darwin")
	if err := os.WriteFile(filepath.Join(tools, "security"), []byte("#!/bin/sh\nexit 36\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := invoke("--purge")
	if err == nil || !strings.Contains(out, "state key") {
		t.Fatalf("%v: %s", err, out)
	}
	for _, path := range []string{filepath.Join(config, "keep"), filepath.Join(state, "keep")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUninstallPackageRemovalFailureKeepsState(t *testing.T) {
	config, state, tools, invoke := uninstallFixture(t, "Linux")
	if err := os.WriteFile(filepath.Join(tools, "dpkg"), []byte("#!/bin/sh\nexit 9\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("--purge"); err == nil {
		t.Fatal("package failure was ignored: ", out)
	}
	for _, path := range []string{filepath.Join(config, "keep"), filepath.Join(state, "keep")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUninstallUsesInstalledRpmDatabaseWithoutRepositoryTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper")
	}
	root, tools := t.TempDir(), t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = resolved
	for _, program := range []string{"dirname", "mkdir", "install", "rm"} {
		source, err := exec.LookPath(program)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(tools, program)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "calls")
	body := "#!/bin/sh\nif [ \"$1\" = -q ]; then exit 0; fi\necho \"$*\" >\"$LOG\"\n"
	if err := os.WriteFile(filepath.Join(tools, "rpm"), []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "pydantic-opamp-uninstall", "--purge")
	command.Env = append(os.Environ(), "PATH="+tools, "LOG="+log, "OPAMP_UNINSTALL_OS=Linux", "OPAMP_UNINSTALL_ROOT="+root)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "--erase pydantic-opamp-supervisor\n" {
		t.Fatalf("wrong removal: %s", calls)
	}
}

func TestUninstallWaitsForTheMacServiceToUnload(t *testing.T) {
	_, _, tools, invoke := uninstallFixture(t, "Darwin")
	script := `#!/bin/sh
 echo "$*" >>"$LOG"
 if [ "$1" = print ]; then
  if [ -f "$LOG.stopped" ]; then exit 113; else exit 0; fi
 fi
 if [ "$1" = bootout ]; then : >"$LOG.stopped"; fi
 `
	if err := os.WriteFile(filepath.Join(tools, "launchctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("--purge"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

// A purge takes what enrolling with a telemetry token put outside the package's directories: the
// unit's drop-in names a credential that goes with the configuration, and the next start failed on
// it (243/CREDENTIALS). Another drop-in of the unit's stays, and without --purge both do.
func TestUninstallPurgeRemovesTheTelemetryDropIn(t *testing.T) {
	config, _, _, invoke := uninstallFixture(t, "Linux")
	root := filepath.Dir(filepath.Dir(config))
	dropins := filepath.Join(root, "etc/systemd/system/pydantic-opamp-supervisor.service.d")
	telemetry, other := filepath.Join(dropins, "telemetry.conf"), filepath.Join(dropins, "operator.conf")
	write := func(paths ...string) {
		if err := os.MkdirAll(dropins, 0755); err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			content := "[Service]\n"
			if path == telemetry {
				content += "LoadCredentialEncrypted=opamp-telemetry-token:/etc/pydantic-opamp/telemetry-token.cred\n"
			}
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	there := func(path string) bool { _, err := os.Stat(path); return err == nil }
	reloads := func() int {
		log, _ := os.ReadFile(filepath.Join(root, "calls"))
		return strings.Count(string(log), "systemctl daemon-reload")
	}
	write(telemetry, other)
	if out, err := invoke(); err != nil || !there(telemetry) || !there(other) {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := invoke("--purge"); err != nil || there(telemetry) || !there(other) || reloads() != 1 {
		t.Fatalf("telemetry.conf left %v, operator.conf left %v, reloads %d: %v: %s", there(telemetry), there(other), reloads(), err, out)
	}
	// Alone, its directory goes too; with nothing there, nothing is done.
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	write(telemetry)
	if out, err := invoke("--purge"); err != nil || there(dropins) || reloads() != 2 {
		t.Fatalf("%s left %v, reloads %d: %v: %s", dropins, there(dropins), reloads(), err, out)
	}
	if out, err := invoke("--purge"); err != nil || reloads() != 2 {
		t.Fatalf("reloads %d: %v: %s", reloads(), err, out)
	}
	// What an administrator wrote there, or added to, stays, and is said; --dry-run names what goes.
	write(telemetry)
	if out, err := invoke("--purge", "--dry-run"); err != nil || !strings.Contains(out, "Delete "+telemetry+".") {
		t.Fatalf("%v: %s", err, out)
	}
	for _, content := range []string{"[Service]\nEnvironment=HTTPS_PROXY=http://proxy:3128\n", "[Service]\nLoadCredentialEncrypted=opamp-telemetry-token:/etc/pydantic-opamp/telemetry-token.cred\nEnvironment=A=b\n"} {
		if err := os.WriteFile(telemetry, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if out, err := invoke("--purge", "--dry-run"); err != nil || !strings.Contains(out, "Keep "+telemetry) {
			t.Fatalf("%v: %s", err, out)
		}
		if out, err := invoke("--purge"); err != nil || !there(telemetry) || !strings.Contains(out, "isn't as this package writes it, and is kept") || reloads() != 2 {
			t.Fatalf("reloads %d: %v: %s", reloads(), err, out)
		}
	}
	// Nor is a link followed, the directory's or the file's.
	elsewhere := t.TempDir()
	kept := filepath.Join(elsewhere, "telemetry.conf")
	write(telemetry)
	if err := os.Rename(telemetry, kept); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(kept, telemetry); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("--purge"); err != nil || !there(telemetry) || !there(kept) {
		t.Fatalf("%v: %s", err, out)
	}
	if err := os.RemoveAll(dropins); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dropins); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("--purge"); err == nil || !strings.Contains(out, "refusing symbolic link") || !there(kept) {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := invoke(); err != nil || !there(kept) {
		t.Fatalf("without --purge: %v: %s", err, out)
	}
}
