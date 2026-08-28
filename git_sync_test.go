package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGitSyncCommitsRebasesAndPushes(t *testing.T) {
	remote, local := makeGitFixture(t)
	if err := os.WriteFile(filepath.Join(local, "new file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncer := gitSyncer{runner: execCommandRunner{}}
	report, err := syncer.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Committed != 1 || !report.Pushed || report.Pulled {
		t.Fatalf("report = %+v", report)
	}
	message := gitOutput(t, local, "log", "-1", "--pretty=%B")
	if !strings.Contains(message, "Changed files:\n- new file.txt") {
		t.Fatalf("commit message did not list file:\n%s", message)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:new file.txt"); got != "hello\n" {
		t.Fatalf("remote file = %q", got)
	}
	// A clean, up-to-date repository is a silent no-op.
	report, err = syncer.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil || report.String() != "" {
		t.Fatalf("second sync = %+v, %v", report, err)
	}
}

func TestGitSyncPullsRemoteChanges(t *testing.T) {
	remote, local := makeGitFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", remote, other)
	configureGitUser(t, other)
	writeAndCommit(t, other, "from-other.txt", "hi\n", "other change")
	gitRun(t, other, "push", "origin", "main")

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	if err != nil || !report.Pulled || report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(local, "from-other.txt")); err != nil {
		t.Fatalf("remote change was not pulled: %v", err)
	}
}

func TestGitSyncSkipsFeatureBranchWithoutTouchingIt(t *testing.T) {
	_, local := makeGitFixture(t)
	gitRun(t, local, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(local, "feature.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || !strings.Contains(skip.reason, "only main") {
		t.Fatalf("error = %v, want off-branch skip", err)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); !strings.Contains(got, "feature.txt") {
		t.Fatalf("worktree was unexpectedly changed: %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "feature" {
		t.Fatalf("branch was switched to %q", got)
	}
}

func TestGitSyncSkipsDetachedHead(t *testing.T) {
	_, local := makeGitFixture(t)
	gitRun(t, local, "checkout", "--detach")
	_, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || !strings.Contains(skip.reason, "detached") {
		t.Fatalf("error = %v, want detached skip", err)
	}
}

func TestGitSyncAbortsRebaseConflictAndSkips(t *testing.T) {
	remote, local := makeGitFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", remote, other)
	configureGitUser(t, other)

	writeAndCommit(t, local, "shared.txt", "local\n", "local change")
	writeAndCommit(t, other, "shared.txt", "remote\n", "remote change")
	gitRun(t, other, "push", "origin", "main")
	localHead := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(gitOutput(t, other, "rev-parse", "HEAD"))

	_, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "rebase aborted") {
		t.Fatalf("error = %v, want conflict skip", err)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD")); got != localHead {
		t.Fatalf("HEAD after abort = %s, want %s", got, localHead)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteHead {
		t.Fatalf("remote main moved to %s; force-push suspected", got)
	}
	gitDir := strings.TrimSpace(gitOutput(t, local, "rev-parse", "--git-dir"))
	if _, err := os.Stat(filepath.Join(local, gitDir, "rebase-merge")); !os.IsNotExist(err) {
		t.Fatalf("rebase was not aborted: %v", err)
	}
}

func TestGitSyncSkipsHumanOperationWithoutTouchingIt(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		dir    bool
	}{
		{name: "merge", marker: "MERGE_HEAD"},
		{name: "cherry-pick", marker: "CHERRY_PICK_HEAD"},
		{name: "revert", marker: "REVERT_HEAD"},
		{name: "bisect", marker: "BISECT_LOG"},
		{name: "rebase", marker: "rebase-merge", dir: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, local := makeGitFixture(t)
			gitDir := strings.TrimSpace(gitOutput(t, local, "rev-parse", "--git-dir"))
			marker := filepath.Join(local, gitDir, test.marker)
			var err error
			if test.dir {
				err = os.Mkdir(marker, 0o755)
			} else {
				err = os.WriteFile(marker, []byte(strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))+"\n"), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
			var skip *skipError
			if !errors.As(err, &skip) || !strings.Contains(skip.reason, test.name+" is in progress") {
				t.Fatalf("error = %v, want temporary skip", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("operation state was touched: %v", err)
			}
		})
	}
}

func TestGitSyncUsesRemoteDefaultBranch(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	local := filepath.Join(root, "local")
	gitRun(t, "", "init", "--bare", "--initial-branch=trunk", remote)
	gitRun(t, "", "clone", remote, seed)
	configureGitUser(t, seed)
	writeAndCommit(t, seed, "README.md", "initial\n", "initial")
	gitRun(t, seed, "push", "-u", "origin", "trunk")
	gitRun(t, "", "clone", remote, local) // a fresh clone records origin/HEAD -> trunk
	configureGitUser(t, local)

	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	if got := syncer.defaultBranch(context.Background(), repo); got != "trunk" {
		t.Fatalf("default branch = %q, want trunk", got)
	}
	if err := os.WriteFile(filepath.Join(local, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.sync(context.Background(), repo, true); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "trunk:a.txt"); got != "a\n" {
		t.Fatalf("remote trunk file = %q", got)
	}
	gitRun(t, local, "checkout", "-b", "main")
	_, err := syncer.sync(context.Background(), repo, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "only trunk") {
		t.Fatalf("error = %v, want skip because main is not the default branch", err)
	}
}

func TestGitSyncLeavesSecretFilesOut(t *testing.T) {
	remote, local := makeGitFixture(t)
	files := map[string]string{
		".env":            "TOKEN=1\n",
		".env.example":    "TOKEN=\n",
		"keys/server.pem": "key\n",
		"app.txt":         "app\n",
	}
	for name, contents := range files {
		path := filepath.Join(local, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, local, "add", ".env") // a user staging a secret by hand must not publish it

	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := syncer.sync(context.Background(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Committed != 2 || !reflect.DeepEqual(report.Blocked, []string{".env", "keys/server.pem"}) {
		t.Fatalf("report = %+v", report)
	}
	remoteFiles := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
	if !reflect.DeepEqual(remoteFiles, []string{".env.example", "README.md", "app.txt"}) {
		t.Fatalf("remote files = %v", remoteFiles)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); !strings.Contains(status, "?? .env") {
		t.Fatalf("staged secret was not unstaged: %q", status)
	}

	repo.Allow = []string{".env"}
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || report.Committed != 1 {
		t.Fatalf("after allow: report = %+v, err = %v", report, err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:.env"); got != "TOKEN=1\n" {
		t.Fatalf("allowed file not synced: %q", got)
	}
}

func TestGitSyncSkipsWhenTrackedSecretIsModified(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "TOKEN=old\n", "user committed a secret earlier")
	gitRun(t, local, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(local, ".env"), []byte("TOKEN=new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "tracked secret file .env") {
		t.Fatalf("error = %v, want tracked-secret skip", err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:.env"); got != "TOKEN=old\n" {
		t.Fatalf("modified secret reached the remote: %q", got)
	}
}

func TestParseStatus(t *testing.T) {
	output := " M a.txt\x00?? b/c.txt\x00R  new.txt\x00old.txt\x00A  d.txt\x00"
	got := parseStatus(output)
	want := []change{
		{" M", "a.txt"}, {"??", "b/c.txt"}, {"R ", "new.txt"}, {" D", "old.txt"}, {"A ", "d.txt"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseStatus = %+v, want %+v", got, want)
	}
	if got[0].staged() || !got[4].staged() || got[1].tracked() {
		t.Fatalf("status helpers wrong: %+v", got)
	}
}

func makeGitFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	local := filepath.Join(root, "local")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", remote)
	gitRun(t, "", "clone", remote, local)
	configureGitUser(t, local)
	writeAndCommit(t, local, "README.md", "initial\n", "initial")
	gitRun(t, local, "push", "-u", "origin", "main")
	return remote, local
}

func configureGitUser(t *testing.T, path string) {
	t.Helper()
	gitRun(t, path, "config", "user.name", "repo-sync test")
	gitRun(t, path, "config", "user.email", "repo-sync@example.invalid")
}

func writeAndCommit(t *testing.T, path, name, contents, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "add", name)
	gitRun(t, path, "commit", "-q", "-m", message)
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOutput(t, dir, args...)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
