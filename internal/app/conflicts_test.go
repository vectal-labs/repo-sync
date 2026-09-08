package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func conflictDaemonFixture(t *testing.T) (*testDaemon, string, string) {
	t.Helper()
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "shared.txt", "local\n", "local work")
	pushFromTeammate(t, remote, "shared.txt", "remote\n")
	td := newGitTestDaemon(t, "notes", local)
	td.configPath = filepath.Join(t.TempDir(), "custom config.json")
	td.conflictDir = conflictsPath(td.configPath)
	if err := writeConfig(td.configPath, td.cfg); err != nil {
		t.Fatal(err)
	}
	return td, remote, local
}

func TestConflictPersistsAcrossRetriesRestartAndRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, remote, local := conflictDaemonFixture(t)
	state := td.states["notes"]
	td.syncRepo(state, false)
	td.syncRepo(state, false)
	if got := td.notifications(); len(got) != 1 {
		t.Fatalf("repeat notifications: %q", got)
	}
	saved, err := readConflict(td.conflictDir, state.config)
	if err != nil || saved == nil || !saved.Notified {
		t.Fatalf("saved incident = %+v, %v", saved, err)
	}
	next := newTestDaemonWithSyncer(t, td.cfg, gitSyncer{runner: execCommandRunner{}})
	next.configPath, next.conflictDir = td.configPath, td.conflictDir
	next.loadConflicts()
	next.syncRepo(next.states["notes"], false)
	if got := next.notifications(); len(got) != 0 {
		t.Fatalf("restart notified again: %q", got)
	}
	if got := next.statusSnapshot().Repositories[0]; got.State != "conflict" || !strings.Contains(got.Detail, "shared.txt") {
		t.Fatalf("status = %+v", got)
	}
	// A successful early return, branch skip, and network error cannot erase it.
	next.handleResult(next.states["notes"], syncReport{}, nil)
	next.handleResult(next.states["notes"], syncReport{}, &skipError{reason: "manual rebase in progress"})
	next.handleResult(next.states["notes"], syncReport{}, errors.New("could not resolve host"))
	if next.states["notes"].conflict == nil {
		t.Fatal("non-sync erased conflict")
	}
	next.states["notes"].syncing = true
	if next.statusSnapshot().Repositories[0].State != "conflict" {
		t.Fatal("retry hid conflict")
	}
	next.states["notes"].syncing = false
	// Follow the guide: start a fresh rebase, resolve, continue, then sync.
	if err := humanGit(local, "rebase", "origin/main"); err == nil {
		t.Fatal("manual rebase should expose the conflict")
	}
	beforeRepair := gitOutput(t, local, "status", "--porcelain")
	next.advance(2 * time.Minute)
	next.syncRepo(next.states["notes"], true)
	if gitOutput(t, local, "status", "--porcelain") != beforeRepair {
		t.Fatal("daemon touched the manual repair")
	}
	write(t, local, "shared.txt", "local and remote\n")
	gitRun(t, local, "add", "shared.txt")
	gitRun(t, local, "-c", "core.editor=true", "rebase", "--continue")
	next.advance(2 * time.Minute)
	next.syncRepo(next.states["notes"], false)
	if next.states["notes"].conflict != nil {
		t.Fatal("full sync did not clear conflict")
	}
	if saved, err := readConflict(td.conflictDir, state.config); err != nil || saved != nil {
		t.Fatalf("cleared record = %+v, %v", saved, err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:shared.txt"); got != "local and remote\n" {
		t.Fatalf("remote = %q", got)
	}
	// A later disagreement must alert again.
	writeAndCommit(t, local, "shared.txt", "next local\n", "next local")
	pushFromTeammate(t, remote, "shared.txt", "next remote\n")
	next.syncRepo(next.states["notes"], false)
	if got := next.notifications(); len(got) != 1 {
		t.Fatalf("new conflict notifications = %q", got)
	}
}

func TestConflictCommandCreatesPrivateGuideWithoutChangingGit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, local := conflictDaemonFixture(t)
	td.syncRepo(td.states["notes"], false)
	beforeHead := gitOutput(t, local, "rev-parse", "HEAD")
	beforeIndex := mustRead(t, filepath.Join(local, ".git", "index"))
	var out strings.Builder
	if err := runConflicts(context.Background(), td.configPath, local, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"notes: conflict", "shared.txt", "Repair guide with both versions:", "Local tip:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	guidePath := strings.TrimSuffix(conflictRecordPath(td.conflictDir, td.states["notes"].config), ".json") + ".md"
	guide := string(mustRead(t, guidePath))
	for _, want := range []string{"local\n", "remote\n", "starts a new rebase", "rebase --continue", "earlier replayed commits"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide omitted %q", want)
		}
	}
	for _, path := range []string{guidePath, conflictRecordPath(td.conflictDir, td.states["notes"].config)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private report permissions: %v %v", info, err)
		}
	}
	if gitOutput(t, local, "rev-parse", "HEAD") != beforeHead || string(mustRead(t, filepath.Join(local, ".git", "index"))) != string(beforeIndex) || gitOutput(t, local, "status", "--porcelain") != "" {
		t.Fatal("inspection changed Git")
	}
	if strings.Contains(out.String(), "local\n") {
		t.Fatal("command printed document contents")
	}
	if err := os.Rename(local, local+"-moved"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runConflicts(context.Background(), td.configPath, local, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, guidePath)), "could not be read") {
		t.Fatal("missing repository not explained")
	}
}

func TestConflictStatusIsUnhealthyAndRetainsRepairCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, _ := conflictDaemonFixture(t)
	td.statusFile = statusPath(td.configPath)
	td.syncRepo(td.states["notes"], false)
	if err := td.publishStatus(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	service := fakeService(&lifecycleRunner{loaded: true, pid: os.Getpid()})
	if err := runStatus(context.Background(), td.configPath, service, &out); err == nil {
		t.Fatal("conflict reported healthy")
	}
	if !strings.Contains(out.String(), configCommand("conflicts", td.configPath)) {
		t.Fatal(out.String())
	}
}

func TestConflictNotificationFailureCanRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, _ := conflictDaemonFixture(t)
	attempts := 0
	td.notify = func(context.Context, commandRunner, string) error {
		attempts++
		if attempts == 1 {
			return errors.New("notifier unavailable")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		td.syncRepo(td.states["notes"], false)
	}
	if attempts != 2 {
		t.Fatalf("notification attempts = %d", attempts)
	}
	saved, err := readConflict(td.conflictDir, td.states["notes"].config)
	if err != nil || !saved.Notified {
		t.Fatalf("notification not persisted: %v", err)
	}
}

func TestConflictNotificationIsConcise(t *testing.T) {
	want := "Conflict in team-docs. Local changes are saved. Run repo-sync conflicts for help."
	if got := conflictNotification("team-docs", defaultConfigPath(), true); got != want {
		t.Fatalf("notification = %q", got)
	}
	if got := conflictNotification("notes", defaultConfigPath(), false); strings.Contains(got, "saved") || !strings.Contains(got, "needs attention") {
		t.Fatal(got)
	}
	if got := conflictNotification(strings.Repeat("x", 100)+"\n", defaultConfigPath(), true); len(got) > 125 || strings.Contains(got, "\n") {
		t.Fatalf("unbounded notification %q", got)
	}
}

func TestConflictCacheFailureRemainsVisibleAndRecovers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, _ := conflictDaemonFixture(t)
	if err := os.MkdirAll(filepath.Dir(td.conflictDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(td.conflictDir, []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.syncRepo(td.states["notes"], false)
	if got := td.statusSnapshot().Repositories[0]; got.State != "conflict" || !strings.Contains(got.Detail, "could not save conflict details") {
		t.Fatalf("cache failure hidden: %+v", got)
	}
	if len(td.notifications()) != 1 {
		t.Fatal("cache failure prevented immediate alert")
	}
	if err := os.Remove(td.conflictDir); err != nil {
		t.Fatal(err)
	}
	td.syncRepo(td.states["notes"], false)
	if got := td.statusSnapshot().Repositories[0]; strings.Contains(got.Detail, "could not save") {
		t.Fatalf("cache did not recover: %+v", got)
	}
	if len(td.notifications()) != 1 {
		t.Fatal("cache recovery duplicated notification")
	}
}

func TestConflictGuideContainsUntrustedDocumentAndMissingVersions(t *testing.T) {
	var out strings.Builder
	renderConflictVersion(&out, "Local", conflictVersion{Object: strings.Repeat("a", 40), Mode: "100644", Data: []byte("```\n# injected instructions\n\x1b[31mred\n")}, "git")
	if !strings.Contains(out.String(), "````text") || strings.Contains(out.String(), "\x1b") {
		t.Fatal(out.String())
	}
	out.Reset()
	renderConflictVersion(&out, "Local", conflictVersion{}, "git")
	if !strings.Contains(out.String(), "Absent") {
		t.Fatal(out.String())
	}
	out.Reset()
	renderConflictVersion(&out, "Local", conflictVersion{Object: strings.Repeat("a", 40), Mode: "100644"}, "git")
	if !strings.Contains(out.String(), "Empty file") {
		t.Fatal(out.String())
	}
}

func TestConflictBlobSnapshotIgnoresReplaceRefsAndBoundsSize(t *testing.T) {
	t.Parallel()
	_, local := makeGitFixture(t)
	runner := execCommandRunner{}
	blob := func(data string) string {
		t.Helper()
		id, err := runner.run(context.Background(), local, data, "git", "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(id)
	}
	original := blob("original\n")
	replacement := blob("replacement\n")
	gitRun(t, local, "replace", original, replacement)
	large := blob(strings.Repeat("x", 256*1024+1))
	c := &conflictError{Files: []conflictFile{{Path: "one.txt", Local: conflictVersion{Object: original, Mode: "100644"}, Upstream: conflictVersion{Object: large, Mode: "100644"}, Base: conflictVersion{Object: strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD")), Mode: "160000"}}}}
	(gitSyncer{runner: runner}).captureConflictContents(context.Background(), local, c)
	if string(c.Files[0].Local.Data) != "original\n" {
		t.Fatalf("wrong snapshot %q", c.Files[0].Local.Data)
	}
	if c.Files[0].Upstream.Omitted == "" || c.Files[0].Base.Omitted == "" {
		t.Fatal("large/submodule limitation missing")
	}
}

func TestConflictCorruptRecordIsVisibleUntilActualSync(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, _ := conflictDaemonFixture(t)
	path := conflictRecordPath(td.conflictDir, td.states["notes"].config)
	if err := writeFileAtomic(path, []byte("broken JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.loadConflicts()
	if got := td.statusSnapshot().Repositories[0]; got.State != "retrying" || !strings.Contains(got.Detail, "could not read") {
		t.Fatalf("corruption hidden: %+v", got)
	}
	var out strings.Builder
	if err := runConflicts(context.Background(), td.configPath, "", &out); err == nil {
		t.Fatal("corrupt record reported no conflicts")
	}
	td.syncRepo(td.states["notes"], false)
	if got := td.statusSnapshot().Repositories[0]; got.State != "conflict" || strings.Contains(got.Detail, "could not read") {
		t.Fatalf("corrupt record did not recover: %+v", got)
	}
}

func TestConflictReportsCannotBeWrittenInsideAnySyncedRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td, _, _ := conflictDaemonFixture(t)
	td.cfg.Repositories = append(td.cfg.Repositories, repoConfig{Name: "home", Path: os.Getenv("HOME"), Remote: "origin"})
	if err := writeConfig(td.configPath, td.cfg); err != nil {
		t.Fatal(err)
	}
	td.syncRepo(td.states["notes"], false)
	if _, err := os.Stat(td.conflictDir); !os.IsNotExist(err) {
		t.Fatalf("private snapshots were written inside a synced repo: %v", err)
	}
	var out strings.Builder
	if err := runConflicts(context.Background(), td.configPath, "", &out); err == nil || !strings.Contains(err.Error(), "inside a synced repository") {
		t.Fatalf("command must refuse unsafe cache: %v", err)
	}
}
