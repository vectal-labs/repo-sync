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
		if _, err := runGit(ctx, s.runner, repo.Path, "rebase", remoteRef); err != nil {
			if s.rebaseInProgress(ctx, repo.Path) {
				_, _ = runGit(ctx, s.runner, repo.Path, "rebase", "--abort")
				return report, &skipError{reason: "rebase conflict with " + remoteRef + "; rebase aborted, will retry"}
			}
			// A file changed between our clean check and the rebase. Nothing is
			// broken; the next cycle simply commits it first.
			if strings.Contains(err.Error(), "cannot rebase:") {
				return report, &skipError{reason: "worktree changed before rebase; will retry"}
			}
			return report, err
		}
		head, err := s.revParse(ctx, repo.Path, "HEAD")
		if err != nil || head == "" {
			return report, fmt.Errorf("resolve HEAD: %w", err) // an empty source would delete the remote branch
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
// ones. Files that are already ignored by Git never appear.
func (s gitSyncer) changes(ctx context.Context, repo repoConfig) ([]change, []change, error) {
	output, err := runGit(ctx, s.runner, repo.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, nil, err
	}
	var allowed, blocked []change
	for _, entry := range parseStatus(output) {
		if isSecretPath(entry.path, repo.Allow) {
			blocked = append(blocked, entry)
		} else {
			allowed = append(allowed, entry)
		}
	}
	return allowed, blocked, nil
}

// parseStatus reads `git status --porcelain=v1 -z`. Renames and copies carry a
// second NUL-terminated field with the original path, which counts as deleted.
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
		if (code[0] == 'R' || code[0] == 'C') && i+1 < len(fields) && fields[i+1] != "" {
			i++
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
		spec.WriteString(entry.path)
		spec.WriteByte(0)
	}
	if _, err := s.runner.run(ctx, path, spec.String(), "git", "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		// A file listed by status vanished before add saw it (editors and
		// agents create and delete files quickly). Retry from a fresh status.
		if strings.Contains(err.Error(), "did not match any files") {
			return 0, &skipError{reason: "files changed while staging; will retry"}
		}
		return 0, err
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
