package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		return report, fmt.Errorf("worktree became dirty during commit; remote sync deferred")
	}
	for _, entry := range blocked {
		if entry.tracked() {
			return report, &skipError{reason: fmt.Sprintf("tracked secret file %s has local changes; revert it or run `repo-sync allow %s`", entry.path, entry.path)}
		}
	}

	remoteRef := repo.Remote + "/" + branch
	remoteBefore, _ := s.revParse(ctx, repo.Path, remoteRef)
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
		return report, err
	}
	head, _ := s.revParse(ctx, repo.Path, "HEAD")
	if head != remoteAfter {
		if _, err := runGit(ctx, s.runner, repo.Path, "push", repo.Remote, "HEAD:"+branch); err != nil {
			return report, err
		}
		report.Pushed = true
	}
	return report, nil
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
