package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolatedLeftoverOptions(f uninstallFixture, out *strings.Builder) uninstallOptions {
	opts := f.options(out)
	opts.binaryPaths = []string{}
	opts.discoverProcesses = func(context.Context, []string) ([]ownedProcess, error) { return nil, nil }
	opts.stopProcesses = func(_ context.Context, processes []ownedProcess, _ time.Duration) error {
		if len(processes) != 0 {
			return fmt.Errorf("test must not stop real processes")
		}
		return nil
	}
	return opts
}

func writeUninstallRecord(t *testing.T, cfgs, binaries []string) {
	t.Helper()
	data, err := json.Marshal(installRecord{Version: 1, ConfigPaths: cfgs, BinaryPaths: binaries})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(installRecordPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertUninstallReport(t *testing.T, output, heading string, paths ...string) {
	t.Helper()
	var section []string
	inSection := false
	foundHeading := false
	for _, line := range strings.Split(output, "\n") {
		label := strings.TrimSpace(line)
		if label == "Removed:" || label == "Preserved:" || label == "Failed:" {
			inSection = label == heading+":"
			foundHeading = foundHeading || inSection
			continue
		}
		if inSection {
			section = append(section, line)
		}
	}
	if !foundHeading {
		t.Fatalf("missing %s report:\n%s", heading, output)
	}
	for _, path := range paths {
		if !strings.Contains(strings.Join(section, "\n"), path) {
			t.Errorf("%s report does not contain %s:\n%s", heading, path, output)
		}
	}
}

func TestUninstallRemovesStaleStatusCache(t *testing.T) {
	f := newUninstallFixture(t)
	stale := filepath.Join(filepath.Dir(statusPath(f.config)), "status-"+strings.Repeat("a", 64)+".json")
	uninstallWrite(t, stale, "{\"pid\":0}\n", 0o600)
	var out strings.Builder
	opts := isolatedLeftoverOptions(f, &out)
	opts.keepBinary = true
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, stale, filepath.Dir(stale))
	f.assertUserFilesPreserved(t)
	assertUninstallReport(t, out.String(), "Removed", stale)
	assertUninstallReport(t, out.String(), "Preserved", f.binary)
	assertUninstallReport(t, out.String(), "Failed")
}

func TestUninstallRemovesOwnedLeftoversAndReportsPersonalFiles(t *testing.T) {
	f := newUninstallFixture(t)
	cache := filepath.Dir(statusPath(f.config))
	custom := filepath.Join(f.home, "settings", "old-sync.json")
	if err := writeConfig(custom, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	writeUninstallRecord(t, []string{f.config, custom}, []string{f.binary})
	owned := []string{
		updateSettingsPath(),
		updateStatePath(f.config),
		filepath.Join(cache, "updates-"+strings.Repeat("a", 64)+".json"),
		filepath.Join(cache, "sync.lock"),
		filepath.Join(cache, "update.lock"),
		filepath.Join(f.logs, "updates-stdout.log"),
		filepath.Join(f.logs, "updates-stderr.log.2.gz"),
		filepath.Join(cache, "status-"+strings.Repeat("b", 64)+".json"),
		filepath.Join(filepath.Dir(f.config), ".repo-sync-12345"),
		filepath.Join(cache, ".repo-sync-45678"),
		filepath.Join(filepath.Dir(custom), ".repo-sync-98765"),
		filepath.Join(filepath.Dir(f.plist), ".repo-sync-23456"),
		filepath.Join(f.logs, "stdout.log.1"),
		filepath.Join(f.logs, "stderr.log.2.gz"),
	}
	for _, path := range owned {
		uninstallWrite(t, path, "old repo-sync state", 0o600)
	}
	personal := []string{
		filepath.Join(cache, "updates-personal.json"),
		filepath.Join(f.logs, "personal.log"),
		filepath.Join(f.logs, "stdout.log.backup"),
		filepath.Join(f.logs, "stderr.log.2.gz.notes"),
		filepath.Join(f.logs, ".repo-sync-1234"),
		filepath.Join(cache, "status-"+strings.Repeat("c", 63)+".json"),
		filepath.Join(cache, "status-personal.json"),
		filepath.Join(cache, ".repo-sync-personal"),
		filepath.Join(filepath.Dir(custom), ".repo-sync-12345.notes"),
		filepath.Join(filepath.Dir(custom), "personal.txt"),
		filepath.Join(f.repo, ".repo-sync-12345"),
	}
	for _, path := range personal {
		uninstallWrite(t, path, "keep this exact text", 0o600)
		f.kept[path] = "keep this exact text"
	}
	var out strings.Builder
	opts := isolatedLeftoverOptions(f, &out)
	opts.keepBinary = true
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, append(owned, custom)...)
	uninstallAssertPresent(t, installRecordPath())
	f.assertUserFilesPreserved(t)
	assertUninstallReport(t, out.String(), "Removed", append(owned, custom)...)
	// Unknown files in app-owned folders are reported. A custom config may live
	// directly in HOME, so its unrelated siblings do not need to be enumerated.
	assertUninstallReport(t, out.String(), "Preserved", personal[:8]...)
	assertUninstallReport(t, out.String(), "Preserved", installRecordPath())
	assertUninstallReport(t, out.String(), "Failed")
	if !strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatalf("successful cleanup did not confirm completion:\n%s", out.String())
	}
}

func TestUninstallRemovesRecordedOldConfigsAndDuplicatePrograms(t *testing.T) {
	binaryData := uninstallBinaryBytes(t)
	f := newUninstallFixture(t)
	oldConfig := filepath.Join(f.home, "old-settings", "repo-sync.json")
	if err := writeConfig(oldConfig, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(f.home, "old-go", "bin", "repo-sync")
	third := filepath.Join(f.home, "tools", "repo-sync")
	other := filepath.Join(f.home, "unrelated", "repo-sync")
	for _, path := range []string{second, third} {
		uninstallWrite(t, path, string(binaryData), 0o755)
	}
	uninstallWrite(t, other, "#!/bin/sh\nexit 0\n", 0o755)
	f.kept[other] = "#!/bin/sh\nexit 0\n"
	writeUninstallRecord(t, []string{oldConfig}, []string{f.binary, second, second})
	var out strings.Builder
	opts := isolatedLeftoverOptions(f, &out)
	opts.binaryPaths = []string{third, other}
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, append(f.installed, oldConfig, second, third, installRecordPath())...)
	f.assertUserFilesPreserved(t)
	assertUninstallReport(t, out.String(), "Removed", oldConfig, second, third)
	assertUninstallReport(t, out.String(), "Preserved", other)
	assertUninstallReport(t, out.String(), "Failed")
}

func TestUninstallPartialFailureKeepsOwnershipForRetry(t *testing.T) {
	f := newUninstallFixture(t)
	oldDir := filepath.Join(f.home, "old-settings")
	oldConfig := filepath.Join(oldDir, "repo-sync.json")
	if err := writeConfig(oldConfig, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	writeUninstallRecord(t, []string{f.config, oldConfig}, []string{f.binary})
	if err := os.Chmod(oldDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(oldDir, 0o700) })
	var out strings.Builder
	opts := isolatedLeftoverOptions(f, &out)
	opts.keepBinary = true
	err := runUninstall(context.Background(), opts)
	if err == nil {
		t.Fatalf("failed deletion must return an error:\n%s", out.String())
	}
	uninstallAssertPresent(t, oldConfig, installRecordPath(), f.binary)
	assertUninstallReport(t, out.String(), "Removed", f.config)
	assertUninstallReport(t, out.String(), "Preserved", installRecordPath())
	assertUninstallReport(t, out.String(), "Failed", oldConfig)
	if strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatalf("partial cleanup must not report full success:\n%s", out.String())
	}
	if err := os.Chmod(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("retry must use retained ownership record: %v\n%s", err, out.String())
	}
	uninstallAssertMissing(t, oldConfig)
	uninstallAssertPresent(t, installRecordPath())
	record, err := loadInstallRecord()
	if err != nil || len(record.ConfigPaths) != 0 || len(record.BinaryPaths) == 0 {
		t.Fatalf("keep-binary retry must retain only program ownership: record=%+v error=%v", record, err)
	}
	assertUninstallReport(t, out.String(), "Removed", oldConfig)
	assertUninstallReport(t, out.String(), "Failed")
	f.assertUserFilesPreserved(t)
	out.Reset()
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("repeated successful cleanup must be harmless: %v\n%s", err, out.String())
	}
	uninstallAssertPresent(t, f.binary)
	assertUninstallReport(t, out.String(), "Removed")
	assertUninstallReport(t, out.String(), "Preserved", f.binary)
	assertUninstallReport(t, out.String(), "Failed")
}
