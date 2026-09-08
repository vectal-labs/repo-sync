package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallRemovesConflictSnapshotsAndPreservesUnknownFiles(t *testing.T) {
	f := newUninstallFixture(t)
	dir := conflictsPath(f.config)
	record := filepath.Join(dir, strings.Repeat("a", 64)+".json")
	guide := filepath.Join(dir, strings.Repeat("a", 64)+".md")
	personal := filepath.Join(dir, "personal.txt")
	uninstallWrite(t, record, "saved conflict", 0o600)
	uninstallWrite(t, guide, "saved guide", 0o600)
	uninstallWrite(t, personal, "keep", 0o600)
	var out strings.Builder
	opts := isolatedLeftoverOptions(f, &out)
	opts.keepBinary = true
	if err := runUninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	uninstallAssertMissing(t, record, guide)
	if string(mustRead(t, personal)) != "keep" {
		t.Fatal("unrelated file changed")
	}
	assertUninstallReport(t, out.String(), "Removed", record, guide)
	assertUninstallReport(t, out.String(), "Preserved", personal)
}
