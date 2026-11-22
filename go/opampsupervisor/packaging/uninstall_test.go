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

// The uninstaller asks the Supervisor to tell Fleet, through the request file, only when the
// service runs; it prints one line for what came of it, removes both files, and goes on whatever
// it says. --help and --dry-run ask nothing.
func TestUninstallSaysWhetherFleetWasTold(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		for name, c := range map[string]struct {
			notice  string // what the Supervisor writes as it stops ("" for none)
			running bool
			want    string // "" for no line about Fleet
			listed  bool   // the line about the server still listing the host
		}{
			"told":                        {notice: "told", running: true, want: "Fleet was told this host is being removed."},
			"not told":                    {notice: "not told: no answer within 10s", running: true, want: "Fleet was not told this host is being removed: no answer within 10s.", listed: true},
			"the Supervisor says nothing": {running: true, want: "Fleet was not told this host is being removed: the Supervisor did not say, as it stopped.", listed: true},
			"not running":                 {notice: "told", listed: true},
		} {
			t.Run(platform+"/"+name, func(t *testing.T) {
				config, state, tools, invoke := uninstallFixture(t, platform)
				t.Setenv("NOTICE", c.notice)
				// What the Supervisor does as it stops: take the request, write the notice.
				supervisor := `if [ -d "` + state + `/uninstall-requested" ]; then
	rmdir "` + state + `/uninstall-requested"
	[ -z "$NOTICE" ] || printf '%s\n' "$NOTICE" >"` + state + `/uninstall-notice"
fi`
				active := "exit 0"
				if !c.running {
					active = "exit 3"
				}
				gone := filepath.Join(filepath.Dir(state), "stopped")
				launchctl := `echo "launchctl $*" >>"$LOG"
case "$1" in
print) if [ -e "` + gone + `" ]; then exit 113; fi; ` + map[bool]string{true: "exit 0", false: "exit 113"}[c.running] + ` ;;
bootout) ` + supervisor + `
	touch "` + gone + `" ;;
esac`
				systemctl := `echo "systemctl $*" >>"$LOG"
case "$1" in
show) echo loaded ;;
is-active) ` + active + ` ;;
disable) ` + supervisor + ` ;;
esac`
				for name, body := range map[string]string{"systemctl": systemctl, "launchctl": launchctl} {
					if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
						t.Fatal(err)
					}
				}
				if out, err := invoke("--dry-run"); err != nil || strings.Contains(out, "Fleet was") {
					t.Fatalf("dry run: %v: %s", err, out)
				}
				if out, err := invoke("--help"); err != nil || strings.Contains(out, "Fleet was") {
					t.Fatalf("help: %v: %s", err, out)
				}
				if _, err := os.Stat(filepath.Join(state, "uninstall-requested")); !os.IsNotExist(err) {
					t.Fatal("--help or --dry-run asked for the notice")
				}
				out, err := invoke()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				if c.want != "" && !strings.Contains(out, c.want) {
					t.Fatalf("want %q in %s", c.want, out)
				}
				if c.want == "" && strings.Contains(out, "Fleet was") {
					t.Fatalf("a line about Fleet for a service that wasn't running: %s", out)
				}
				if got := strings.Contains(out, "still lists this host"); got != c.listed {
					t.Fatalf("still listed line: %v, want %v: %s", got, c.listed, out)
				}
				for _, f := range []string{"uninstall-requested", "uninstall-notice"} {
					if _, err := os.Stat(filepath.Join(state, f)); !os.IsNotExist(err) {
						t.Fatalf("%s was left behind", f)
					}
				}
				_ = config
			})
		}
	}
}

// The storage directory is the service user's: whatever is planted there, root neither follows,
// truncates, opens nor waits on it, and the removal goes on. A stop that fails leaves no request,
// so a later ordinary stop of the Supervisor doesn't tell the server.
func TestUninstallRequestIsSafeInTheServiceUsersDirectory(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		stubs := func(t *testing.T, tools, stop string) {
			t.Helper()
			for name, body := range map[string]string{
				"systemctl": `echo "systemctl $*" >>"$LOG"; case "$1" in show) echo loaded ;; disable) ` + stop + ` ;; esac`,
				"launchctl": `echo "launchctl $*" >>"$LOG"; case "$1" in print) if [ -e "$GONE" ]; then exit 113; fi; exit 0 ;; bootout) ` + stop + `; touch "$GONE" ;; esac`,
			} {
				if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GONE", filepath.Join(tools, "gone"))
		}
		t.Run(platform+"/a link at the request", func(t *testing.T) {
			_, state, tools, invoke := uninstallFixture(t, platform)
			stubs(t, tools, ":")
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("keep me"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(state, "uninstall-requested")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(state, "uninstall-notice")); err != nil {
				t.Fatal(err)
			}
			out, err := invoke()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if b, err := os.ReadFile(victim); err != nil || string(b) != "keep me" {
				t.Fatalf("the link was followed: %q %v", b, err)
			}
			if strings.Contains(out, "keep me") {
				t.Fatal("a linked notice was read: " + out)
			}
		})
		t.Run(platform+"/a linked notice planted as the service stops", func(t *testing.T) {
			_, state, tools, invoke := uninstallFixture(t, platform)
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("told\n"), 0600); err != nil {
				t.Fatal(err)
			}
			stubs(t, tools, `ln -sf "`+victim+`" "`+state+`/uninstall-notice"`)
			out, err := invoke()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if strings.Contains(out, "Fleet was told") || !strings.Contains(out, "the Supervisor did not say") {
				t.Fatal("a linked notice was read: " + out)
			}
			if b, err := os.ReadFile(victim); err != nil || string(b) != "told\n" {
				t.Fatalf("the link was followed: %q %v", b, err)
			}
		})
		// The notice turns into a FIFO just before it is taken: after any check of its path.
		t.Run(platform+"/a notice swapped for a fifo as it is read", func(t *testing.T) {
			_, state, tools, invoke := uninstallFixture(t, platform)
			stubs(t, tools, `printf 'told\n' >"`+state+`/uninstall-notice"`)
			mv, err := exec.LookPath("mv")
			if err != nil {
				t.Fatal(err)
			}
			swap := `case "$*" in *uninstall-notice*) rm -f "` + state + `/uninstall-notice"; mkfifo "` + state + `/uninstall-notice" ;; esac
exec ` + mv + ` "$@"`
			if err := os.WriteFile(filepath.Join(tools, "mv"), []byte("#!/bin/sh\n"+swap+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			out, err := invoke()
			if err != nil || !strings.Contains(out, "the Supervisor did not say") {
				t.Fatalf("%v: %s", err, out)
			}
			left, err := filepath.Glob(filepath.Join(state, ".uninstall-notice*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("left behind: %v %v", left, err)
			}
		})
		t.Run(platform+"/a fifo at the request", func(t *testing.T) {
			_, state, tools, invoke := uninstallFixture(t, platform)
			stubs(t, tools, ":")
			if err := exec.Command("mkfifo", filepath.Join(state, "uninstall-requested")).Run(); err != nil {
				t.Skip("no mkfifo: ", err)
			}
			if out, err := invoke(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
		t.Run(platform+"/a storage directory that can't be written", func(t *testing.T) {
			if os.Getuid() == 0 {
				t.Skip("root writes anywhere")
			}
			_, state, tools, invoke := uninstallFixture(t, platform)
			stubs(t, tools, ":")
			if err := os.Chmod(state, 0500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(state, 0700)
			out, err := invoke()
			if err != nil || !strings.Contains(out, "Removed the Collector service") {
				t.Fatalf("%v: %s", err, out)
			}
		})
		t.Run(platform+"/a stop that fails leaves no request", func(t *testing.T) {
			_, state, tools, invoke := uninstallFixture(t, platform)
			stubs(t, tools, "exit 1")
			if out, err := invoke(); err == nil {
				t.Fatalf("the stop failed and the removal didn't: %s", out)
			}
			if _, err := os.Lstat(filepath.Join(state, "uninstall-requested")); !os.IsNotExist(err) {
				t.Fatal("the request was left: a later stop would tell the server")
			}
		})
	}
}
