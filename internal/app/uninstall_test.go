package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type uninstallRunner struct {
	label     string
	loaded    bool
	stopError error
	brewError error
	calls     [][]string
	brewPaths []string
}

func (r *uninstallRunner) run(_ context.Context, _, _, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "/bin/launchctl" && len(args) > 0 {
		switch args[0] {
		case "print":
			if r.loaded {
				return "state = waiting\n", nil
			}
			return "Could not find service " + r.label, errors.New("service not found")
		case "bootout":
			if r.stopError != nil {
				return "", r.stopError
			}
			r.loaded = false
			return "", nil
		}
	}
	if filepath.Base(name) == "brew" {
		if r.brewError != nil {
			return "", r.brewError
		}
		for _, path := range r.brewPaths {
			if _, err := os.Lstat(path); err != nil {
				return "", fmt.Errorf("binary removed before package manager: %w", err)
			}
		}
		for _, path := range r.brewPaths {
			if err := os.Remove(path); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected command: %s %v", name, args)
}

type uninstallFixture struct {
	home      string
	binary    string
	config    string
	plist     string
	logs      string
	repo      string
	runner    *uninstallRunner
	service   *launchService
	installed []string
	kept      map[string]string
}

func newUninstallFixture(t *testing.T) uninstallFixture {
	t.Helper()
	binaryData := uninstallBinaryBytes(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := &uninstallRunner{label: "test.repo-sync.uninstall", loaded: true}
	service := &launchService{runner: runner, domain: "gui/test", label: runner.label}
	f := uninstallFixture{
		home: home, binary: filepath.Join(home, "go", "bin", "repo-sync"),
		config: defaultConfigPath(), plist: service.plistPath(home),
		logs: filepath.Join(home, "Library", "Logs", "repo-sync"),
		repo: filepath.Join(home, "projects", "notes"), runner: runner, service: service,
	}
	initRepo(t, f.repo, true)
	f.kept = map[string]string{
		filepath.Join(f.repo, "notes.md"):                 "work in progress\n",
		filepath.Join(home, ".gitconfig"):                 "[user]\n\tname = Keep Me\n",
		filepath.Join(home, ".config", "gh", "hosts.yml"): "github.com: {}\n",
	}
	for path, content := range f.kept {
		uninstallWrite(t, path, content, 0o600)
	}
	gitConfig, err := os.ReadFile(filepath.Join(f.repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	f.kept[filepath.Join(f.repo, ".git", "config")] = string(gitConfig)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: f.repo, Remote: "origin"}}
	if err := writeConfig(f.config, cfg); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, f.binary, string(binaryData), 0o755)
	uninstallWrite(t, f.plist, launchAgentPlist(f.binary, f.config, f.logs), 0o644)
	uninstallWrite(t, statusPath(f.config), "{\"pid\":0}\n", 0o600)
	uninstallWrite(t, filepath.Join(f.logs, "stdout.log"), "ordinary output\n", 0o600)
	uninstallWrite(t, filepath.Join(f.logs, "stderr.log"), "ordinary error\n", 0o600)
	f.installed = []string{f.binary, f.config, f.plist, statusPath(f.config), filepath.Join(f.logs, "stdout.log"), filepath.Join(f.logs, "stderr.log")}
	return f
}

func uninstallWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func uninstallAssertMissing(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected removal of %s; stat error = %v", path, err)
		}
	}
}

func uninstallAssertPresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("expected %s to remain: %v", path, err)
		}
	}
}

func (f uninstallFixture) assertUserFilesPreserved(t *testing.T) {
	t.Helper()
	for path, want := range f.kept {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("user file changed at %s: content=%q error=%v", path, got, err)
		}
	}
}

func (f uninstallFixture) options(out *strings.Builder) uninstallOptions {
	return uninstallOptions{configPath: f.config, binary: f.binary, yes: true, in: strings.NewReader(""), out: out, service: f.service,
		binaryPaths: []string{}, discoverProcesses: func(context.Context, []string) ([]ownedProcess, error) { return nil, nil }, stopProcesses: func(context.Context, []ownedProcess, time.Duration) error { return nil }}

}

func TestUninstallCancelPreservesInstallation(t *testing.T) {
	f := newUninstallFixture(t)
	var out strings.Builder
	opts := f.options(&out)
	opts.yes, opts.in = false, strings.NewReader("no\n")
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertPresent(t, f.installed...)
	f.assertUserFilesPreserved(t)
	if len(f.runner.calls) != 0 || !strings.Contains(out.String(), "Uninstall cancelled.") {
		t.Fatalf("cancellation must make no external changes: calls=%v output=%s", f.runner.calls, out.String())
	}
}

func TestUninstallStandalonePreservesUserFiles(t *testing.T) {
	f := newUninstallFixture(t)
	custom := filepath.Join(f.home, "documents", "sync.json")
	if err := writeConfig(custom, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, statusPath(custom), "{}", 0o600)
	uninstallWrite(t, f.plist, launchAgentPlist(f.binary, custom, f.logs), 0o644)
	sibling := filepath.Join(filepath.Dir(custom), "personal.txt")
	uninstallWrite(t, sibling, "keep this", 0o600)
	f.kept[sibling] = "keep this"
	var out strings.Builder
	if err := runUninstall(context.Background(), f.options(&out)); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, append(f.installed, custom, statusPath(custom))...)
	uninstallAssertMissing(t, filepath.Dir(f.config), f.logs, filepath.Dir(statusPath(f.config)))
	f.assertUserFilesPreserved(t)
	if f.runner.loaded || !strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatalf("uninstall must stop service and confirm completion: loaded=%v output=%s", f.runner.loaded, out.String())
	}
	if !strings.Contains(out.String(), custom) {
		t.Fatalf("preview must include custom config recorded in service: %s", out.String())
	}
}

func TestUninstallPreservesUnknownFilesInAppDirectories(t *testing.T) {
	f := newUninstallFixture(t)
	for _, path := range []string{filepath.Join(filepath.Dir(f.config), "personal.txt"), filepath.Join(f.logs, "personal.log")} {
		uninstallWrite(t, path, "unrelated data", 0o600)
		f.kept[path] = "unrelated data"
	}
	var out strings.Builder
	if err := runUninstall(context.Background(), f.options(&out)); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, f.installed...)
	f.assertUserFilesPreserved(t)
}

func TestUninstallMissingFilesCanBeRepeatedWithKeepBinary(t *testing.T) {
	f := newUninstallFixture(t)
	for _, path := range []string{f.plist, statusPath(f.config), filepath.Join(f.logs, "stdout.log")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	opts := f.options(&out)
	opts.keepBinary = true
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("second uninstall must be harmless: %v", err)
	}
	uninstallAssertPresent(t, f.binary)
	uninstallAssertMissing(t, f.installed[1:]...)
	f.assertUserFilesPreserved(t)
}

func TestUninstallStopFailurePreservesInstallation(t *testing.T) {
	f := newUninstallFixture(t)
	f.runner.stopError = errors.New("permission denied")
	var out strings.Builder
	err := runUninstall(context.Background(), f.options(&out))
	if err == nil || !strings.Contains(err.Error(), "stop service") {
		t.Fatalf("expected actionable stop failure, got %v", err)
	}
	uninstallAssertPresent(t, f.installed...)
	f.assertUserFilesPreserved(t)
	if strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatal("stop failure must not report success")
	}
}

func TestUninstallRemovesFilesCreatedDuringShutdown(t *testing.T) {
	f := newUninstallFixture(t)
	lateCache := filepath.Join(f.home, "Library", "Caches", "repo-sync", "status-"+strings.Repeat("a", 64)+".json")
	lateTemporary := filepath.Join(filepath.Dir(f.config), ".repo-sync-987654")
	var out strings.Builder
	opts := f.options(&out)
	opts.stopProcesses = func(context.Context, []ownedProcess, time.Duration) error {
		uninstallWrite(t, lateCache, "{}", 0o600)
		uninstallWrite(t, lateTemporary, "partial write", 0o600)
		return nil
	}
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, append(f.installed, lateCache, lateTemporary, installRecordPath())...)
	f.assertUserFilesPreserved(t)
}

func TestUninstallHomebrewMustActuallyRemoveProgram(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	brew := filepath.Join(prefix, "bin", "brew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	uninstallWrite(t, brew, "fake brew; never execute", 0o755)
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	// No brewPaths: this package manager claims success without removing files.
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = binary
	if err := runUninstall(context.Background(), opts); err == nil {
		t.Fatal("package manager success must be checked against remaining files")
	}
	uninstallAssertPresent(t, binary, installRecordPath())
	if strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatal("leftover binaries must not report successful removal")
	}
}

func TestUninstallRefusesSymlinkedApplicationDirectory(t *testing.T) {
	f := newUninstallFixture(t)
	appDir := filepath.Dir(f.config)
	outside := filepath.Join(f.home, "user-data")
	if err := os.Rename(appDir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, appDir); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runUninstall(context.Background(), f.options(&out))
	if err == nil || !strings.Contains(err.Error(), "symlinked directory") {
		t.Fatalf("expected refusal to follow app directory symlink, got %v", err)
	}
	uninstallAssertPresent(t, f.installed...)
	uninstallAssertPresent(t, filepath.Join(outside, "config.json"))
	f.assertUserFilesPreserved(t)
	if len(f.runner.calls) != 0 {
		t.Fatalf("unsafe cleanup must be rejected before service changes: %v", f.runner.calls)
	}
}

func TestUninstallStandaloneSymlinkRemovesLinkAndExecutable(t *testing.T) {
	f := newUninstallFixture(t)
	link := filepath.Join(f.home, "bin", "repo-sync")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.binary, link); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = link
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, append(f.installed, link)...)
	f.assertUserFilesPreserved(t)
}

func TestUninstallStandaloneThroughParentAlias(t *testing.T) {
	f := newUninstallFixture(t)
	alias := filepath.Join(f.home, "tools")
	if err := os.Symlink(filepath.Dir(f.binary), alias); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = filepath.Join(alias, "repo-sync")
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("a symlinked parent must not cause the binary to be removed twice: %v", err)
	}
	uninstallAssertMissing(t, f.installed...)
	uninstallAssertPresent(t, alias)
	f.assertUserFilesPreserved(t)
}

func TestUninstallDoesNotTrustStatusPIDToIdentifyProcesses(t *testing.T) {
	f := newUninstallFixture(t)
	f.runner.loaded = false
	status, err := json.Marshal(serviceStatus{PID: os.Getpid(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, statusPath(f.config), string(status), 0o600)
	var out strings.Builder
	opts := f.options(&out)
	// The actual process scanner must reject this PID: it is a Go test process,
	// and its executable is not one of this installation's verified binaries.
	opts.discoverProcesses = discoverRepoSyncProcesses
	opts.stopProcesses = stopRepoSyncProcesses
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f.assertUserFilesPreserved(t)
	uninstallAssertMissing(t, f.installed...)
}

func TestUninstallHomebrewUsesPackageManager(t *testing.T) {
	for _, test := range []struct{ directory, kind string }{{"Caskroom", "--cask"}, {"Cellar", "--formula"}} {
		t.Run(test.directory, func(t *testing.T) {
			f := newUninstallFixture(t)
			prefix := filepath.Join(f.home, "homebrew")
			brew := filepath.Join(prefix, "bin", "brew")
			binary := filepath.Join(prefix, test.directory, "repo-sync", "1.0.0", "repo-sync")
			uninstallWrite(t, brew, "fake brew; never execute", 0o755)
			uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
			link := filepath.Join(prefix, "bin", "repo-sync")
			if err := os.Symlink(binary, link); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			opts := f.options(&out)
			opts.binary = link
			f.runner.brewPaths = []string{binary, link}
			if err := runUninstall(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			resolvedBrew, err := filepath.EvalSymlinks(brew)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{resolvedBrew, "uninstall", test.kind}
			if test.kind == "--formula" {
				want = append(want, "--force")
			}
			want = append(want, "repo-sync")
			found := false
			for _, call := range f.runner.calls {
				if reflect.DeepEqual(call, want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("Homebrew must own binary removal: calls=%v want=%v", f.runner.calls, want)
			}
			uninstallAssertMissing(t, f.installed[1:]...)
			// The fake package manager asserts that both files still existed when
			// it was invoked, then removes them. Cleanup verifies its result.
			uninstallAssertMissing(t, binary, link)
			uninstallAssertPresent(t, brew)
			f.assertUserFilesPreserved(t)
		})
	}
}

func TestUninstallHomebrewFailureReportsRemainingProgram(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	brew := filepath.Join(prefix, "bin", "brew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	uninstallWrite(t, brew, "fake brew; never execute", 0o755)
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	f.runner.brewError = errors.New("package database is locked")
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = binary
	err := runUninstall(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "Homebrew removal failed") || !strings.Contains(err.Error(), brew+" uninstall --cask repo-sync") {
		t.Fatalf("package removal failure must include recovery command, got %v", err)
	}
	uninstallAssertMissing(t, f.installed[1:]...)
	uninstallAssertPresent(t, binary)
	f.assertUserFilesPreserved(t)
	if strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatal("partial removal must not report complete success")
	}
}

func TestUninstallMissingHomebrewPreservesInstallation(t *testing.T) {
	f := newUninstallFixture(t)
	binary := filepath.Join(f.home, "homebrew", "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = binary
	err := runUninstall(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "repair Homebrew or use --keep-binary") {
		t.Fatalf("missing package manager must have an actionable error, got %v", err)
	}
	uninstallAssertPresent(t, append(f.installed, binary)...)
	if len(f.runner.calls) != 0 {
		t.Fatalf("must not partially remove installation before validating package manager: %v", f.runner.calls)
	}
}

func TestUninstallRefusesUnverifiedInstalledCustomConfig(t *testing.T) {
	f := newUninstallFixture(t)
	custom := filepath.Join(f.home, "documents", "unrelated.json")
	uninstallWrite(t, custom, "{\"personal\":true}", 0o600)
	uninstallWrite(t, f.plist, launchAgentPlist(f.binary, custom, f.logs), 0o644)
	var out strings.Builder
	err := runUninstall(context.Background(), f.options(&out))
	if err == nil || !strings.Contains(err.Error(), "cannot verify custom config") {
		t.Fatalf("must refuse unverified custom file recorded in service, got %v", err)
	}
	uninstallAssertPresent(t, append(f.installed, custom)...)
	if len(f.runner.calls) != 0 {
		t.Fatalf("unverified config must be rejected before service changes: %v", f.runner.calls)
	}
}

func TestUninstallCustomConfigOutsideHomeThroughMacOSAlias(t *testing.T) {
	f := newUninstallFixture(t)
	custom := filepath.Join(t.TempDir(), "custom.json")
	if err := writeConfig(custom, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	opts := f.options(&out)
	opts.configPath = custom
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("custom config outside HOME: %v", err)
	}
	uninstallAssertMissing(t, custom)
	f.assertUserFilesPreserved(t)
}

var uninstallBuildOnce sync.Once
var uninstallBuiltBytes []byte
var uninstallBuildErr error
var uninstallBuildEnvironment = os.Environ()

func uninstallBinaryBytes(t *testing.T) []byte {
	t.Helper()
	uninstallBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "repo-sync-uninstall-test-")
		if err != nil {
			uninstallBuildErr = err
			return
		}
		defer os.RemoveAll(dir)
		binary := filepath.Join(dir, "repo-sync")
		root, err := filepath.Abs("../..")
		if err != nil {
			uninstallBuildErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", binary, ".")
		cmd.Dir = root
		cmd.Env = uninstallBuildEnvironment
		if output, err := cmd.CombinedOutput(); err != nil {
			uninstallBuildErr = fmt.Errorf("build test executable: %w: %s", err, output)
			return
		}
		uninstallBuiltBytes, uninstallBuildErr = os.ReadFile(binary)
	})
	if uninstallBuildErr != nil {
		t.Fatal(uninstallBuildErr)
	}
	return uninstallBuiltBytes
}
