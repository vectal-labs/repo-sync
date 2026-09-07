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

func TestUninstallHomebrewRemovesExternalAlias(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	brew := filepath.Join(prefix, "bin", "brew")
	managedLink := filepath.Join(prefix, "bin", "repo-sync")
	alias := filepath.Join(f.home, "bin", "repo-sync")
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	uninstallWrite(t, brew, "fake brew", 0o755)
	makeUninstallAlias(t, binary, managedLink)
	makeUninstallAlias(t, managedLink, alias)
	f.runner.brewPaths = []string{binary, managedLink}
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = alias
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("external Homebrew alias must be removed after package removal: %v\n%s", err, out.String())
	}
	uninstallAssertMissing(t, alias, binary, managedLink)
	f.assertUserFilesPreserved(t)
}

func TestUninstallRemovesRecordedDanglingAlias(t *testing.T) {
	f := newUninstallFixture(t)
	missing := filepath.Join(f.home, "old-version", "repo-sync")
	alias := filepath.Join(f.home, "old-bin", "repo-sync")
	makeUninstallAlias(t, missing, alias)
	writeUninstallRecord(t, []string{f.config}, []string{alias, missing, f.binary})
	var out strings.Builder
	if err := runUninstall(context.Background(), f.options(&out)); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, alias, installRecordPath())
	f.assertUserFilesPreserved(t)
}

func TestUninstallDanglingAliasFailureCanBeRetried(t *testing.T) {
	f := newUninstallFixture(t)
	missing := filepath.Join(f.home, "old-version", "repo-sync")
	alias := filepath.Join(f.home, "old-bin", "repo-sync")
	makeUninstallAlias(t, missing, alias)
	writeUninstallRecord(t, []string{f.config}, []string{alias, missing, f.binary})
	if err := os.Chmod(filepath.Dir(alias), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(alias), 0o700) })
	var out strings.Builder
	opts := f.options(&out)
	if err := runUninstall(context.Background(), opts); err == nil {
		t.Fatal("failed alias deletion must fail uninstall")
	}
	uninstallAssertPresent(t, alias, installRecordPath(), f.binary)
	if err := os.Chmod(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("recorded dangling alias must be removable on retry: %v\n%s", err, out.String())
	}
	uninstallAssertMissing(t, alias, installRecordPath())
}

func TestUninstallPreservesRecordedAliasRepointedToUnrelatedFile(t *testing.T) {
	f := newUninstallFixture(t)
	missing := filepath.Join(f.home, "old-version", "repo-sync")
	alias := filepath.Join(f.home, "old-bin", "repo-sync")
	unrelated := filepath.Join(f.home, "personal.txt")
	uninstallWrite(t, unrelated, "keep this", 0o600)
	makeUninstallAlias(t, unrelated, alias)
	writeUninstallRecord(t, []string{f.config}, []string{alias, missing, f.binary})
	var out strings.Builder
	if err := runUninstall(context.Background(), f.options(&out)); err != nil {
		t.Fatal(err)
	}
	uninstallAssertPresent(t, alias, unrelated)
	assertUninstallReport(t, out.String(), "Preserved", alias)
}

func TestUninstallRechecksAliasAfterHomebrewRemoval(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	brew := filepath.Join(prefix, "bin", "brew")
	alias := filepath.Join(f.home, "bin", "repo-sync")
	unrelated := filepath.Join(f.home, "personal.txt")
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	uninstallWrite(t, brew, "fake brew", 0o755)
	uninstallWrite(t, unrelated, "keep this", 0o600)
	makeUninstallAlias(t, binary, alias)
	f.runner.brewPaths = []string{binary}
	f.service.runner = preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		output, err := f.runner.run(ctx, dir, stdin, name, args...)
		if err == nil && filepath.Base(name) == "brew" {
			if err := os.Remove(alias); err != nil {
				return "", err
			}
			if err := os.Symlink(unrelated, alias); err != nil {
				return "", err
			}
		}
		return output, err
	})
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = alias
	err := runUninstall(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "symlink changed") {
		t.Fatalf("alias changed during package removal must be preserved and reported: %v", err)
	}
	uninstallAssertPresent(t, alias, unrelated, installRecordPath())
	if got := string(readPreflightFile(t, unrelated)); got != "keep this" {
		t.Fatalf("unrelated file changed: %q", got)
	}
}

func TestUninstallRechecksDanglingAliasBeforeRemoval(t *testing.T) {
	f := newUninstallFixture(t)
	missing := filepath.Join(f.home, "old-version", "repo-sync")
	alias := filepath.Join(f.home, "old-bin", "repo-sync")
	makeUninstallAlias(t, missing, alias)
	writeUninstallRecord(t, []string{f.config}, []string{alias, missing, f.binary})
	var out strings.Builder
	plan, err := buildCleanupPlan(f.home, f.options(&out), f.service)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	makeUninstallAlias(t, filepath.Join(f.home, "unrelated-missing"), alias)
	var report cleanupReport
	found := false
	for _, binary := range plan.binaries {
		if slices.Contains(binary.paths, alias) {
			found = true
			removeBinary(context.Background(), f.runner, binary, &report)
		}
	}
	if !found || len(report.failed) == 0 {
		t.Fatalf("changed dangling alias was not rejected: plan=%+v report=%+v", plan, report)
	}
	uninstallAssertPresent(t, alias)
}

func TestUninstallFormulaRemovesAllInstalledVersions(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	brew := filepath.Join(prefix, "bin", "brew")
	link := filepath.Join(prefix, "bin", "repo-sync")
	oldBinary := filepath.Join(prefix, "Cellar", "repo-sync", "1.0.0", "bin", "repo-sync")
	newBinary := filepath.Join(prefix, "Cellar", "repo-sync", "2.0.0", "bin", "repo-sync")
	for _, path := range []string{oldBinary, newBinary} {
		uninstallWrite(t, path, string(uninstallBinaryBytes(t)), 0o755)
	}
	uninstallWrite(t, brew, "fake brew", 0o755)
	makeUninstallAlias(t, newBinary, link)
	writeUninstallRecord(t, []string{f.config}, []string{link, oldBinary, newBinary})
	managerCalls := 0
	f.service.runner = preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if filepath.Base(name) == "brew" {
			managerCalls++
			paths := []string{newBinary, link}
			if slices.Contains(args, "--force") {
				paths = append(paths, oldBinary)
			}
			for _, path := range paths {
				if err := os.Remove(path); err != nil {
					return "", err
				}
			}
			return "", nil
		}
		return f.runner.run(ctx, dir, stdin, name, args...)
	})
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = link
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatalf("all formula versions must be removed: %v\n%s", err, out.String())
	}
	if managerCalls != 1 {
		t.Fatalf("expected one package-manager operation for all versions, got %d", managerCalls)
	}
	uninstallAssertMissing(t, oldBinary, newBinary, link)
}

func TestUninstallFormulaFailureIncludesCompleteRetryCommand(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	binary := filepath.Join(prefix, "Cellar", "repo-sync", "1.0.0", "bin", "repo-sync")
	brew := filepath.Join(prefix, "bin", "brew")
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	uninstallWrite(t, brew, "fake brew", 0o755)
	f.runner.brewError = errors.New("package database is locked")
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = binary
	err := runUninstall(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "uninstall --formula --force repo-sync") {
		t.Fatalf("formula failure needs all-versions retry command: %v", err)
	}
	uninstallAssertPresent(t, binary)
}

func makeUninstallAlias(t *testing.T, target, alias string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
}
