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
	output := " M a.txt\x00?? b/c.txt\x00R  new.txt\x00old.txt\x00A  d.txt\x00 R wt.txt\x00wt-old.txt\x00C  copy.txt\x00src.txt\x00"
	got := parseStatus(output)
	want := []change{
		{" M", "a.txt"}, {"??", "b/c.txt"}, {"R ", "new.txt"}, {"D ", "old.txt"}, {"A ", "d.txt"},
		{" R", "wt.txt"}, {" D", "wt-old.txt"}, {"C ", "copy.txt"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseStatus = %+v, want %+v", got, want)
	}
	if got[0].staged() || !got[3].staged() || !got[4].staged() || got[1].tracked() {
		t.Fatalf("status helpers wrong: %+v", got)
	}
	if !got[3].deleted() || (change{"DA", "x"}).deleted() || (change{" D", "x"}).deleted() {
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

// A staged rename (`git mv`) must sync as a rename: the old path has already
// left the index, so there is nothing to `git add` for it.
func TestGitSyncCommitsStagedRename(t *testing.T) {
	remote, local := makeGitFixture(t)
	gitRun(t, local, "mv", "README.md", "renamed.md")

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("staged rename must sync, got: %v", err)
	}
	if report.Committed == 0 || !report.Pushed {
		t.Fatalf("report = %+v", report)
	}
	files := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
	if !reflect.DeepEqual(files, []string{"renamed.md"}) {
		t.Fatalf("remote files = %v, want only renamed.md", files)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:renamed.md"); got != "initial\n" {
		t.Fatalf("renamed contents = %q", got)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree not clean after sync: %q", status)
	}
}

// A staged deletion (`git rm`) must sync even when it is the only change, and
// an untracked secret next to it must stay out of the commit.
func TestGitSyncCommitsStagedDeletion(t *testing.T) {
	remote, local := makeGitFixture(t)
	gitRun(t, local, "rm", "-q", "README.md")
	write(t, local, ".env", "TOKEN=1\n")

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("staged deletion must sync, got: %v", err)
	}
	if report.Committed != 1 || !report.Pushed || !reflect.DeepEqual(report.Blocked, []string{".env"}) {
		t.Fatalf("report = %+v", report)
	}
	if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.TrimSpace(tree) != "" {
		t.Fatalf("remote still has files: %q", tree)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); status != "?? .env\n" {
		t.Fatalf("status after sync = %q, want only the untracked secret", status)
	}
}

// Staged and unstaged edits can coexist: a rename whose new path was edited
// afterwards, a staged file edited again, an unstaged deletion, and a path
// with NUL-unfriendly characters. All of it must land as the user has it.
func TestGitSyncCommitsMixedStagedAndUnstagedChanges(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "keep.txt", "keep\n", "add keep")
	writeAndCommit(t, local, "drop.txt", "drop\n", "add drop")
	gitRun(t, local, "push", "-q", "origin", "main")

	weird := "odd name*\n[1].md"
	gitRun(t, local, "mv", "README.md", weird)
	write(t, local, weird, "renamed then edited\n")
	write(t, local, "keep.txt", "staged\n")
	gitRun(t, local, "add", "keep.txt")
	write(t, local, "keep.txt", "staged then edited\n")
	if err := os.Remove(filepath.Join(local, "drop.txt")); err != nil {
		t.Fatal(err)
	}

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("mixed changes must sync, got: %v", err)
	}
	if !report.Pushed {
		t.Fatalf("report = %+v", report)
	}
	files := splitNUL(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "-z", "main"))
	if !reflect.DeepEqual(files, []string{"keep.txt", weird}) {
		t.Fatalf("remote files = %q", files)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:"+weird); got != "renamed then edited\n" {
		t.Fatalf("renamed contents = %q", got)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:keep.txt"); got != "staged then edited\n" {
		t.Fatalf("keep.txt contents = %q", got)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree not clean after sync: %q", status)
	}
}

// Renaming a file onto a blocked name is treated like any other blocked file:
// the secret is unstaged and stays local; the rest of the change still syncs.
func TestGitSyncBlocksRenameToSecretPath(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "notes.txt", "TOKEN=1\n", "add notes")
	gitRun(t, local, "push", "-q", "origin", "main")
	gitRun(t, local, "mv", "notes.txt", ".env")

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if !reflect.DeepEqual(report.Blocked, []string{".env"}) || !report.Pushed {
		t.Fatalf("report = %+v", report)
	}
	files := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
	if !reflect.DeepEqual(files, []string{"README.md"}) {
		t.Fatalf("remote files = %v; .env must never be published", files)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); status != "?? .env\n" {
		t.Fatalf("status after sync = %q, want the secret left untracked locally", status)
	}
}

// `git rm` followed by recreating the file and `git add -N` shows up as `DA`:
// deleted in the index, re-added with intent-to-add. That is a modification,
// not a deletion, and must sync as one so a teammate's edit to another part
// of the same file still merges cleanly.
func TestGitSyncKeepsRecreatedIntentToAddFile(t *testing.T) {
	remote, local := makeGitFixture(t)
	base := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	writeAndCommit(t, local, "shared.txt", base, "base")
	gitRun(t, local, "push", "-q", "origin", "main")
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	writeAndCommit(t, other, "shared.txt", strings.Replace(base, "nine", "remote-nine", 1), "remote edit")
	gitRun(t, other, "push", "-q", "origin", "main")

	gitRun(t, local, "rm", "-q", "shared.txt")
	write(t, local, "shared.txt", strings.Replace(base, "two", "local-two", 1))
	gitRun(t, local, "add", "-N", "shared.txt")
	if status := gitOutput(t, local, "status", "--porcelain"); status != "DA shared.txt\n" {
		t.Fatalf("fixture status = %q, want DA", status)
	}

	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil {
		t.Fatalf("recreated file must sync in one cycle, got: %v", err)
	}
	if report.Committed != 1 || !report.Pulled || !report.Pushed {
		t.Fatalf("report = %+v", report)
	}
	want := strings.Replace(strings.Replace(base, "two", "local-two", 1), "nine", "remote-nine", 1)
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:shared.txt"); got != want {
		t.Fatalf("remote shared.txt = %q, want both edits merged", got)
	}
	if status := gitOutput(t, local, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree not clean after sync: %q", status)
	}
}

// Deleting a blocked path is not a leak. Renaming a tracked `.env` to a safe
// name, or removing it with `git rm`, must sync so the secret leaves the tree.
func TestGitSyncCommitsDeletionOfTrackedSecret(t *testing.T) {
	for _, test := range []struct {
		name string
		git  []string
		want []string // remote tree afterwards
	}{
		{"rename to safe name", []string{"mv", ".env", "public.txt"}, []string{"README.md", "public.txt"}},
		{"git rm", []string{"rm", "-q", ".env"}, []string{"README.md"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote, local := makeGitFixture(t)
			writeAndCommit(t, local, ".env", "fixture only\n", "user committed a secret earlier")
			gitRun(t, local, "push", "-q", "origin", "main")
			gitRun(t, local, test.git...)

			report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
			if err != nil {
				t.Fatalf("deleting a secret path must sync, got: %v", err)
			}
			if report.Committed != 1 || !report.Pushed {
				t.Fatalf("report = %+v", report)
			}
			files := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
			if !reflect.DeepEqual(files, test.want) {
				t.Fatalf("remote files = %v, want %v", files, test.want)
			}
			if status := gitOutput(t, local, "status", "--porcelain"); status != "" {
				t.Fatalf("worktree not clean after sync: %q", status)
			}
			// The next cycle must be a silent no-op, not a stuck retry.
			if report, err := (gitSyncer{runner: execCommandRunner{}}).sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true); err != nil || report.String() != "" {
				t.Fatalf("second sync = %+v, %v", report, err)
			}
		})
	}
}

// `git rm --cached` plus a .gitignore entry is how users stop tracking a file
// while keeping it on disk. Git would silently overwrite that ignored copy
// when a rebase checks out the remote, so a rebase that has to check anything
// out is refused while the file is in the way. The local copy is never
// touched, the local deletion commit is kept, and nothing is pushed until the
// user moves the file aside. The same applies to ordinary files, not just
// secrets. When the remote has not moved there is nothing to check out and
// the deletion syncs normally.
func TestGitSyncRefusesRebaseOverRetainedIgnoredFile(t *testing.T) {
	for _, test := range []struct {
		name   string
		file   string
		remote string // "" = unchanged, otherwise the file the teammate commits
	}{
		{"secret, remote unchanged", ".env", ""},
		{"secret, unrelated remote edit", ".env", "teammate.txt"},
		{"secret, conflicting remote edit", ".env", ".env"},
		{"ordinary file, unrelated remote edit", "notes.txt", "teammate.txt"},
		{"nested secret, unrelated remote edit", "config/.env", "teammate.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote, local := makeGitFixture(t)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(local, test.file)), 0o755); err != nil {
				t.Fatal(err)
			}
			writeAndCommit(t, local, test.file, "old\n", "tracked baseline")
			gitRun(t, local, "push", "-q", "origin", "main")
			if test.remote != "" {
				other := filepath.Join(t.TempDir(), "other")
				gitRun(t, "", "clone", "-q", remote, other)
				configureGitUser(t, other)
				writeAndCommit(t, other, test.remote, "remote\n", "remote edit")
				gitRun(t, other, "push", "-q", "origin", "main")
			}
			remoteBefore := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))

			gitRun(t, local, "rm", "-q", "--cached", test.file)
			want := "kept locally, never committed\n"
			write(t, local, test.file, want)
			write(t, local, ".gitignore", test.file+"\n")

			syncer := gitSyncer{runner: execCommandRunner{}}
			repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
			report, err := syncer.sync(context.Background(), repo, true)
			if got, readErr := os.ReadFile(filepath.Join(local, test.file)); readErr != nil || string(got) != want {
				t.Fatalf("retained local file was lost: %q, %v (report=%+v err=%v)", got, readErr, report, err)
			}
			if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
				t.Fatalf("status after sync = %q, want clean", got)
			}
			if test.remote == "" {
				if err != nil || report.Committed != 2 || !report.Pushed {
					t.Fatalf("report = %+v, err = %v; the deletion must sync when nothing needs checking out", report, err)
				}
				if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, test.file) {
					t.Fatalf("remote files = %q; the deletion did not reach the remote", tree)
				}
				if report, err := syncer.sync(context.Background(), repo, true); err != nil || report.String() != "" {
					t.Fatalf("second sync = %+v, %v", report, err)
				}
				return
			}
			var skip *skipError
			if errors.As(err, &skip) || err == nil || !strings.Contains(err.Error(), "ignored file "+test.file+" would be overwritten") {
				t.Fatalf("error = %v, want a refused rebase naming the file", err)
			}
			if report.Pushed {
				t.Fatalf("report = %+v; nothing may be pushed", report)
			}
			if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteBefore {
				t.Fatalf("remote main moved to %s", got)
			}
			if message := gitOutput(t, local, "log", "-1", "--pretty=%B"); !strings.Contains(message, test.file) {
				t.Fatalf("the local deletion commit was lost: %q", message)
			}
			// Once the file is out of the way the deletion syncs like any other change.
			if err := os.Rename(filepath.Join(local, test.file), filepath.Join(t.TempDir(), "aside")); err != nil {
				t.Fatal(err)
			}
			report, err = syncer.sync(context.Background(), repo, true)
			if test.remote == test.file {
				if !errors.As(err, &skip) || !strings.Contains(skip.reason, "rebase aborted") {
					t.Fatalf("error = %v, want conflict skip", err)
				}
				return
			}
			if err != nil || !report.Pushed {
				t.Fatalf("after moving the file aside: report = %+v, err = %v", report, err)
			}
			if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, test.file) || !strings.Contains(tree, "teammate.txt") {
				t.Fatalf("remote files = %q", tree)
			}
		})
	}
}

// An untracked (not ignored) copy is protected by Git itself: the rebase's
// checkout refuses to overwrite it. repo-sync must then leave the file exactly
// as it is, even when an editor wrote newer contents a moment earlier.
func TestGitSyncKeepsNewerUntrackedEditWhenRebaseIsRefused(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "old\n", "tracked baseline")
	gitRun(t, local, "push", "-q", "origin", "main")
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	writeAndCommit(t, other, "teammate.txt", "remote\n", "remote edit")
	gitRun(t, other, "push", "-q", "origin", "main")
	remoteBefore := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))

	gitRun(t, local, "rm", "-q", "--cached", ".env")
	write(t, local, ".env", "first local edit\n")
	want := "newer editor contents\n"
	runner := &hookedRunner{inner: execCommandRunner{}, command: "rebase", before: func() { write(t, local, ".env", want) }}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if !runner.fired {
		t.Fatal("test setup: the rebase was never attempted")
	}
	if err == nil || !strings.Contains(err.Error(), "untracked working tree files would be overwritten") {
		t.Fatalf("error = %v, want Git's own refusal", err)
	}
	if report.Pushed {
		t.Fatalf("report = %+v; nothing may be pushed", report)
	}
	if got, readErr := os.ReadFile(filepath.Join(local, ".env")); readErr != nil || string(got) != want {
		t.Fatalf("newer local edit was overwritten: %q, %v", got, readErr)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteBefore {
		t.Fatalf("remote main moved to %s", got)
	}
}
