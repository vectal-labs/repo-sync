package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUpdaterUsesStableHomebrewPathAndWakeSchedule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prefix := filepath.Join(home, "homebrew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.2.3", "repo-sync")
	service := &launchService{domain: "gui/test", label: "test.repo-sync.updates-install"}
	configPath := filepath.Join(home, "settings & notes", "config.json")
	if err := installUpdater(context.Background(), configPath, binary, true, service); err != nil {
		t.Fatal(err)
	}
	path := updaterService(service).plistPath(home)
	args, err := installedArguments(path)
	if err != nil || !slices.Equal(args, []string{filepath.Join(prefix, "bin", "repo-sync"), "update", "--scheduled", "--config", configPath}) {
		t.Fatalf("updater arguments = %v, %v", args, err)
	}
	plist := string(readPreflightFile(t, path))
	if !strings.Contains(plist, "<key>RunAtLoad</key>\n  <true/>") || !strings.Contains(plist, "<key>StartCalendarInterval</key>") || strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Fatalf("updater must catch login/wake without a busy restart loop: %s", plist)
	}
	for _, minute := range []string{"0", "15", "30", "45"} {
		if !strings.Contains(plist, "<key>Minute</key><integer>"+minute+"</integer>") {
			t.Errorf("missing quarter-hour wake schedule %s", minute)
		}
	}
}

func TestStableUpdateBinarySupportsFormulaAndDirectInstallAliases(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"Cellar", "Caskroom"} {
		binary := filepath.Join(root, directory, "repo-sync", "1.2.3", "bin", "repo-sync")
		got, err := stableUpdateBinary(binary)
		if err != nil || got != filepath.Join(root, "bin", "repo-sync") {
			t.Fatalf("stable %s path = %q, %v", directory, got, err)
		}
	}
	binary := filepath.Join(root, "go", "bin", "repo-sync")
	got, err := stableUpdateBinary(binary)
	if err != nil || got != binary {
		t.Fatalf("direct installation path = %q, %v", got, err)
	}
}

func TestInstallUpdaterPreservesExistingRegistrationDuringUpgrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := &lifecycleRunner{}
	service := fakeService(runner)
	binary := filepath.Join(home, "homebrew", "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	configPath := defaultConfigPath()
	if err := writeFileAtomic(updateSettingsPath(), []byte(`{"automatic":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installUpdater(context.Background(), configPath, binary, false, service); err != nil {
		t.Fatal(err)
	}
	path := updaterService(service).plistPath(home)
	before := readPreflightFile(t, path)
	if err := installUpdater(context.Background(), configPath, strings.Replace(binary, "1.0.0", "2.0.0", 1), false, service); err != nil {
		t.Fatal(err)
	}
	if !runner.updaterLoaded || runner.updaterStarts != 1 || string(readPreflightFile(t, path)) != string(before) {
		t.Fatalf("upgrade changed its own updater registration: %+v", runner)
	}
	if string(readPreflightFile(t, updateSettingsPath())) != `{"automatic":false}` {
		t.Fatal("upgrade changed explicit automatic-off preference")
	}
	if err := installUpdater(context.Background(), filepath.Join(home, "different.json"), binary, false, service); err == nil {
		t.Fatal("different config must require setup instead of interrupting an active updater")
	}
	if !runner.updaterLoaded || runner.updaterStarts != 1 || string(readPreflightFile(t, path)) != string(before) {
		t.Fatal("mismatched configuration changed existing updater")
	}
}

func TestSetupRollsBackDaemonAndUpdaterWhenUpdaterStartupFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, repo := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{loaded: true, updaterLoaded: true, failUpdaterStart: true}
	service := setupOwnershipReadyService(t, runner, configPath, func() {})
	oldDaemon := launchAgentPlist("/old/repo-sync", configPath, filepath.Join(home, "logs"))
	oldUpdater := updaterPlist(service.label+".updates", "/old/repo-sync", configPath, filepath.Join(home, "logs"))
	if err := writeFileAtomic(service.plistPath(home), []byte(oldDaemon), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(updaterService(service).plistPath(home), []byte(oldUpdater), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &strings.Builder{}, runner: execCommandRunner{}, service: service})
	if err == nil || !strings.Contains(err.Error(), "updater start refused") || !strings.Contains(err.Error(), "previous setup restored") {
		t.Fatalf("updater startup failure was not rolled back: %v", err)
	}
	if !runner.loaded || !runner.updaterLoaded || runner.starts != 2 || runner.updaterStarts != 2 {
		t.Fatalf("both previous jobs must restart: %+v", runner)
	}
	if string(readPreflightFile(t, service.plistPath(home))) != oldDaemon || string(readPreflightFile(t, updaterService(service).plistPath(home))) != oldUpdater {
		t.Fatal("rollback changed previous service files")
	}
	if _, err := os.Stat(installRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left new installation record: %v", err)
	}
}

func TestUninstallRefusesToInterruptUpdate(t *testing.T) {
	f := newUninstallFixture(t)
	unlock, err := acquireUpdateLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out strings.Builder
	err = runUninstall(context.Background(), f.options(&out))
	if !errors.Is(err, errUpdateBusy) {
		t.Fatalf("expected busy update, got %v", err)
	}
	if len(f.runner.calls) != 0 || !f.runner.loaded || !f.runner.updaterLoaded {
		t.Fatalf("busy update was interrupted: %+v", f.runner)
	}
	uninstallAssertPresent(t, f.installed...)
}

func TestSetupRollbackKeepsUpgradeCompletedDuringPreflight(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, repo := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{loaded: true, failStart: true}
	service := fakeService(runner)
	path := service.plistPath(home)
	if err := writeFileAtomic(path, []byte(launchAgentPlist("/old/repo-sync", configPath, home)), 0o644); err != nil {
		t.Fatal(err)
	}
	upgradedPlist := launchAgentPlist("/upgraded/repo-sync", configPath, home)
	upgraded := false
	gitRunner := preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if !upgraded {
			upgraded = true
			if err := writeFileAtomic(path, []byte(upgradedPlist), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := recordInstallation(configPath, "/upgraded/repo-sync"); err != nil {
				t.Fatal(err)
			}
		}
		return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
	})
	err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &strings.Builder{}, runner: gitRunner, service: service})
	if err == nil || !strings.Contains(err.Error(), "previous setup restored") {
		t.Fatalf("expected startup rollback, got %v", err)
	}
	if string(readPreflightFile(t, path)) != upgradedPlist {
		t.Fatal("rollback restored stale files from before the completed upgrade")
	}
	record, err := loadInstallRecord()
	if err != nil || !slices.Contains(record.BinaryPaths, "/upgraded/repo-sync") {
		t.Fatalf("rollback lost upgraded installation record: %+v, %v", record, err)
	}
}
