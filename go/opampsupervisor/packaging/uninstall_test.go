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
				if out, err := invoke(); err != nil {
					t.Fatalf("%v: %s", err, out)
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
