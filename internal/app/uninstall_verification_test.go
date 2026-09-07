package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallReportsFilesRecreatedByPackageManager(t *testing.T) {
	f := newUninstallFixture(t)
	prefix := filepath.Join(f.home, "homebrew")
	brew := filepath.Join(prefix, "bin", "brew")
	binary := filepath.Join(prefix, "Caskroom", "repo-sync", "1.0.0", "repo-sync")
	uninstallWrite(t, brew, "fake brew; never execute", 0o755)
	uninstallWrite(t, binary, string(uninstallBinaryBytes(t)), 0o755)
	f.runner.brewPaths = []string{binary}
	lateCache := filepath.Join(f.home, "Library", "Caches", "repo-sync", "status-"+strings.Repeat("b", 64)+".json")
	f.service.runner = preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		output, err := f.runner.run(ctx, dir, stdin, name, args...)
		if filepath.Base(name) == "brew" && err == nil {
			uninstallWrite(t, lateCache, "{}", 0o600)
		}
		return output, err
	})
	var out strings.Builder
	opts := f.options(&out)
	opts.binary = binary
	if err := runUninstall(context.Background(), opts); err == nil {
		t.Fatal("recreated app data must not report complete removal")
	}
	uninstallAssertMissing(t, binary)
	uninstallAssertPresent(t, lateCache, installRecordPath())
	failed := strings.Split(out.String(), "Failed:\n")
	if len(failed) != 2 || !strings.Contains(failed[1], lateCache) || strings.Contains(out.String(), "repo-sync uninstalled.") {
		t.Fatalf("final report must name the recreated file: %s", out.String())
	}
	f.assertUserFilesPreserved(t)
}
