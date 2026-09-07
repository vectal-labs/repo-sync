package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// hookedRunner runs a callback right before the first git command named
// `command`, which lets a test change the world between two of our steps.
type hookedRunner struct {
	inner   commandRunner
	command string
	before  func()
	fired   bool
}

func (h *hookedRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	if name == "git" && slices.Contains(args, h.command) && !h.fired {
		h.fired = true
		h.before()
	}
	return h.inner.run(ctx, dir, stdin, name, args...)
}

func TestGitSyncSkipsWhenFileVanishesBeforeAdd(t *testing.T) {
	_, local := makeGitFixture(t)
	gone := filepath.Join(local, "gone.txt")
	if err := os.WriteFile(gone, []byte("brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := gitOutput(t, local, "rev-parse", "HEAD")
	runner := &hookedRunner{inner: execCommandRunner{}, command: "add", before: func() { os.Remove(gone) }}
	_, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "staging") {
		t.Fatalf("a vanished file must be a skip, got: %v", err)
	}
	if after := gitOutput(t, local, "rev-parse", "HEAD"); after != before {
		t.Fatal("skip must not commit anything")
	}
}

func TestGitSyncSkipsWhenWorktreeChangesBeforeRebase(t *testing.T) {
	_, local := makeGitFixture(t)
	runner := &hookedRunner{inner: execCommandRunner{}, command: "rebase", before: func() {
		if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("edited mid-cycle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "rebase") {
		t.Fatalf("a dirty worktree at rebase time must be a skip, got: %v", err)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); !strings.Contains(got, "README.md") {
		t.Fatalf("the user's edit must survive untouched, status: %q", got)
	}
}

func TestGitSyncRetriesWhenRemoteMovesDuringPush(t *testing.T) {
	remote, local := makeGitFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	if err := os.WriteFile(filepath.Join(local, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &hookedRunner{inner: execCommandRunner{}, command: "push", before: func() {
		writeAndCommit(t, other, "theirs.txt", "theirs\n", "teammate pushed first")
		gitRun(t, other, "push", "-q", "origin", "main")
	}}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("a rejected push must be retried, got: %v", err)
	}
	if !report.Pushed || !report.Pulled {
		t.Fatalf("report = %+v, want pulled and pushed", report)
	}
	files := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
	if !reflect.DeepEqual(files, []string{"README.md", "mine.txt", "theirs.txt"}) {
		t.Fatalf("remote files = %v; both commits must land without force-push", files)
	}
}

func TestGitSyncReportsPersistentRemoteLockAsFailure(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	lock := filepath.Join(remote, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	remoteHead := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))

	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := syncer.sync(context.Background(), repo, true)
	if err == nil {
		t.Fatalf("push through a stuck remote lock must fail, report = %+v", report)
	}
	var skip *skipError
	if errors.As(err, &skip) {
		t.Fatalf("a stuck remote lock is a failure, not a skip: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot lock ref") {
		t.Fatalf("error must carry Git's reason for the operator: %v", err)
	}
	if report.Committed != 1 || report.Pushed {
		t.Fatalf("report = %+v, want the commit kept locally and nothing pushed", report)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteHead {
		t.Fatalf("remote main moved to %s while locked", got)
	}

	// Once the lock is gone the next cycle pushes the kept commit as usual.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || report.Committed != 0 {
		t.Fatalf("after unlock: report = %+v, err = %v", report, err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:public.txt"); got != "public\n" {
		t.Fatalf("remote file = %q", got)
	}
}

func TestPushRejected(t *testing.T) {
	rejected := []string{
		"git push origin HEAD:main: exit status 1: To github.com:x/y.git\n ! [rejected]        HEAD -> main (fetch first)\nerror: failed to push some refs to 'github.com:x/y.git'",
		"git push origin HEAD:main: exit status 1: To github.com:x/y.git\n ! [rejected]        HEAD -> main (non-fast-forward)\nerror: failed to push some refs to 'github.com:x/y.git'",
		// Our lease found the destination somewhere else than we validated.
		"git push --force-with-lease=refs/heads/main:0f1e2d3c origin 6e8d7f0c:refs/heads/main: exit status 1: To github.com:x/y.git\n ! [rejected]        6e8d7f0c -> main (stale info)\nerror: failed to push some refs to 'github.com:x/y.git'",
		// The remote's own compare-and-swap lost against a concurrent push.
		"git push origin HEAD:main: exit status 1: remote: error: cannot lock ref 'refs/heads/main': is at 6e8d7f0c9b2a1e3d4c5b6a7f8e9d0c1b2a3f4e5d but expected 0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c\nTo github.com:x/y.git\n ! [remote rejected] HEAD -> main (failed to update ref)",
	}
	for _, message := range rejected {
		if !pushRejected(errors.New(message)) {
			t.Errorf("%q should count as a concurrent push", message)
		}
	}
	notRejected := []string{
		// A stuck lock file on the remote does not go away by retrying.
		"git push origin HEAD:main: exit status 1: remote: error: cannot lock ref 'refs/heads/main': Unable to create '/srv/git/notes.git/./refs/heads/main.lock': File exists.\nremote: Another git process seems to be running in this repository\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (failed to update ref)",
		"git push origin HEAD:main: exit status 1: remote: error: cannot lock ref 'refs/heads/main': Unable to create '/srv/git/notes.git/./refs/heads/main.lock': Permission denied\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (failed to update ref)",
		"git push origin HEAD:main: exit status 1: remote: no pushes allowed\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (pre-receive hook declined)",
		"git push origin HEAD:main: exit status 1: error: remote unpack failed: unable to create temporary object directory\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (unpacker error)",
		"git push origin HEAD:main: exit status 128: ERROR: Permission to x/y.git denied to someone.\nfatal: Could not read from remote repository.",
		"git push origin HEAD:main: exit status 128: fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com",
		// Reasons quoted inside a path, branch name, or hook message are not proof of a race.
		"git push origin HEAD:main: exit status 1: remote: error: cannot lock ref 'refs/heads/main': Unable to create '/srv/fetch first/non-fast-forward/refs/heads/main.lock': File exists.\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (failed to update ref)",
		"git push origin HEAD:non-fast-forward: exit status 1: To /srv/git/notes.git\n ! [remote rejected] HEAD -> non-fast-forward (pre-receive hook declined)",
		"git push origin HEAD:main: exit status 1: remote: policy: is at odds with branch protection but expected to pass\nTo /srv/git/notes.git\n ! [remote rejected] HEAD -> main (pre-receive hook declined)",
	}
	for _, message := range notRejected {
		if pushRejected(errors.New(message)) {
			t.Errorf("%q must stay a normal failure so it backs off and eventually notifies", message)
		}
	}
}

// The worktree guard runs before the commit. Everything below covers what
// slips past it: secrets staged at the last moment, by hooks, or committed by
// hand. The push itself is the last line, so it inspects the exact commits.

func remoteTree(t *testing.T, local, remote string) string {
	t.Helper()
	return gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
}

func assertWithheld(t *testing.T, err error, report syncReport, paths ...string) {
	t.Helper()
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "push withheld") {
		t.Fatalf("error = %v, want push-withheld skip", err)
	}
	var got []string
	for _, entry := range report.Withheld {
		got = append(got, entry.Path)
		if len(entry.Commit) != 7 {
			t.Fatalf("withheld entry has no commit: %+v", entry)
		}
	}
	if !report.Checked || report.Pushed || !reflect.DeepEqual(got, paths) {
		t.Fatalf("report = %+v, want withheld %v", report, paths)
	}
}

func TestGitSyncHoldsPushWhenSecretIsStagedRightBeforeCommit(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	runner := &hookedRunner{inner: execCommandRunner{}, command: "commit", before: func() {
		write(t, local, ".env", "TOKEN=1\n")
		gitRun(t, local, "add", ".env")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") {
		t.Fatalf(".env reached the remote:\n%s", tree)
	}
	// Nothing is rewritten or deleted for the user: the commit and file stay.
	if got := gitOutput(t, local, "show", "--name-only", "--pretty=", "HEAD"); !strings.Contains(got, ".env") {
		t.Fatalf("local commit was rewritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(local, ".env")); err != nil {
		t.Fatalf("user file was removed: %v", err)
	}
}

func TestGitSyncHoldsPushWhenPreCommitHookStagesSecret(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	hook := filepath.Join(local, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'TOKEN=1\\n' > .env\ngit add .env\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") {
		t.Fatalf(".env reached the remote:\n%s", tree)
	}
}

func TestGitSyncHoldsManualSecretCommitUntilDroppedOrAllowed(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "user committed a secret by hand")
	writeAndCommit(t, local, "public.txt", "public\n", "safe follow-up")
	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}

	// Held on every retry, and the safe commit behind it waits too.
	for i := 0; i < 2; i++ {
		report, err := syncer.sync(context.Background(), repo, true)
		assertWithheld(t, err, report, ".env")
	}
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") || strings.Contains(tree, "public.txt") {
		t.Fatalf("held commits reached the remote:\n%s", tree)
	}

	// The documented way out: squash the unpublished commits back into the
	// index and let repo-sync recommit. It unstages the secret and pushes the rest.
	gitRun(t, local, "reset", "--soft", "origin/main")
	report, err := syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || report.Committed != 1 || !reflect.DeepEqual(report.Blocked, []string{".env"}) {
		t.Fatalf("after reset: report = %+v, err = %v", report, err)
	}
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") || !strings.Contains(tree, "public.txt") {
		t.Fatalf("remote tree after recovery:\n%s", tree)
	}

	// Allowing the path publishes a commit that contains it.
	writeAndCommit(t, local, ".env", "TOKEN=2\n", "committed again by hand")
	report, err = syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	repo.Allow = []string{".env"}
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || len(report.Withheld) != 0 {
		t.Fatalf("allowed path still held: report = %+v, err = %v", report, err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:.env"); got != "TOKEN=2\n" {
		t.Fatalf("allowed file not pushed: %q", got)
	}
}

func TestGitSyncHoldsSecretCommittedThenDeletedBeforePush(t *testing.T) {
	remote, local := makeGitFixture(t)
	if err := os.Mkdir(filepath.Join(local, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, local, "keys/server.pem", "key\n", "oops")
	gitRun(t, local, "rm", "-q", "keys/server.pem")
	gitRun(t, local, "commit", "-q", "-m", "remove key")
	writeAndCommit(t, local, "public.txt", "public\n", "safe")
	// The final tree is clean; only the intermediate commit leaks.
	if got := gitOutput(t, local, "ls-tree", "-r", "--name-only", "HEAD"); strings.Contains(got, ".pem") {
		t.Fatalf("test setup: final tree still has the key: %q", got)
	}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, "keys/server.pem")
	if got := gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"); strings.TrimSpace(got) == strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD")) {
		t.Fatal("history containing the key was pushed")
	}
}

func TestGitSyncStillPushesAroundSecretsTheRemoteAlreadyHas(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "TOKEN=old\n", "secret already published")
	gitRun(t, local, "push", "-q", "origin", "main")
	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}

	// An unrelated change is not frozen by a baseline secret.
	writeAndCommit(t, local, "public.txt", "public\n", "safe")
	report, err := syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || len(report.Withheld) != 0 {
		t.Fatalf("baseline secret froze a safe push: report = %+v, err = %v", report, err)
	}
	// Changing the secret in a manual commit is held.
	writeAndCommit(t, local, ".env", "TOKEN=new\n", "rotated by hand")
	report, err = syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:.env"); got != "TOKEN=old\n" {
		t.Fatalf("modified secret reached the remote: %q", got)
	}
	// A commit that only removes it is always allowed.
	gitRun(t, local, "reset", "-q", "--hard", "origin/main")
	gitRun(t, local, "rm", "-q", ".env")
	gitRun(t, local, "commit", "-q", "-m", "remove secret")
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("deleting a secret must push: report = %+v, err = %v", report, err)
	}
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") {
		t.Fatalf(".env still on the remote:\n%s", tree)
	}
}

func TestGitSyncPushesOnlyTheValidatedCommit(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	// A manual secret commit lands on the branch after the check, before push.
	runner := &hookedRunner{inner: execCommandRunner{}, command: "push", before: func() {
		writeAndCommit(t, local, ".env", "TOKEN=1\n", "raced in by hand")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	tree := remoteTree(t, local, remote)
	if !strings.Contains(tree, "public.txt") || strings.Contains(tree, ".env") {
		t.Fatalf("remote tree = %q; only the validated commit may be pushed", tree)
	}
	report, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
}

func TestGitSyncRevalidatesAfterRejectedPush(t *testing.T) {
	remote, local := makeGitFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	write(t, local, "mine.txt", "mine\n")
	runner := &hookedRunner{inner: execCommandRunner{}, command: "push", before: func() {
		writeAndCommit(t, other, "theirs.txt", "theirs\n", "teammate pushed first")
		gitRun(t, other, "push", "-q", "origin", "main")
		writeAndCommit(t, local, ".env", "TOKEN=1\n", "committed by hand meanwhile")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if !report.Pulled {
		t.Fatalf("report = %+v, want pulled", report)
	}
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") || strings.Contains(tree, "mine.txt") {
		t.Fatalf("retry pushed unvalidated history:\n%s", tree)
	}
}

func TestGitSyncOrdinaryPushIsUnaffected(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "notes.md", "n\n", "manual safe commit")
	writeAndCommit(t, local, ".env.example", "TOKEN=\n", "template is fine")
	write(t, local, "public.txt", "public\n")
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || !report.Checked || len(report.Withheld) != 0 {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	files := strings.Fields(remoteTree(t, local, remote))
	if !reflect.DeepEqual(files, []string{".env.example", "README.md", "notes.md", "public.txt"}) {
		t.Fatalf("remote files = %v", files)
	}
}

// Findings from review: validation must look at the real objects and at the
// real destination, publication must be bound to what was validated, and
// submodules must neither break safe pushes nor publish unchecked.

func TestGitSyncHoldsSecretHiddenByReplaceRef(t *testing.T) {
	remote, local := makeGitFixture(t)
	base := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "secret")
	secret := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	// A replacement ref makes every ordinary Git view of the commit look clean.
	tree := strings.TrimSpace(gitOutput(t, local, "rev-parse", base+"^{tree}"))
	clean := strings.TrimSpace(gitOutput(t, local, "commit-tree", tree, "-p", base, "-m", "clean replacement"))
	gitRun(t, local, "replace", secret, clean)
	gitRun(t, local, "reset", "--hard", "HEAD")
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") {
		t.Fatalf("replaced commit still published the secret:\n%s", tree)
	}
}

func TestGitSyncRevalidatesWhenDestinationIsRolledBack(t *testing.T) {
	remote, local := makeGitFixture(t)
	base := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "previously published")
	gitRun(t, local, "push", "-q", "origin", "main")
	writeAndCommit(t, local, "safe.txt", "safe\n", "safe")
	// The destination loses the secret commit between our fetch and our push.
	runner := &hookedRunner{inner: execCommandRunner{}, command: "push", before: func() {
		gitRun(t, local, "--git-dir", remote, "update-ref", "refs/heads/main", base)
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != base {
		t.Fatalf("remote main = %s, want it left at %s", got, base)
	}
}

func TestGitSyncValidatesAgainstSeparatePushDestination(t *testing.T) {
	source, local := makeGitFixture(t)
	destination := filepath.Join(t.TempDir(), "destination.git")
	gitRun(t, local, "clone", "-q", "--bare", source, destination)
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "on the fetch source only")
	gitRun(t, local, "push", "-q", "origin", "main")
	gitRun(t, local, "remote", "set-url", "--push", "origin", destination)
	writeAndCommit(t, local, "safe.txt", "safe\n", "safe")
	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}

	report, err := syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if tree := remoteTree(t, local, destination); strings.Contains(tree, ".env") || strings.Contains(tree, "safe.txt") {
		t.Fatalf("destination received unvalidated history:\n%s", tree)
	}
	// Allowed, it publishes to the destination, which was behind the source.
	repo.Allow = []string{".env"}
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if tree := remoteTree(t, local, destination); !strings.Contains(tree, ".env") || !strings.Contains(tree, "safe.txt") {
		t.Fatalf("destination tree = %q", tree)
	}
}

func TestGitSyncNeverOverwritesDestinationHistoryItDoesNotHave(t *testing.T) {
	source, local := makeGitFixture(t)
	destination := filepath.Join(t.TempDir(), "destination.git")
	gitRun(t, local, "clone", "-q", "--bare", source, destination)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", destination, other)
	configureGitUser(t, other)
	writeAndCommit(t, other, "theirs.txt", "theirs\n", "only at the destination")
	gitRun(t, other, "push", "-q", "origin", "main")
	theirs := strings.TrimSpace(gitOutput(t, other, "rev-parse", "HEAD"))

	gitRun(t, local, "remote", "set-url", "--push", "origin", destination)
	writeAndCommit(t, local, "mine.txt", "mine\n", "mine")
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "not part of local history") || report.Pushed {
		t.Fatalf("report = %+v, err = %v; want a skip that overwrites nothing", report, err)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", destination, "rev-parse", "main")); got != theirs {
		t.Fatalf("destination main = %s, want untouched %s", got, theirs)
	}
}

func TestGitSyncRetriesLeaseRaceThenPublishes(t *testing.T) {
	remote, local := makeGitFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	write(t, local, "mine.txt", "mine\n")
	runner := &hookedRunner{inner: execCommandRunner{}, command: "push", before: func() {
		writeAndCommit(t, other, "theirs.txt", "theirs\n", "teammate pushed first")
		gitRun(t, other, "push", "-q", "origin", "main")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || !report.Pulled {
		t.Fatalf("report = %+v, err = %v; a stale lease must be retried after a fresh fetch", report, err)
	}
	files := strings.Fields(remoteTree(t, local, remote))
	if !reflect.DeepEqual(files, []string{"README.md", "mine.txt", "theirs.txt"}) {
		t.Fatalf("remote files = %v", files)
	}
}

// submoduleFixture returns a parent checkout with a submodule at child/, both
// with their own bare remotes, and the child checkout on its main branch.
func submoduleFixture(t *testing.T) (parentRemote, parent, childRemote, child string) {
	t.Helper()
	childRemote, seed := makeGitFixture(t)
	_ = seed
	parentRemote, parent = makeGitFixture(t)
	gitRun(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", childRemote, "child")
	gitRun(t, parent, "commit", "-q", "-am", "add child")
	gitRun(t, parent, "push", "-q", "origin", "main")
	child = filepath.Join(parent, "child")
	configureGitUser(t, child)
	gitRun(t, child, "checkout", "-q", "-B", "main", "origin/main")
	return parentRemote, parent, childRemote, child
}

func TestGitSyncWaitsForSubmoduleCommitsInsteadOfPushingThem(t *testing.T) {
	parentRemote, parent, childRemote, child := submoduleFixture(t)
	writeAndCommit(t, child, "safe.txt", "safe\n", "child work")
	gitRun(t, parent, "add", "child")
	gitRun(t, parent, "commit", "-q", "-m", "update child")
	gitRun(t, parent, "config", "push.recurseSubmodules", "on-demand") // user preference; we still only check
	parentBefore := strings.TrimSpace(gitOutput(t, parent, "--git-dir", parentRemote, "rev-parse", "main"))

	syncer := gitSyncer{runner: execCommandRunner{}}
	parentRepo := repoConfig{Name: "parent", Path: parent, Remote: "origin"}
	_, err := syncer.sync(context.Background(), parentRepo, true)
	var skip *skipError
	if err == nil || errors.As(err, &skip) || pushRejected(err) || !strings.Contains(err.Error(), "submodule child has commits that are not on its remote") {
		t.Fatalf("err = %v; want a normal failure naming the submodule", err)
	}
	if got := strings.TrimSpace(gitOutput(t, parent, "--git-dir", parentRemote, "rev-parse", "main")); got != parentBefore {
		t.Fatal("parent was published ahead of its submodule")
	}
	if tree := remoteTree(t, child, childRemote); strings.Contains(tree, "safe.txt") {
		t.Fatal("submodule commit was pushed on the user's behalf")
	}

	// The submodule synced as its own repository unblocks the parent.
	report, err := syncer.sync(context.Background(), repoConfig{Name: "child", Path: child, Remote: "origin"}, true)
	if err != nil || !report.Pushed {
		t.Fatalf("child: report = %+v, err = %v", report, err)
	}
	report, err = syncer.sync(context.Background(), parentRepo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("parent: report = %+v, err = %v", report, err)
	}
	if got := strings.TrimSpace(gitOutput(t, parent, "--git-dir", parentRemote, "rev-parse", "main")); got != strings.TrimSpace(gitOutput(t, parent, "rev-parse", "HEAD")) {
		t.Fatal("parent commit was not published after the submodule")
	}
}

func TestGitSyncNeverPublishesSubmoduleSecretThroughParent(t *testing.T) {
	parentRemote, parent, childRemote, child := submoduleFixture(t)
	writeAndCommit(t, child, ".env", "TOKEN=1\n", "child secret")
	gitRun(t, parent, "add", "child")
	gitRun(t, parent, "commit", "-q", "-m", "update child")
	gitRun(t, parent, "config", "push.recurseSubmodules", "on-demand")
	parentBefore := strings.TrimSpace(gitOutput(t, parent, "--git-dir", parentRemote, "rev-parse", "main"))

	syncer := gitSyncer{runner: execCommandRunner{}}
	report, err := syncer.sync(context.Background(), repoConfig{Name: "child", Path: child, Remote: "origin"}, true)
	assertWithheld(t, err, report, ".env")
	_, err = syncer.sync(context.Background(), repoConfig{Name: "parent", Path: parent, Remote: "origin"}, true)
	if err == nil || !strings.Contains(err.Error(), "submodule child") {
		t.Fatalf("parent err = %v", err)
	}
	if tree := remoteTree(t, child, childRemote); strings.Contains(tree, ".env") {
		t.Fatalf("submodule secret reached its remote:\n%s", tree)
	}
	if got := strings.TrimSpace(gitOutput(t, parent, "--git-dir", parentRemote, "rev-parse", "main")); got != parentBefore {
		t.Fatal("parent published a pointer to unpublished submodule history")
	}
}

func TestGitSyncRecoveryForModifiedTrackedSecret(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "TOKEN=old\n", "published earlier")
	gitRun(t, local, "push", "-q", "origin", "main")
	writeAndCommit(t, local, ".env", "TOKEN=new\n", "rotated by hand")
	writeAndCommit(t, local, "safe.txt", "safe\n", "safe")
	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if !report.Withheld[0].Tracked {
		t.Fatalf("a file the remote already has must be reported as tracked: %+v", report.Withheld)
	}
	advice := withheldAdvice(report.Withheld, "origin/main")
	if !strings.Contains(advice, "git checkout origin/main -- <path>") || !strings.Contains(advice, "git reset --soft origin/main") {
		t.Fatalf("advice for a tracked secret must restore the published version: %q", advice)
	}
	// Following the advice to the letter gets everything else published.
	gitRun(t, local, "reset", "-q", "--soft", "origin/main")
	gitRun(t, local, "checkout", "-q", "origin/main", "--", ".env")
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed || report.Committed != 1 {
		t.Fatalf("after recovery: report = %+v, err = %v", report, err)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:.env"); got != "TOKEN=old\n" {
		t.Fatalf("remote .env = %q", got)
	}
	if tree := remoteTree(t, local, remote); !strings.Contains(tree, "safe.txt") {
		t.Fatalf("safe work was not published:\n%s", tree)
	}
}

func TestUnpublishedSubmodules(t *testing.T) {
	err := errors.New("git push: exit status 128: The following submodule paths contain changes that can\nnot be found on any remote:\n  child\n  vendor/lib\n\nPlease try\n\n\tgit push --recurse-submodules=on-demand\n")
	if got := unpublishedSubmodules(err); !reflect.DeepEqual(got, []string{"child", "vendor/lib"}) {
		t.Fatalf("unpublishedSubmodules = %v", got)
	}
	if got := unpublishedSubmodules(errors.New("git push: ! [rejected] main -> main (stale info)")); got != nil {
		t.Fatalf("unrelated error parsed as submodules: %v", got)
	}
}

func TestGitSyncAdviceForSecretAlreadyOnFetchSource(t *testing.T) {
	source, local := makeGitFixture(t)
	destination := filepath.Join(t.TempDir(), "destination.git")
	gitRun(t, local, "clone", "-q", "--bare", source, destination)
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "published on the fetch source")
	gitRun(t, local, "push", "-q", "origin", "main")
	gitRun(t, local, "remote", "set-url", "--push", "origin", destination)
	writeAndCommit(t, local, "safe.txt", "safe\n", "safe")
	syncer := gitSyncer{runner: execCommandRunner{}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if !report.Withheld[0].OnSource || report.Withheld[0].Tracked {
		t.Fatalf("withheld = %+v; want a commit known to be on the fetch source only", report.Withheld)
	}
	advice := withheldAdvice(report.Withheld, "origin/main")
	if strings.Contains(advice, "git reset --soft") || !strings.Contains(advice, "already on origin/main") || !strings.Contains(advice, "repo-sync allow") || !strings.Contains(advice, "set-url --push") {
		t.Fatalf("advice must not promise a reset that cannot remove a published commit: %q", advice)
	}
	// A reset to the fetch tip indeed changes nothing; the guard stays put.
	gitRun(t, local, "reset", "-q", "--soft", "origin/main")
	report, err = syncer.sync(context.Background(), repo, true)
	assertWithheld(t, err, report, ".env")
	if tree := remoteTree(t, local, destination); strings.Contains(tree, ".env") {
		t.Fatalf("destination received the secret:\n%s", tree)
	}
	// Both named options work: pointing the push URL back at a destination that
	// already has the commit, or an explicit allow.
	gitRun(t, local, "remote", "set-url", "--push", "origin", source)
	report, err = syncer.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("after fixing the push URL: report = %+v, err = %v", report, err)
	}
	if tree := remoteTree(t, local, source); !strings.Contains(tree, "safe.txt") {
		t.Fatalf("source tree = %q", tree)
	}
}

func TestWithheldAdviceMixesLocalAndSourceCommits(t *testing.T) {
	advice := withheldAdvice([]withheldSecret{
		{Path: "a.pem", Commit: "1111111"},
		{Path: ".env", Commit: "2222222", Tracked: true},
		{Path: "keys.json", Commit: "3333333", OnSource: true},
	}, "origin/main")
	for _, want := range []string{"git reset --soft origin/main", "leaves a.pem out", "already tracks .env", "git checkout origin/main -- <path>", "keys.json is already on origin/main"} {
		if !strings.Contains(advice, want) {
			t.Fatalf("advice missing %q: %s", want, advice)
		}
	}
}
