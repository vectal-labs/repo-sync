package app

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		report.Committed, err = s.commit(ctx, repo, changed)
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

	remoteRef := repo.Remote + "/" + branch
	remoteBefore, _ := s.revParse(ctx, repo.Path, remoteRef)
	// Someone else may push between our fetch and our push. That is normal
	// teamwork, not a failure: fetch, rebase, and push once more right away.
	for attempt := 0; ; attempt++ {
		if _, err := runGit(ctx, s.runner, repo.Path, "fetch", repo.Remote); err != nil {
			return report, err
		}
		remoteAfter, _ := s.revParse(ctx, repo.Path, remoteRef)
		report.Pulled = remoteAfter != remoteBefore
		if err := s.rebase(ctx, repo.Path, remoteRef); err != nil {
			return report, err
		}
		head, _ := s.revParse(ctx, repo.Path, "HEAD")
		if head == remoteAfter {
			return report, nil
		}
		_, err := runGit(ctx, s.runner, repo.Path, "push", repo.Remote, "HEAD:"+branch)
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

// rebase replays the local commits on top of remoteRef and aborts on conflict.
//
// Git starts a rebase by checking out remoteRef. It refuses to overwrite an
// untracked file, but silently overwrites an ignored one. A file the user
// untracked but kept on disk (`git rm --cached .env` plus a .gitignore entry)
// would be replaced by the remote's copy and then deleted when the local
// deletion replays or the rebase aborts. Nothing can put such a file back
// safely afterwards (an editor may have written newer contents meanwhile), so
// the rebase is refused instead and the repository waits for the user.
func (s gitSyncer) rebase(ctx context.Context, path, remoteRef string) error {
	if file, err := s.ignoredFileInTheWay(ctx, path, remoteRef); err != nil {
		return err
	} else if file != "" {
		return fmt.Errorf("ignored file %s would be overwritten by rebasing onto %s; move it aside until the repository has synced", file, remoteRef)
	}
	_, err := runGit(ctx, s.runner, path, "rebase", remoteRef)
	if err == nil {
		return nil
	}
	if s.rebaseInProgress(ctx, path) {
		_, _ = runGit(ctx, s.runner, path, "rebase", "--abort")
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
func (s gitSyncer) ignoredFileInTheWay(ctx context.Context, path, remoteRef string) (string, error) {
	remoteHead, err := s.revParse(ctx, path, remoteRef)
	if err != nil {
		return "", err
	}
	base, err := runGit(ctx, s.runner, path, "merge-base", "HEAD", remoteRef)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(base) == remoteHead {
		return "", nil
	}
	output, err := runGit(ctx, s.runner, path, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", "HEAD", remoteRef)
	if err != nil {
		return "", err
	}
	var present []string
	for _, name := range splitNUL(output) {
		if _, err := os.Lstat(filepath.Join(path, name)); err == nil {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	args := append([]string{"--literal-pathspecs", "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--"}, present...)
	output, err = runGit(ctx, s.runner, path, args...)
	if err != nil {
		return "", err
	}
	if ignored := splitNUL(output); len(ignored) > 0 {
		return ignored[0], nil
	}
	return "", nil
}

// Push rejections Git reports when the remote branch moved after our fetch.
// The reason sits in parentheses at the end of a "[rejected]" line, or the
// remote's compare-and-swap names the two object ids it saw. Anything else
// that mentions these words (a path, a branch name, a hook message) is not
// evidence of a concurrent push.
var (
	pushRaceReason = regexp.MustCompile(`(?m)^ ! \[rejected\] .*\((fetch first|non-fast-forward)\)\s*$`)
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
	current, err := runGit(ctx, s.runner, repo.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", &skipError{reason: "HEAD is detached; only " + branch + " is synced", offBranch: true}
	}
	current = strings.TrimSpace(current)
	if current != branch {
		return "", &skipError{reason: fmt.Sprintf("branch is %s; only %s is synced", current, branch), offBranch: true}
	}
	return branch, nil
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
	output, err := runGit(ctx, s.runner, repo.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, nil, err
	}
	var allowed, blocked []change
	for _, entry := range parseStatus(output) {
		if isSecretPath(entry.path, repo.Allow) && !entry.deleted() {
			blocked = append(blocked, entry)
		} else {
			allowed = append(allowed, entry)
		}
	}
	return allowed, blocked, nil
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

// commit stages exactly the given paths and commits them. Blocked secret files
// that a user staged by hand are unstaged first so they never reach the remote.
func (s gitSyncer) commit(ctx context.Context, repo repoConfig, changed []change) (int, error) {
	path := repo.Path
	_, blocked, err := s.changes(ctx, repo)
	if err != nil {
		return 0, err
	}
	for _, entry := range blocked {
		if entry.staged() {
			if _, err := runGit(ctx, s.runner, path, "--literal-pathspecs", "reset", "-q", "--", entry.path); err != nil {
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
		if _, err := s.runner.run(ctx, path, spec.String(), "git", "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			// A file listed by status vanished before add saw it (editors and
			// agents create and delete files quickly). Retry from a fresh status.
			if strings.Contains(err.Error(), "did not match any files") {
				return 0, &skipError{reason: "files changed while staging; will retry"}
			}
			return 0, err
		}
	}
	output, err := runGit(ctx, s.runner, path, "diff", "--cached", "--name-only", "-z")
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
	if _, err := s.runner.run(ctx, path, message.String(), "git", "commit", "-q", "-F", "-"); err != nil {
		return 0, err
	}
	return len(files), nil
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
