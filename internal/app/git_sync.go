package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipError means this cycle did nothing and should simply be retried later.
// It is never a failure and never causes backoff or a notification by itself.
type skipError struct {
	reason    string
	offBranch bool // checkout is not on the remote default branch
}

func (e *skipError) Error() string { return e.reason }

type change struct {
	code string // two-letter porcelain status, "??" for untracked
	path string
}

func (c change) tracked() bool { return c.code != "??" }
func (c change) staged() bool  { return c.tracked() && c.code[0] != ' ' }

// deleted reports a plain staged deletion: the path left the index and
// nothing took its place in the worktree. `DA` (deleted, then recreated with
// intent-to-add) is a modification, not a deletion.
func (c change) deleted() bool { return c.code == "D " }

type syncReport struct {
	Scanned   bool     // the worktree was inspected; Blocked is meaningful
	Blocked   []string // secret paths left out of the commit
	Committed int      // files committed this cycle
	Pulled    bool     // remote default branch moved
	Pushed    bool     // local commits were pushed
	Branch    string   // remote default branch the checkout syncs with
	Checked   bool     // unpublished commits were inspected; Withheld is meaningful
	Withheld  []withheldSecret
}

// withheldSecret is a blocked path that an unpublished commit adds or changes.
// Such a push is held back until the user drops or allows the file.
type withheldSecret struct {
	Path    string
	Commit  string // abbreviated hash of the first commit touching the path
	Tracked bool   // the push destination already has the file; local edits to it never publish
	// OnSource means the commit is already on the fetch source and only the
	// separate push destination lacks it. No local reset removes such a commit.
	OnSource bool
}

func (w withheldSecret) String() string { return w.Path + " (" + w.Commit + ")" }

func withheldList(withheld []withheldSecret) string {
	parts := make([]string, 0, len(withheld))
	for _, entry := range withheld {
		parts = append(parts, entry.String())
	}
	return strings.Join(parts, ", ")
}

func (r syncReport) String() string {
	var parts []string
	if r.Committed > 0 {
		parts = append(parts, fmt.Sprintf("committed %d file(s)", r.Committed))
	}
	if r.Pulled {
		parts = append(parts, "pulled")
	}
	if r.Pushed {
		parts = append(parts, "pushed")
	}
	return strings.Join(parts, ", ")
}

type syncer interface {
	sync(ctx context.Context, repo repoConfig, commitLocal bool) (syncReport, error)
	changes(ctx context.Context, repo repoConfig) ([]change, []change, error)
}

type gitSyncer struct {
	runner commandRunner
}

func (s gitSyncer) sync(ctx context.Context, repo repoConfig, commitLocal bool) (syncReport, error) {
	var report syncReport
	branch, err := s.preflight(ctx, repo)
	if err != nil {
		return report, err
	}
	report.Branch = branch
	changed, blocked, err := s.changes(ctx, repo)
	if err != nil {
		return report, err
	}
	report.Scanned = true
	for _, entry := range blocked {
		report.Blocked = append(report.Blocked, entry.path)
	}
	if len(changed) > 0 {
		if !commitLocal {
			return report, nil
		}
		report.Committed, err = s.commit(ctx, repo, branch)
		if err != nil {
			return report, err
		}
	}
	// Never rebase with uncommitted changes. Hooks may alter the worktree, and a
	// tracked secret file with local edits is deliberately left uncommitted.
	changed, blocked, err = s.changes(ctx, repo)
	if err != nil {
		return report, err
	}
	if len(changed) > 0 {
		return report, &skipError{reason: "worktree changed during commit; will retry"}
	}
	for _, entry := range blocked {
		if entry.tracked() {
			return report, &skipError{reason: fmt.Sprintf("tracked secret file %s has local changes; revert it or run `repo-sync allow %s`", entry.path, entry.path)}
		}
	}

	// The full ref name: a local branch called "origin/main" must never win.
	remoteRef := "refs/remotes/" + repo.Remote + "/" + branch
	remoteBefore, _ := s.revParse(ctx, repo.Path, remoteRef)
	// Someone else may push between our fetch and our push. That is normal
	// teamwork, not a failure: fetch, rebase, and push once more right away.
	for attempt := 0; ; attempt++ {
		if _, err := runGit(ctx, s.runner, repo.Path, "fetch", repo.Remote); err != nil {
			return report, err
		}
		remoteAfter, _ := s.revParse(ctx, repo.Path, remoteRef)
		report.Pulled = remoteAfter != remoteBefore
		head, err := s.integrate(ctx, repo, branch, remoteRef, remoteAfter)
		if err != nil {
			return report, err
		}
		if head == "" {
			return report, errors.New("resolve " + branch + ": empty") // an empty source would delete the remote branch
		}
		if head == remoteAfter {
			report.Checked, report.Withheld = true, nil
			return report, nil
		}
		// Validate against what the push destination actually holds, and publish
		// only if it still holds that when the push lands. The fetched tip is
		// normally the destination tip; a remote with its own push URL is asked
		// directly, because history the fetch source has may be missing there.
		baseline, err := s.pushBaseline(ctx, repo, branch, remoteAfter)
		if err != nil {
			return report, err
		}
		if baseline != "" {
			if _, err := runGit(ctx, s.runner, repo.Path, "--no-replace-objects", "merge-base", "--is-ancestor", baseline, head); err != nil {
				return report, &skipError{reason: fmt.Sprintf("%s at the push destination (%s) is not part of local history; nothing is overwritten, will retry", branch, baseline[:7])}
			}
		}
		// Inspect the exact commits about to be published. Hooks, late staging,
		// and manual commits all bypass the worktree guard above.
		withheld, err := s.outgoingSecrets(ctx, repo, baseline, remoteAfter, head)
		if err != nil {
			return report, err
		}
		report.Checked, report.Withheld = true, withheld
		if len(withheld) > 0 {
			return report, &skipError{reason: fmt.Sprintf("push withheld: unpublished commits add or change secret file(s) %s; drop them from your local commits or run `repo-sync allow <path>`", withheldList(withheld))}
		}
		// Push the validated commit itself, not HEAD, so nothing that lands on
		// the branch after the check can ride along. The lease makes the update
		// conditional on the destination still being at the validated baseline;
		// with the ancestry check above it is always a fast-forward, never a
		// force-push. Submodule commits are only checked, never pushed for you.
		_, err = runGit(ctx, s.runner, repo.Path, "push", "--recurse-submodules=check",
			"--force-with-lease=refs/heads/"+branch+":"+baseline, repo.Remote, head+":refs/heads/"+branch)
		if paths := unpublishedSubmodules(err); len(paths) > 0 {
			return report, fmt.Errorf("submodule %s has commits that are not on its remote; check out a branch in it and run `repo-sync add <path>`, or push it yourself", strings.Join(paths, ", "))
		}
		if err == nil {
			report.Pushed = true
			return report, nil
		}
		if !pushRejected(err) {
			return report, err
		}
		if attempt >= 1 {
			return report, &skipError{reason: "remote " + branch + " keeps moving during push; will retry"}
		}
	}
}

// pushBaseline returns the commit the push destination holds for branch, or
// "" when the branch does not exist there. That is the fetched tip unless the
// remote pushes somewhere else than it fetches from.
func (s gitSyncer) pushBaseline(ctx context.Context, repo repoConfig, branch, fetched string) (string, error) {
	fetchURL, err := runGit(ctx, s.runner, repo.Path, "remote", "get-url", repo.Remote)
	if err != nil {
		return "", err
	}
	pushURL, err := runGit(ctx, s.runner, repo.Path, "remote", "get-url", "--push", repo.Remote)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(pushURL) == strings.TrimSpace(fetchURL) {
		return fetched, nil
	}
	output, err := runGit(ctx, s.runner, repo.Path, "ls-remote", "--heads", strings.TrimSpace(pushURL), "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return fields[0], nil
		}
	}
	return "", nil
}

// outgoingSecrets lists blocked paths that any commit in base..head adds or
// changes. Every commit is inspected on its own, so a secret committed and
// deleted again before the push is still caught. Deleting a blocked file, or
// leaving one the destination already has untouched, is fine. No content is
// read. Replacement refs are ignored: push publishes the real objects.
func (s gitSyncer) outgoingSecrets(ctx context.Context, repo repoConfig, base, fetched, head string) ([]withheldSecret, error) {
	span := head
	if base != "" {
		span = base + ".." + head
	}
	output, err := runGit(ctx, s.runner, repo.Path, "--no-replace-objects", "rev-list", "--reverse", span)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var withheld []withheldSecret
	for _, commit := range strings.Fields(output) {
		// -c makes a merge list only paths that differ from every parent, so
		// content merged in unchanged from the remote is not flagged again.
		files, err := runGit(ctx, s.runner, repo.Path, "--no-replace-objects", "diff-tree", "-r", "-c", "--root",
			"--no-commit-id", "--name-only", "--diff-filter=d", "--no-renames", "-z", commit)
		if err != nil {
			return nil, err
		}
		for _, file := range splitNUL(files) {
			if seen[file] || !isSecretPath(file, repo.Allow) {
				continue
			}
			seen[file] = true
			tracked, err := s.inTree(ctx, repo.Path, base, file)
			if err != nil {
				return nil, err
			}
			onSource := false
			if base != fetched && fetched != "" {
				_, err := runGit(ctx, s.runner, repo.Path, "--no-replace-objects", "merge-base", "--is-ancestor", commit, fetched)
				onSource = err == nil
			}
			withheld = append(withheld, withheldSecret{Path: file, Commit: commit[:7], Tracked: tracked, OnSource: onSource})
		}
	}
	return withheld, nil
}

// inTree reports whether tree (a commit, or "" for none) contains file.
func (s gitSyncer) inTree(ctx context.Context, path, tree, file string) (bool, error) {
	if tree == "" {
		return false, nil
	}
	output, err := runGit(ctx, s.runner, path, "--no-replace-objects", "--literal-pathspecs", "ls-tree", "-z", tree, "--", file)
	if err != nil {
		return false, err
	}
	for _, entry := range splitNUL(output) {
		if _, name, ok := strings.Cut(entry, "\t"); ok && name == file {
			return true, nil
		}
	}
	return false, nil
}

// unpublishedSubmodules extracts the submodule paths a push refused to
// publish ahead of, from `git push --recurse-submodules=check` output.
func unpublishedSubmodules(err error) []string {
	if err == nil {
		return nil
	}
	_, rest, found := strings.Cut(err.Error(), "not be found on any remote:")
	if !found {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(rest, "\n")[1:] {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		paths = append(paths, strings.TrimSpace(line))
	}
	return paths
}

// rebase replays the local commits on top of remoteRef and aborts on conflict.
//
// Git starts a rebase by checking out remoteRef. It refuses to overwrite an
// untracked file, but silently overwrites an ignored one. A file the user
// untracked but kept on disk (`git rm --cached .env` plus a .gitignore entry)
// would be replaced by the remote's copy and then deleted when the local
// deletion replays or the rebase aborts. Nothing can put such a file back
// safely afterwards (an editor may have written newer contents meanwhile), so
// the rebase is refused instead and the repository waits for the user.
//
// It runs with the checkout owned, so nobody else can start a rebase
// meanwhile: any rebase state left behind is ours to abort. --no-autostash
// keeps a dirty worktree a clean skip instead of a stash.
func (s gitSyncer) rebase(ctx context.Context, c *checkout, remoteRef string) error {
	if file, err := s.ignoredFileInTheWay(ctx, c, remoteRef); err != nil {
		return err
	} else if file != "" {
		return fmt.Errorf("ignored file %s would be overwritten by rebasing onto %s; move it aside until the repository has synced", file, remoteRef)
	}
	_, err := c.git(ctx, "", "rebase", "--no-autostash", remoteRef)
	if err == nil {
		return nil
	}
	if s.rebaseInProgress(ctx, c.path) {
		_, _ = c.git(ctx, "", "rebase", "--abort")
		return &skipError{reason: "rebase conflict with " + remoteRef + "; rebase aborted, will retry"}
	}
	// A file changed between our clean check and the rebase. Nothing is
	// broken; the next cycle simply commits it first.
	if strings.Contains(err.Error(), "cannot rebase:") {
		return &skipError{reason: "worktree changed before rebase; will retry"}
	}
	return err
}

// ignoredFileInTheWay returns an ignored file that the rebase's checkout of
// remoteRef would overwrite: it exists on disk at a path remoteRef has and
// HEAD does not. When remoteRef is already part of HEAD's history the rebase
// checks nothing out, so there is nothing in the way.
func (s gitSyncer) ignoredFileInTheWay(ctx context.Context, c *checkout, remoteRef string) (string, error) {
	remoteHead, err := s.revParse(ctx, c.path, remoteRef)
	if err != nil {
		return "", err
	}
	base, err := c.git(ctx, "", "merge-base", "HEAD", remoteRef)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(base) == remoteHead {
		return "", nil
	}
	output, err := c.git(ctx, "", "diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", "HEAD", remoteRef)
	if err != nil {
		return "", err
	}
	var present []string
	for _, name := range splitNUL(output) {
		if _, err := os.Lstat(filepath.Join(c.path, name)); err == nil {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	args := append([]string{"--literal-pathspecs", "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--"}, present...)
	output, err = c.git(ctx, "", args...)
	if err != nil {
		return "", err
	}
	if ignored := splitNUL(output); len(ignored) > 0 {
		return ignored[0], nil
	}
	return "", nil
}

// Push rejections Git reports when the remote branch moved after our fetch.
// The reason sits in parentheses at the end of a "[rejected]" line ("stale
// info" is our own lease finding the destination elsewhere than validated),
// or the remote's compare-and-swap names the two object ids it saw. Anything else
// that mentions these words (a path, a branch name, a hook message) is not
// evidence of a concurrent push.
var (
	pushRaceReason = regexp.MustCompile(`(?m)^ ! \[rejected\] .*\((fetch first|non-fast-forward|stale info)\)\s*$`)
	pushRaceSwap   = regexp.MustCompile(`cannot lock ref '[^'\n]*': is at [0-9a-f]{7,64} but expected [0-9a-f]{7,64}`)
)

// pushRejected reports that a concurrent push won the race, so fetching and
// rebasing once more will most likely succeed. Every other push failure, such
// as a stuck lock file, a permission problem, or a hook refusal, does not go
// away by retrying immediately and stays a normal error: it backs off and
// notifies once it has lasted long enough.
func pushRejected(err error) bool {
	message := err.Error()
	return pushRaceReason.MatchString(message) || pushRaceSwap.MatchString(message)
}

// preflight checks that the repository is safe to touch and returns the
// remote default branch the checkout must be on.
func (s gitSyncer) preflight(ctx context.Context, repo repoConfig) (string, error) {
	busy, operation, err := s.inProgress(ctx, repo.Path)
	if err != nil {
		return "", err
	}
	if busy {
		return "", &skipError{reason: operation + " is in progress; repository was not touched"}
	}
	branch := s.defaultBranch(ctx, repo)
	if err := s.checkBranch(ctx, repo, branch); err != nil {
		return "", err
	}
	return branch, nil
}

// checkBranch confirms HEAD is the default branch. Outside the checkout lock
// this is only an early, cheap skip; the authoritative check happens again
// once the checkout is owned.
func (s gitSyncer) checkBranch(ctx context.Context, repo repoConfig, branch string) error {
	current, err := s.currentBranch(ctx, repo.Path)
	if err != nil {
		return err
	}
	if current == "" {
		return &skipError{reason: "HEAD is detached; only " + branch + " is synced", offBranch: true}
	}
	if current != branch {
		return &skipError{reason: fmt.Sprintf("branch is %s; only %s is synced", current, branch), offBranch: true}
	}
	return nil
}

// currentBranch returns the branch HEAD points at, or "" when HEAD is
// detached. Git exits 1 for a detached HEAD; anything else is a real failure.
func (s gitSyncer) currentBranch(ctx context.Context, path string) (string, error) {
	output, err := runGit(ctx, s.runner, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// defaultBranch reads the remote's default branch from refs/remotes/<remote>/HEAD
// and falls back to main when it is not recorded locally.
func (s gitSyncer) defaultBranch(ctx context.Context, repo repoConfig) string {
	output, err := runGit(ctx, s.runner, repo.Path, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+repo.Remote+"/HEAD")
	if err != nil {
		return "main"
	}
	name := strings.TrimPrefix(strings.TrimSpace(output), repo.Remote+"/")
	if name == "" {
		return "main"
	}
	return name
}

// changes returns the syncable changes and, separately, the secret-guarded
// ones. Files that are already ignored by Git never appear. A staged deletion
// of a secret path (`git rm .env`, or the old path of `git mv .env notes.txt`)
// is syncable: it publishes nothing, and holding it back would leave the
// repository stuck behind a tracked secret that no longer exists on disk.
func (s gitSyncer) changes(ctx context.Context, repo repoConfig) ([]change, []change, error) {
	output, err := runGit(ctx, s.runner, repo.Path, statusArgs...)
	if err != nil {
		return nil, nil, err
	}
	allowed, blocked := splitChanges(output, repo.Allow)
	return allowed, blocked, nil
}

var statusArgs = []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}

func splitChanges(status string, allow []string) (allowed, blocked []change) {
	for _, entry := range parseStatus(status) {
		if isSecretPath(entry.path, allow) && !entry.deleted() {
			blocked = append(blocked, entry)
		} else {
			allowed = append(allowed, entry)
		}
	}
	return allowed, blocked
}

// parseStatus reads `git status --porcelain=v1 -z`. Renames and copies carry a
// second NUL-terminated field with the original path. For a rename that path
// is a deletion in the same column (staged for `R `, worktree-only for ` R`);
// a copy leaves its original untouched.
func parseStatus(output string) []change {
	fields := strings.Split(output, "\x00")
	var result []change
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if len(field) < 4 {
			continue
		}
		code, file := field[:2], field[3:]
		result = append(result, change{code: code, path: file})
		if !strings.ContainsAny(code, "RC") || i+1 >= len(fields) || fields[i+1] == "" {
			continue
		}
		i++
		switch {
		case code[0] == 'R':
			result = append(result, change{code: "D ", path: fields[i]})
		case code[1] == 'R':
			result = append(result, change{code: " D", path: fields[i]})
		}
	}
	return result
}

// commit stages every syncable change and commits it on the default branch.
// It owns the checkout for the whole operation, so a human Git command in the
// meantime is refused by Git itself instead of racing with us. Blocked secret
// files that a user staged by hand are unstaged first so they never reach the
// remote.
func (s gitSyncer) commit(ctx context.Context, repo repoConfig, branch string) (int, error) {
	c, err := s.lockCheckout(ctx, repo, branch)
	if err != nil {
		return 0, err
	}
	defer c.discard()
	changed, blocked, err := c.changes(ctx)
	if err != nil {
		return 0, err
	}
	if len(changed) == 0 {
		return 0, nil
	}
	for _, entry := range blocked {
		if entry.staged() {
			if _, err := c.git(ctx, "", "--literal-pathspecs", "reset", "-q", "--", entry.path); err != nil {
				return 0, err
			}
		}
	}
	var spec strings.Builder
	for _, entry := range changed {
		// A staged deletion, including the old path of a staged rename, has
		// already left the index and the worktree. There is nothing to add
		// for it, and `git add` would reject the missing path.
		if entry.deleted() {
			continue
		}
		spec.WriteString(entry.path)
		spec.WriteByte(0)
	}
	// With an empty pathspec `git add -A` would stage the whole worktree,
	// including files the guard just unstaged. Staged-only changes need no add.
	if spec.Len() > 0 {
		if _, err := c.git(ctx, spec.String(), "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			// A file listed by status vanished before add saw it (editors and
			// agents create and delete files quickly). Retry from a fresh status.
			if strings.Contains(err.Error(), "did not match any files") {
				return 0, &skipError{reason: "files changed while staging; will retry"}
			}
			return 0, err
		}
	}
	output, err := c.git(ctx, "", "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return 0, err
	}
	files := splitNUL(output)
	if len(files) == 0 {
		return 0, nil
	}
	sort.Strings(files)
	var message strings.Builder
	fmt.Fprintf(&message, "repo-sync: sync %d changed file", len(files))
	if len(files) != 1 {
		message.WriteByte('s')
	}
	message.WriteString("\n\nChanged files:\n")
	for _, file := range files {
		fmt.Fprintf(&message, "- %s\n", file)
	}
	if _, err := c.git(ctx, message.String(), "commit", "-q", "-F", "-"); err != nil {
		return 0, err
	}
	// Where did the commit land? Only the default branch's own movement proves
	// it; HEAD may have been moved afterwards by a hook (hooks inherit our
	// index file and get past the lock). The locked index is installed either
	// way: it reflects everything the commit and its hooks did.
	tip, err := s.revParse(ctx, repo.Path, "refs/heads/"+branch)
	if err != nil {
		return 0, err
	}
	if tip != c.base {
		if err := c.install(); err != nil {
			return 0, err
		}
		return len(files), nil
	}
	// `git checkout -b` at the same commit needs no index lock, so the commit
	// landed on that brand-new branch instead. That is provably our commit
	// only if its parent is the tip we started from; then put the branch back
	// exactly where it was and leave the edits in the worktree, uncommitted.
	head, err := c.head(ctx)
	if err != nil {
		return 0, err
	}
	commit, err := s.revParse(ctx, repo.Path, "HEAD")
	if err != nil {
		return 0, err
	}
	if parent, _ := s.revParse(ctx, repo.Path, commit+"^"); head != "" && head != branch && parent == c.base {
		if _, err := c.git(ctx, "", "update-ref", "refs/heads/"+head, c.base, commit); err != nil {
			return 0, err
		}
		return 0, &skipError{reason: fmt.Sprintf("branch changed to %s during commit; commit undone, only %s is synced", head, branch), offBranch: true}
	}
	return 0, errors.Join(fmt.Errorf("commit did not land on %s (HEAD is %q); a hook changed the checkout?", branch, head), c.install())
}

// integrate rebases the default branch onto the fetched remote while owning
// the checkout, and returns the verified tip of the branch to publish. Only
// the branch's own movement proves the rebase acted on it; HEAD's reflog
// tells who else moved HEAD, and when.
func (s gitSyncer) integrate(ctx context.Context, repo repoConfig, branch, remoteRef, remoteAfter string) (string, error) {
	if tip, _ := s.revParse(ctx, repo.Path, "refs/heads/"+branch); tip == remoteAfter {
		return tip, nil
	}
	c, err := s.lockCheckout(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	defer c.discard()
	if c.base == remoteAfter {
		return c.base, nil
	}
	reflogBefore := s.headReflog(ctx, repo.Path)
	if err := s.rebase(ctx, c, remoteRef); err != nil {
		return "", err
	}
	tip, err := s.revParse(ctx, repo.Path, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	head, err := c.head(ctx)
	if err != nil {
		return "", err
	}
	reflogAfter := s.headReflog(ctx, repo.Path)
	entries := reflogAfter[:max(0, len(reflogAfter)-len(reflogBefore))]
	// Split what moved HEAD, newest first: after the rebase finished, during
	// it, or before it started. The rebase's own entries all say "rebase".
	var after, during, before []reflogEntry
	bucket := &after
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.subject, "rebase (finish)"):
			bucket = &during
		case strings.HasPrefix(entry.subject, "rebase (start)"):
			bucket = &before
		case strings.HasPrefix(entry.subject, "rebase"):
		default:
			*bucket = append(*bucket, entry)
		}
	}
	switch {
	case tip != c.base:
		// The rebase acted on the default branch. Anything else that moved
		// HEAD while it ran (a hook that inherited our index file) means Git
		// finished with foreign history on the branch: put it back.
		foreign := ""
		if len(during) > 0 {
			foreign = during[0].subject
		} else if len(entries) == 0 {
			if foreign, err = s.foreignCommits(ctx, repo.Path, remoteAfter, c.base, tip); err != nil {
				return "", err
			}
		}
		if foreign != "" {
			if _, err := c.git(ctx, "", "update-ref", "refs/heads/"+branch, c.base, tip); err != nil {
				return "", err
			}
			if _, err := c.git(ctx, "", "read-tree", "-m", "-u", tip, c.base); err != nil {
				return "", err
			}
			return "", fmt.Errorf("something other than the rebase changed the checkout mid-rebase (%s); %s restored", foreign, branch)
		}
		if head != branch {
			// A hook switched branches once the rebase was complete. The
			// branch is fine; it is just no longer checked out.
			return "", errors.Join(&skipError{reason: fmt.Sprintf("branch changed to %s after rebase; only %s is synced", head, branch), offBranch: true}, c.install())
		}
		return tip, c.install()
	case head != branch:
		// The default branch did not move, so Git rebased the branch HEAD was
		// on. Undo that only when the reflog proves it was created at our
		// starting commit right before the rebase (`git checkout -b` needs no
		// index lock); a branch with history of its own is never touched.
		if len(during) == 0 && len(after) == 0 && len(before) == 1 && before[0].hash == c.base {
			rebased, err := s.revParse(ctx, repo.Path, "HEAD")
			if err != nil {
				return "", err
			}
			if _, err := c.git(ctx, "", "read-tree", "-m", "-u", rebased, c.base); err != nil {
				return "", err
			}
			if _, err := c.git(ctx, "", "update-ref", "refs/heads/"+head, c.base, rebased); err != nil {
				return "", err
			}
			return "", &skipError{reason: fmt.Sprintf("branch changed to %s during rebase; rebase undone, only %s is synced", head, branch), offBranch: true}
		}
		return "", errors.Join(&skipError{reason: fmt.Sprintf("branch changed to %s during rebase; only %s is synced", head, branch), offBranch: true}, c.install())
	default:
		return tip, c.install()
	}
}

type reflogEntry struct{ hash, subject string }

// headReflog returns HEAD's reflog, newest first. A repository without
// reflogs yields nothing, and the callers fall back to commit identity.
func (s gitSyncer) headReflog(ctx context.Context, path string) []reflogEntry {
	output, err := runGit(ctx, s.runner, path, "reflog", "show", "--format=%H%x00%gs", "HEAD")
	if err != nil {
		return nil
	}
	var entries []reflogEntry
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if hash, subject, ok := strings.Cut(line, "\x00"); ok {
			entries = append(entries, reflogEntry{hash: hash, subject: subject})
		}
	}
	return entries
}

// foreignCommits returns the subject of a commit in remote..after that is not
// a replay of a commit in remote..before, or "" when every commit matches. A
// rebase keeps author, author date, and message verbatim. This is only the
// fallback for repositories without reflogs; metadata alone is not identity.
func (s gitSyncer) foreignCommits(ctx context.Context, path, remote, before, after string) (string, error) {
	identities := func(tip string) (map[string]int, error) {
		output, err := runGit(ctx, s.runner, path, "log", "--format=%x1e%an%x00%ae%x00%aI%x00%B", remote+".."+tip)
		if err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, record := range strings.Split(output, "\x1e") {
			if record != "" {
				counts[record]++
			}
		}
		return counts, nil
	}
	expected, err := identities(before)
	if err != nil {
		return "", err
	}
	actual, err := identities(after)
	if err != nil {
		return "", err
	}
	for record, count := range actual {
		if expected[record] < count {
			fields := strings.SplitN(record, "\x00", 4)
			subject, _, _ := strings.Cut(fields[len(fields)-1], "\n")
			return subject, nil
		}
	}
	return "", nil
}

func (s gitSyncer) revParse(ctx context.Context, path, ref string) (string, error) {
	output, err := runGit(ctx, s.runner, path, "rev-parse", "--verify", "--quiet", ref)
	return strings.TrimSpace(output), err
}

func splitNUL(value string) []string {
	parts := strings.Split(value, "\x00")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (s gitSyncer) inProgress(ctx context.Context, path string) (bool, string, error) {
	gitDir, err := runGit(ctx, s.runner, path, "rev-parse", "--git-dir")
	if err != nil {
		return false, "", err
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	checks := []struct {
		name  string
		paths []string
	}{
		{"merge", []string{"MERGE_HEAD"}},
		{"rebase", []string{"rebase-merge", "rebase-apply"}},
		{"cherry-pick", []string{"CHERRY_PICK_HEAD"}},
		{"revert", []string{"REVERT_HEAD"}},
		{"bisect", []string{"BISECT_LOG"}},
	}
	for _, check := range checks {
		for _, name := range check.paths {
			_, statErr := os.Stat(filepath.Join(gitDir, name))
			if statErr == nil {
				return true, check.name, nil
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				return false, "", statErr
			}
		}
	}
	return false, "", nil
}

func (s gitSyncer) rebaseInProgress(ctx context.Context, path string) bool {
	busy, operation, err := s.inProgress(ctx, path)
	return err == nil && busy && operation == "rebase"
}
