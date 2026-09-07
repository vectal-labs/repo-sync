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
	"time"
)

func TestGitSyncCommitsRebasesAndPushes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, local := makeGitFixture(t)
	gitRun(t, local, "checkout", "--detach")
	_, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || !strings.Contains(skip.reason, "detached") {
		t.Fatalf("error = %v, want detached skip", err)
	}
}

func TestGitSyncAbortsRebaseConflictAndSkips(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if git := gitArgs(name, args); git != nil && slices.Contains(git, h.command) && !h.fired {
		h.fired = true
		h.before()
	}
	return h.inner.run(ctx, dir, stdin, name, args...)
}

// gitArgs returns the git arguments of a command, also when it is run as
// `env GIT_INDEX_FILE=... git ...`, or nil when it is not git at all.
func gitArgs(name string, args []string) []string {
	if name == "git" {
		return args
	}
	if name == "env" {
		for i, arg := range args {
			if arg == "git" {
				return args[i+1:]
			}
		}
	}
	return nil
}

// humanGit runs a git command the way a person in a terminal would and
// returns the failure instead of stopping the test.
func humanGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(output)))
	}
	return nil
}

func TestGitSyncSkipsWhenFileVanishesBeforeAdd(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	remote, local := makeGitFixture(t)
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Staging a secret from another process right before the commit is refused
// outright: repo-sync owns the checkout, so Git rejects the `git add` with
// its lock message. The secret stays an untracked file and never reaches the
// remote. (A hook doing the same inherits our index and is covered below.)
func TestGitSyncHoldsPushWhenSecretIsStagedRightBeforeCommit(t *testing.T) {
	t.Parallel()
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	var addErr error
	runner := &hookedRunner{inner: execCommandRunner{}, command: "commit", before: func() {
		write(t, local, ".env", "TOKEN=1\n")
		addErr = humanGit(local, "add", ".env")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	if err != nil || report.Committed != 1 || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if addErr == nil || !strings.Contains(addErr.Error(), "index.lock") {
		t.Fatalf("the concurrent git add must be refused by Git's lock, got: %v", addErr)
	}
	if tree := remoteTree(t, local, remote); strings.Contains(tree, ".env") || !strings.Contains(tree, "public.txt") {
		t.Fatalf("remote tree:\n%s", tree)
	}
	// Nothing is rewritten or deleted for the user: the file stays, untracked.
	if got := gitOutput(t, local, "status", "--porcelain"); got != "?? .env\n" {
		t.Fatalf("status = %q", got)
	}
}

func TestGitSyncHoldsPushWhenPreCommitHookStagesSecret(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	err := errors.New("git push: exit status 128: The following submodule paths contain changes that can\nnot be found on any remote:\n  child\n  vendor/lib\n\nPlease try\n\n\tgit push --recurse-submodules=on-demand\n")
	if got := unpublishedSubmodules(err); !reflect.DeepEqual(got, []string{"child", "vendor/lib"}) {
		t.Fatalf("unpublishedSubmodules = %v", got)
	}
	if got := unpublishedSubmodules(errors.New("git push: ! [rejected] main -> main (stale info)")); got != nil {
		t.Fatalf("unrelated error parsed as submodules: %v", got)
	}
}

func TestGitSyncAdviceForSecretAlreadyOnFetchSource(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A staged rename (`git mv`) must sync as a rename: the old path has already
// left the index, so there is nothing to `git add` for it.
func TestGitSyncCommitsStagedRename(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, test := range []struct {
		name string
		git  []string
		want []string // remote tree afterwards
	}{
		{"rename to safe name", []string{"mv", ".env", "public.txt"}, []string{"README.md", "public.txt"}},
		{"git rm", []string{"rm", "-q", ".env"}, []string{"README.md"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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

// A human runs an ordinary `git checkout feature` while a sync is staging,
// committing, rebasing, or pushing. Git refuses it: repo-sync owns the
// checkout for exactly that stretch. The sync finishes on the default branch,
// feature work is never published, and the feature branch stays as it was.
func TestGitSyncRefusesCheckoutWhileItOwnsTheCheckout(t *testing.T) {
	// The push sends the validated commit hash, so the checkout is released
	// before it: a switch during the push succeeds and changes nothing.
	for _, step := range []string{"add", "commit", "rebase", "push"} {
		t.Run("during "+step, func(t *testing.T) {
			remote, local := makeGitFixture(t)
			featureTip := makeFeatureBranch(t, local)
			pushFromTeammate(t, remote, "remote.txt", "remote\n")
			write(t, local, "mine.txt", "mine\n")
			var checkoutErr error
			runner := &hookedRunner{inner: execCommandRunner{}, command: step, before: func() {
				checkoutErr = humanGit(local, "checkout", "feature")
			}}
			report, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
			if err != nil || report.Committed != 1 || !report.Pulled || !report.Pushed {
				t.Fatalf("report = %+v, err = %v", report, err)
			}
			if step == "push" {
				if checkoutErr != nil {
					t.Fatalf("the checkout is not owned during the push, got: %v", checkoutErr)
				}
				gitRun(t, local, "checkout", "-q", "main")
			} else if checkoutErr == nil || !strings.Contains(checkoutErr.Error(), "index.lock") {
				t.Fatalf("the human checkout must be refused by Git's lock, got: %v", checkoutErr)
			}
			assertMainSynced(t, remote, local, []string{"README.md", "mine.txt", "remote.txt"})
			assertFeatureUntouched(t, remote, local, featureTip)
		})
	}
}

// The scenario from the review: a rebase is paused (here by a slow post-commit
// hook) and a human switches to feature from another terminal. Before the fix
// the rebase finished with feature history on main and pushed it.
func TestGitSyncKeepsMainWhenHumanSwitchesDuringRebase(t *testing.T) {
	remote, local := makeGitFixture(t)
	featureTip := makeFeatureBranch(t, local)
	writeAndCommit(t, local, "mine.txt", "mine\n", "main work")
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	gitDir := filepath.Join(local, ".git")
	hook := "#!/bin/sh\nif test ! -e \"$GIT_DIR_ABS/fired\"; then\n touch \"$GIT_DIR_ABS/fired\" \"$GIT_DIR_ABS/ready\"\n while test ! -e \"$GIT_DIR_ABS/resume\"; do sleep 0.01; done\nfi\n"
	hook = strings.ReplaceAll(hook, "$GIT_DIR_ABS", gitDir)
	if err := os.WriteFile(filepath.Join(gitDir, "hooks", "post-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	human := make(chan error, 1)
	go func() {
		defer os.WriteFile(filepath.Join(gitDir, "resume"), nil, 0o600)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(gitDir, "ready")); err == nil {
				human <- humanGit(local, "checkout", "feature")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		human <- errors.New("the rebase never paused")
	}()
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	if err != nil || !report.Pulled || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if checkoutErr := <-human; checkoutErr == nil || !strings.Contains(checkoutErr.Error(), "index.lock") {
		t.Fatalf("the human checkout must be refused by Git's lock, got: %v", checkoutErr)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt", "remote.txt"})
	assertFeatureUntouched(t, remote, local, featureTip)
}

// `git checkout -b` at the current commit is the one switch Git allows without
// the index lock. If it lands right before our commit, the commit goes to the
// new branch; repo-sync must notice and put that branch back exactly as it was.
func TestGitSyncUndoesCommitOnBranchCreatedMidCommit(t *testing.T) {
	remote, local := makeGitFixture(t)
	base := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	write(t, local, "mine.txt", "mine\n")
	var statusBefore string
	runner := &hookedRunner{inner: execCommandRunner{}, command: "commit", before: func() {
		gitRun(t, local, "checkout", "-b", "feature")
		statusBefore = gitOutput(t, local, "status", "--porcelain")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || report.Committed != 0 || report.Pushed {
		t.Fatalf("report = %+v, err = %v; want off-branch skip", report, err)
	}
	for _, ref := range []string{"main", "feature"} {
		if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", ref)); got != base {
			t.Fatalf("%s moved to %s; nothing may be committed anywhere", ref, got)
		}
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("index or worktree changed: %q -> %q", statusBefore, got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != base {
		t.Fatalf("remote main moved to %s", got)
	}
	assertNoLockLeft(t, local)

	// Back on main the edit syncs as usual.
	gitRun(t, local, "checkout", "main")
	report, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	if err != nil || report.Committed != 1 || !report.Pushed {
		t.Fatalf("retry on main: report = %+v, err = %v", report, err)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt"})
}

// The same `git checkout -b` right before the rebase makes Git rebase the new
// branch. repo-sync restores the branch and the worktree; main is untouched.
func TestGitSyncUndoesRebaseOnBranchCreatedMidCycle(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "mine.txt", "mine\n", "main work")
	tip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	runner := &hookedRunner{inner: execCommandRunner{}, command: "rebase", before: func() {
		gitRun(t, local, "checkout", "-b", "feature")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, false)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || report.Pushed {
		t.Fatalf("report = %+v, err = %v; want off-branch skip", report, err)
	}
	for _, ref := range []string{"main", "feature"} {
		if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", ref)); got != tip {
			t.Fatalf("%s moved to %s; the rebase must be undone", ref, got)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "remote.txt")); !os.IsNotExist(err) {
		t.Fatalf("worktree was left rebased: %v", err)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree is dirty after undo: %q", got)
	}
	assertNoLockLeft(t, local)

	gitRun(t, local, "checkout", "main")
	report, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, false)
	if err != nil || !report.Pushed {
		t.Fatalf("retry on main: report = %+v, err = %v", report, err)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt", "remote.txt"})
}

// A human starts a conflicting rebase at the very moment repo-sync is about
// to rebase. Git refuses one of the two. Either way, no rebase is aborted that
// the daemon did not start, and the human's edit survives.
func TestGitSyncNeverAbortsSomeoneElsesRebase(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "shared.txt", "local\n", "local work")
	localTip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	pushFromTeammate(t, remote, "shared.txt", "remote\n")
	var rebaseErr error
	runner := &hookedRunner{inner: execCommandRunner{}, command: "rebase", before: func() {
		rebaseErr = humanGit(local, "rebase", "origin/main")
		write(t, local, "shared.txt", "human partial resolution\n")
	}}
	_, err := gitSyncer{runner: runner}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	var skip *skipError
	if !errors.As(err, &skip) {
		t.Fatalf("error = %v, want a skip", err)
	}
	if rebaseErr == nil || !strings.Contains(rebaseErr.Error(), "index.lock") {
		t.Fatalf("the human rebase must be refused by Git's lock, got: %v", rebaseErr)
	}
	if data, _ := os.ReadFile(filepath.Join(local, "shared.txt")); string(data) != "human partial resolution\n" {
		t.Fatalf("the human's edit was discarded: %q", data)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main")); got != localTip {
		t.Fatalf("main moved to %s", got)
	}
	if _, err := os.Stat(filepath.Join(local, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Fatalf("a rebase was left in progress: %v", err)
	}
	assertNoLockLeft(t, local)
}

// Another Git process holds the index lock: repo-sync waits its turn and must
// not remove a lock it does not own.
func TestGitSyncSkipsWhileAnotherGitProcessHoldsTheIndex(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "mine.txt", "mine\n")
	lock := filepath.Join(local, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(skip.reason, "index.lock") || report.Committed != 0 {
		t.Fatalf("report = %+v, err = %v; want a lock skip", report, err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("someone else's lock was removed: %v", err)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); !strings.Contains(got, "?? mine.txt") {
		t.Fatalf("worktree was touched: %q", got)
	}
	if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, "mine.txt") {
		t.Fatal("nothing may be pushed while the checkout is in use")
	}
}

// A local branch named "origin/main" must never be mistaken for the remote.
func TestGitSyncUsesFullRemoteRefName(t *testing.T) {
	remote, local := makeGitFixture(t)
	gitRun(t, local, "checkout", "-b", "origin/main")
	writeAndCommit(t, local, "feature-only.txt", "unfinished feature\n", "feature work")
	gitRun(t, local, "checkout", "main")
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	write(t, local, "mine.txt", "mine\n")
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, true)
	if err != nil || !report.Pulled || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt", "remote.txt"})
}

// Between operations the checkout is not owned, so a human switch succeeds.
// The cycle then stops without touching the new branch, and syncs again once
// the human is back on the default branch.
func TestGitSyncStopsWhenBranchChangesBetweenOperations(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "mine.txt", "mine\n")
	var featureTip string
	runner := &hookedRunner{inner: execCommandRunner{}, command: "fetch", before: func() {
		featureTip = makeFeatureBranch(t, local)
		gitRun(t, local, "checkout", "feature")
	}}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: runner}.sync(context.Background(), repo, true)
	var skip *skipError
	if !errors.As(err, &skip) || !skip.offBranch || !strings.Contains(skip.reason, "branch is feature") {
		t.Fatalf("error = %v, report = %+v; want off-branch skip", err, report)
	}
	if report.Committed != 1 || report.Pushed {
		t.Fatalf("report = %+v; the commit on main must stay local", report)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "feature" {
		t.Fatalf("branch was switched to %q", got)
	}
	assertFeatureUntouched(t, remote, local, featureTip)
	assertNoLockLeft(t, local)

	gitRun(t, local, "checkout", "main")
	report, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, true)
	if err != nil || !report.Pushed {
		t.Fatalf("retry on main: report = %+v, err = %v", report, err)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt"})
	assertFeatureUntouched(t, remote, local, featureTip)
}

// makeFeatureBranch creates a feature branch with its own file, returns its
// commit, and goes back to main.
func makeFeatureBranch(t *testing.T, local string) string {
	t.Helper()
	gitRun(t, local, "checkout", "-q", "-b", "feature")
	writeAndCommit(t, local, "feature-only.txt", "unfinished feature\n", "feature work")
	tip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "feature"))
	gitRun(t, local, "checkout", "-q", "main")
	return tip
}

// pushFromTeammate lands a commit on the remote's main from a second clone.
func pushFromTeammate(t *testing.T, remote, name, contents string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	writeAndCommit(t, other, name, contents, "teammate: "+name)
	gitRun(t, other, "push", "-q", "origin", "main")
}

func assertMainSynced(t *testing.T, remote, local string, files []string) {
	t.Helper()
	if got := strings.Fields(gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")); !reflect.DeepEqual(got, files) {
		t.Fatalf("remote main files = %v, want %v", got, files)
	}
	if remoteMain, localMain := gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"), gitOutput(t, local, "rev-parse", "main"); remoteMain != localMain {
		t.Fatalf("remote main = %s, local main = %s", remoteMain, localMain)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "main" {
		t.Fatalf("checkout ended on %q", got)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree or index is dirty after sync: %q", got)
	}
	assertNoLockLeft(t, local)
}

func assertFeatureUntouched(t *testing.T, remote, local, featureTip string) {
	t.Helper()
	if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, "feature-only.txt") {
		t.Fatalf("feature work reached remote main:\n%s", tree)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "feature")); got != featureTip {
		t.Fatalf("feature branch moved from %s to %s", featureTip, got)
	}
	if got := gitOutput(t, local, "show", "feature:feature-only.txt"); got != "unfinished feature\n" {
		t.Fatalf("feature file changed: %q", got)
	}
	if output := gitOutput(t, local, "--git-dir", remote, "branch", "--list", "feature"); strings.TrimSpace(output) != "" {
		t.Fatalf("feature branch was published: %q", output)
	}
}

func assertNoLockLeft(t *testing.T, local string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(local, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("index.lock was left behind: %v", err)
	}
}

// A hook that switches branches in the middle of the rebase inherits our
// index file and gets past the lock. The rebase then finishes with foreign
// history on main. repo-sync must detect it, restore main, and publish nothing.
func TestGitSyncRestoresMainWhenHookSwitchesDuringRebase(t *testing.T) {
	remote, local := makeGitFixture(t)
	featureTip := makeFeatureBranch(t, local)
	writeAndCommit(t, local, "mine.txt", "mine\n", "main work")
	tip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	remoteTip := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))
	hook := filepath.Join(local, ".git", "hooks", "post-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nif test ! -e .git/fired; then touch .git/fired; git checkout -q feature; fi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, false)
	if err == nil || report.Pushed || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("report = %+v, err = %v; want a restore error and no push", report, err)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main")); got != tip {
		t.Fatalf("main = %s, want it restored to %s", got, tip)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteTip {
		t.Fatalf("remote main moved to %s", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "main" {
		t.Fatalf("checkout ended on %q", got)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree is dirty after restore: %q", got)
	}
	assertFeatureUntouched(t, remote, local, featureTip)
	assertNoLockLeft(t, local)

	// With the hook gone the next cycle syncs normally.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	report, err = gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repo, false)
	if err != nil || !report.Pushed {
		t.Fatalf("retry: report = %+v, err = %v", report, err)
	}
	assertMainSynced(t, remote, local, []string{"README.md", "mine.txt", "remote.txt"})
	assertFeatureUntouched(t, remote, local, featureTip)
}

// A post-commit hook switches to an existing feature branch right after the
// commit landed on main. Real daemon. The rollback for "commit landed on a
// new branch" must not fire: the pre-existing feature commit stays, main
// keeps the new commit, and nothing is pushed while the checkout is feature.
func TestGitSyncDaemonKeepsExistingFeatureWhenHookSwitchesAfterCommit(t *testing.T) {
	remote, local := makeGitFixture(t)
	featureTip := makeFeatureBranch(t, local)
	base := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	write(t, local, "mine.txt", "mine\n")
	hook := "#!/bin/sh\nif test ! -e .git/fired; then touch .git/fired; git checkout -q feature; fi\n"
	if err := os.WriteFile(filepath.Join(local, ".git", "hooks", "post-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	_, logs, _, done, cancel := startDaemon(t, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}})
	waitForOrStop(t, "sync outcome", func() bool {
		return strings.Contains(logs.String(), "skipped:") || strings.Contains(logs.String(), "sync failed") || strings.Contains(logs.String(), "pushed")
	}, done)
	stopDaemon(t, done, cancel)
	assertFeatureUntouched(t, remote, local, featureTip)
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main^")); got != base {
		t.Fatalf("main's parent = %s; the commit must have landed on main", got)
	}
	if got := gitOutput(t, local, "show", "main:mine.txt"); got != "mine\n" {
		t.Fatalf("main:mine.txt = %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "feature" {
		t.Fatalf("checkout is on %q; the hook's switch must be respected", got)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("index does not match the feature checkout: %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != base {
		t.Fatalf("remote main moved to %s while the checkout is feature", got)
	}
	assertNoLockLeft(t, local)
}

// The hook-switch-during-rebase scenario where the feature commit and the
// main commit share author, date, and message but differ in content. Commit
// metadata must not be what proves the rebase result is ours.
func TestGitSyncRestoresMainWhenHookSwitchesDuringRebaseWithSameMetadata(t *testing.T) {
	remote, local := makeGitFixture(t)
	gitRun(t, local, "checkout", "-q", "-b", "feature")
	writeAndCommit(t, local, "feature-only.txt", "unfinished feature\n", "work")
	gitRun(t, local, "commit", "-q", "--amend", "--no-edit", "--date=2000-01-01T00:00:00+00:00")
	featureTip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "feature"))
	gitRun(t, local, "checkout", "-q", "main")
	writeAndCommit(t, local, "mine.txt", "mine\n", "work")
	gitRun(t, local, "commit", "-q", "--amend", "--no-edit", "--date=2000-01-01T00:00:00+00:00")
	tip := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main"))
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	remoteTip := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))
	hook := "#!/bin/sh\nif test ! -e .git/fired; then touch .git/fired; git checkout -q feature; fi\n"
	if err := os.WriteFile(filepath.Join(local, ".git", "hooks", "post-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := gitSyncer{runner: execCommandRunner{}}.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	if err == nil || report.Pushed || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("report = %+v, err = %v; want a restore error and no push", report, err)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "main")); got != tip {
		t.Fatalf("main = %s, want it restored to %s", got, tip)
	}
	if got := gitOutput(t, local, "show", "main:mine.txt"); got != "mine\n" {
		t.Fatalf("main work lost: %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteTip {
		t.Fatalf("remote main moved to %s", got)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree is dirty after restore: %q", got)
	}
	assertFeatureUntouched(t, remote, local, featureTip)
	assertNoLockLeft(t, local)
}

// A post-rewrite hook switches to an existing feature branch once main's
// rebase has finished. Real daemon. Main keeps its rebased history, the
// feature branch keeps its own commit, and nothing is pushed off-branch.
func TestGitSyncDaemonKeepsExistingFeatureWhenHookSwitchesAfterRebase(t *testing.T) {
	remote, local := makeGitFixture(t)
	featureTip := makeFeatureBranch(t, local)
	writeAndCommit(t, local, "mine.txt", "mine\n", "main work")
	pushFromTeammate(t, remote, "remote.txt", "remote\n")
	remoteTip := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))
	if err := os.WriteFile(filepath.Join(local, ".git", "hooks", "post-rewrite"), []byte("#!/bin/sh\ngit checkout -q feature\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, logs, _, done, cancel := startDaemon(t, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}})
	waitForOrStop(t, "sync outcome", func() bool {
		return strings.Contains(logs.String(), "skipped:") || strings.Contains(logs.String(), "sync failed") || strings.Contains(logs.String(), "pushed")
	}, done)
	stopDaemon(t, done, cancel)
	assertFeatureUntouched(t, remote, local, featureTip)
	if got := gitOutput(t, local, "show", "main:mine.txt"); got != "mine\n" {
		t.Fatalf("main work lost: %q", got)
	}
	if _, err := runGit(context.Background(), execCommandRunner{}, local, "merge-base", "--is-ancestor", remoteTip, "main"); err != nil {
		t.Fatalf("main was not rebased onto the remote: %v", err)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "branch", "--show-current")); got != "feature" {
		t.Fatalf("checkout is on %q; the hook's switch must be respected", got)
	}
	if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
		t.Fatalf("index does not match the feature checkout: %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteTip {
		t.Fatalf("remote main moved to %s while the checkout is feature", got)
	}
	assertNoLockLeft(t, local)
}
