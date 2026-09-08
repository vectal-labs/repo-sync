package app

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

func skillFixture(t *testing.T) (skillManager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HERMES_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	return skillManager{bundle: testSkillBundle("original"), out: io.Discard}, filepath.Join(home, ".agents", "skills", "repo-sync")
}
func testSkillBundle(text string) fs.FS {
	return fstest.MapFS{
		"SKILL.md":                 &fstest.MapFile{Data: []byte(text)},
		"references/operations.md": &fstest.MapFile{Data: []byte("operations " + text)},
	}
}
func skillWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
func skillRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func skillRun(t *testing.T, manager skillManager, command string) {
	t.Helper()
	if err := manager.run(command); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInstallRefreshAndUninstall(t *testing.T) {
	manager, target := skillFixture(t)
	for _, command := range []string{"install", "install"} {
		skillRun(t, manager, command)
	}
	record, err := readSkillRecord()
	if err != nil || len(record.Copies) != 1 {
		t.Fatalf("record = %+v, %v", record, err)
	}
	if info, err := os.Stat(skillRecordPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt permissions: %v %v", info, err)
	}
	if got := skillRead(t, filepath.Join(target, "references", "operations.md")); got != "operations original" {
		t.Fatal(got)
	}
	manager.bundle = fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("new version")}, "new-reference.md": &fstest.MapFile{Data: []byte("new reference")}}
	var out strings.Builder
	manager.out = &out
	skillRun(t, manager, "status")
	if !strings.Contains(out.String(), "update available") {
		t.Fatal(out.String())
	}
	skillRun(t, manager, "refresh")
	if got := skillRead(t, filepath.Join(target, "SKILL.md")); got != "new version" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(target, "references")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete reference remains: %v", err)
	}
	skillRun(t, manager, "uninstall")
	skillRun(t, manager, "uninstall")
	for _, path := range []string{target, skillRecordPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("not removed: %s: %v", path, err)
		}
	}
}

func TestSkillDetectsAgentsAndDeduplicatesAliases(t *testing.T) {
	manager, target := skillFixture(t)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	customHermes := filepath.Join(home, "profiles", "work")
	t.Setenv("HERMES_HOME", customHermes)
	skillRun(t, manager, "install")
	record, err := readSkillRecord()
	if err != nil || len(record.Copies) != 2 {
		t.Fatalf("record = %+v, %v", record, err)
	}
	skillRead(t, filepath.Join(customHermes, "skills", "repo-sync", "SKILL.md"))
	if _, err := os.Stat(filepath.Join(home, ".pi")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unnecessary Pi copy")
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unnecessary Cursor copy")
	}
	skillRun(t, manager, "uninstall")
	if info, err := os.Lstat(filepath.Join(home, ".claude", "skills")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("agent alias was removed")
	}
}

func TestSkillHonorsClaudeOverrideAndCommandDetection(t *testing.T) {
	manager, _ := skillFixture(t)
	home := os.Getenv("HOME")
	customClaude := filepath.Join(home, "claude-custom")
	t.Setenv("CLAUDE_CONFIG_DIR", customClaude)
	bin := filepath.Join(home, "bin")
	skillWrite(t, filepath.Join(bin, "hermes"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(bin, "hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	skillRun(t, manager, "install")
	for _, root := range []string{customClaude, filepath.Join(home, ".hermes")} {
		skillRead(t, filepath.Join(root, "skills", "repo-sync", "SKILL.md"))
	}
}

func TestSkillPreservesWholeCustomizedFolders(t *testing.T) {
	for _, change := range []string{"edit", "extra file", "extra directory", "missing reference", "symlink", "parent changed"} {
		t.Run(change, func(t *testing.T) {
			manager, target := skillFixture(t)
			skillRun(t, manager, "install")
			switch change {
			case "edit":
				skillWrite(t, filepath.Join(target, "SKILL.md"), "custom instructions")
			case "extra file":
				skillWrite(t, filepath.Join(target, "personal.md"), "personal")
			case "extra directory":
				if err := os.Mkdir(filepath.Join(target, "custom"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "missing reference":
				if err := os.Remove(filepath.Join(target, "references", "operations.md")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				source := t.TempDir()
				skillWrite(t, filepath.Join(source, "SKILL.md"), "linked instructions")
				if err := os.Rename(target, target+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source, target); err != nil {
					t.Fatal(err)
				}
			case "parent changed":
				parent := filepath.Dir(target)
				if err := os.Rename(parent, parent+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-saved", parent); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			manager.out = &out
			manager.bundle = testSkillBundle("upgrade")
			skillRun(t, manager, "refresh")
			skillRun(t, manager, "uninstall")
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("customized folder removed: %v", err)
			}
			if !strings.Contains(out.String(), "Preserved") {
				t.Fatal(out.String())
			}
			if change == "extra file" {
				if got := skillRead(t, filepath.Join(target, "references", "operations.md")); got != "operations original" {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestSkillAdoptsOnlyIdenticalUnownedCopiesOnExplicitInstall(t *testing.T) {
	for _, identical := range []bool{true, false} {
		t.Run(map[bool]string{true: "identical", false: "custom"}[identical], func(t *testing.T) {
			manager, target := skillFixture(t)
			skillWrite(t, filepath.Join(target, "SKILL.md"), "original")
			skillWrite(t, filepath.Join(target, "references", "operations.md"), "operations original")
			if !identical {
				skillWrite(t, filepath.Join(target, "personal.md"), "extra")
			}
			skillRun(t, manager, "refresh")
			if _, err := os.Stat(skillRecordPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refresh adopted unowned copy")
			}
			skillRun(t, manager, "install")
			record, err := readSkillRecord()
			if err != nil {
				t.Fatal(err)
			}
			if (len(record.Copies) == 1) != identical {
				t.Fatalf("adoption: %+v", record)
			}
			skillRun(t, manager, "uninstall")
			_, err = os.Stat(target)
			if identical != errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall: %v", err)
			}
		})
	}
}

func TestSkillReadOnlyAndHooksDoNotCreateStateWithoutOptIn(t *testing.T) {
	manager, _ := skillFixture(t)
	for _, command := range []string{"status", "refresh", "uninstall"} {
		skillRun(t, manager, command)
	}
	entries, err := os.ReadDir(os.Getenv("HOME"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected state: %v, %v", entries, err)
	}
}

func TestSkillMissingCopyRequiresExplicitInstall(t *testing.T) {
	manager, target := skillFixture(t)
	skillRun(t, manager, "install")
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Fatal(err)
	}
	skillRun(t, manager, "refresh")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refresh recreated a removed folder")
	}
	skillRun(t, manager, "install")
	skillRead(t, filepath.Join(target, "SKILL.md"))
}

func TestSkillLockIndependentFromUpdateLock(t *testing.T) {
	manager, _ := skillFixture(t)
	unlock, err := acquireUpdateLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	skillRun(t, manager, "install")
	skillRun(t, manager, "refresh")
	locked, err := lockUpdateFile(skillLockPath(), syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.run("install"); err == nil || !strings.Contains(err.Error(), "another skill") {
		t.Fatalf("skill lock not enforced: %v", err)
	}
	locked()
	skillRun(t, manager, "uninstall")
}

func TestSkillRejectsBadReceiptAndSymlinks(t *testing.T) {
	for _, bad := range []string{"malformed", "traversal", "symlink"} {
		t.Run(bad, func(t *testing.T) {
			manager, target := skillFixture(t)
			skillRun(t, manager, "install")
			switch bad {
			case "malformed":
				skillWrite(t, skillRecordPath(), "{")
			case "traversal":
				record, err := readSkillRecord()
				if err != nil {
					t.Fatal(err)
				}
				record.Copies[0].Hashes["../outside"] = skillHash([]byte("outside"))
				if err := writeSkillRecord(record); err == nil {
					t.Fatal("accepted traversal")
				}
				return
			case "symlink":
				old := skillRecordPath() + "-original"
				if err := os.Rename(skillRecordPath(), old); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(old, skillRecordPath()); err != nil {
					t.Fatal(err)
				}
			}
			for _, command := range []string{"install", "refresh", "uninstall", "status"} {
				if err := manager.run(command); err == nil {
					t.Fatalf("accepted %s", bad)
				}
			}
			if got := skillRead(t, filepath.Join(target, "SKILL.md")); got != "original" {
				t.Fatal("changed managed files")
			}
		})
	}
}

func TestSkillSetupOfferAndRefresh(t *testing.T) {
	for _, answer := range []string{"yes\n", "n\n", ""} {
		t.Run(strings.ReplaceAll(answer, "\n", "newline"), func(t *testing.T) {
			manager, target := skillFixture(t)
			var out strings.Builder
			manager.out = &out
			manager.offer(bufio.NewReader(strings.NewReader(answer)))
			record, err := readSkillRecord()
			if err != nil {
				t.Fatal(err)
			}
			if record.Offered != (answer != "") {
				t.Fatalf("offered: %+v", record)
			}
			if answer == "yes\n" {
				manager.bundle = testSkillBundle("new")
				manager.offer(bufio.NewReader(strings.NewReader("")))
				if got := skillRead(t, filepath.Join(target, "SKILL.md")); got != "new" {
					t.Fatal(got)
				}
			} else if len(record.Copies) > 0 {
				t.Fatal("installed without consent")
			}
			if answer == "n\n" {
				out.Reset()
				manager.offer(bufio.NewReader(strings.NewReader("yes\n")))
				if out.Len() != 0 {
					t.Fatal("repeated declined offer")
				}
			}
		})
	}
}

func TestSkillConfigCannotOverwriteReceipt(t *testing.T) {
	_, _ = skillFixture(t)
	if err := validateInstallConfigPath(skillRecordPath()); err == nil {
		t.Fatal("config would overwrite receipt")
	}
}

func TestSkillFailedWritePreservesOwnershipAndCanRetry(t *testing.T) {
	manager, target := skillFixture(t)
	skillRun(t, manager, "install")
	old, err := readSkillRecord()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	manager.bundle = testSkillBundle("updated")
	if err := manager.run("refresh"); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	current, err := readSkillRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(old.Copies[0].Hashes, current.Copies[0].Hashes) {
		t.Fatal("failed update changed ownership")
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	skillRun(t, manager, "refresh")
	if got := skillRead(t, filepath.Join(target, "SKILL.md")); got != "updated" {
		t.Fatal(got)
	}
}

func TestSkillPartialUninstallCanRetry(t *testing.T) {
	manager, target := skillFixture(t)
	skillRun(t, manager, "install")
	if err := os.Chmod(target, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	if err := manager.run("uninstall"); err == nil {
		t.Fatal("uninstall unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(target, "references", "operations.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("did not exercise partial removal")
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	skillRun(t, manager, "uninstall")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial removal not retried: %v", err)
	}
}

func TestSkillPartialRefreshCanRetryAndProtectFurtherEdits(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "customized"}[custom], func(t *testing.T) {
			manager, target := skillFixture(t)
			skillRun(t, manager, "install")
			referenceDir := filepath.Join(target, "references")
			if err := os.Chmod(referenceDir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(referenceDir, 0o755) })
			manager.bundle = testSkillBundle("updated")
			if err := manager.run("refresh"); err == nil {
				t.Fatal("refresh unexpectedly succeeded")
			}
			if got := skillRead(t, filepath.Join(target, "SKILL.md")); got != "updated" {
				t.Fatal("did not exercise partial update")
			}
			if err := os.Chmod(referenceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if custom {
				skillWrite(t, filepath.Join(target, "SKILL.md"), "my edits")
			}
			skillRun(t, manager, "refresh")
			want := "operations updated"
			if custom {
				want = "operations original"
			}
			if got := skillRead(t, filepath.Join(referenceDir, "operations.md")); got != want {
				t.Fatal(got)
			}
			if custom && skillRead(t, filepath.Join(target, "SKILL.md")) != "my edits" {
				t.Fatal("custom edits overwritten")
			}
		})
	}
}

func TestSkillInterruptedInstallRecoversFromReceipt(t *testing.T) {
	manager, target := skillFixture(t)
	_, hashes, err := readSkillBundle(manager.bundle)
	if err != nil {
		t.Fatal(err)
	}
	copy := skillCopy{Path: target, Parent: canonicalInstallPath(filepath.Dir(target)), Pending: hashes}
	if err := writeSkillRecord(skillRecord{Version: 1, Offered: true, Copies: []skillCopy{copy}}); err != nil {
		t.Fatal(err)
	}
	skillWrite(t, filepath.Join(target, "SKILL.md"), "original")
	skillRun(t, manager, "refresh")
	skillRead(t, filepath.Join(target, "references", "operations.md"))
	record, err := readSkillRecord()
	if err != nil || record.Copies[0].Pending != nil {
		t.Fatalf("pending: %+v %v", record, err)
	}
}

func TestSkillAdoptsExistingPiCopyWithoutCreatingAnother(t *testing.T) {
	manager, _ := skillFixture(t)
	native := filepath.Join(os.Getenv("HOME"), "pi-custom", "skills", "repo-sync")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Dir(filepath.Dir(native)))
	skillWrite(t, filepath.Join(native, "SKILL.md"), "original")
	skillWrite(t, filepath.Join(native, "references", "operations.md"), "operations original")
	skillRun(t, manager, "install")
	manager.bundle = testSkillBundle("updated")
	skillRun(t, manager, "refresh")
	if got := skillRead(t, filepath.Join(native, "SKILL.md")); got != "updated" {
		t.Fatal(got)
	}
}
