package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Git command that is killed instead of stopped leaves index.lock behind,
// and Git then refuses to touch the checkout forever. Real Git work never
// holds the lock for minutes without a Git process running in the repository,
// so such a lock is abandoned and removed. A lock that a running Git process
// may own, such as `git commit` waiting for its editor, is never removed; it is
// reported as stuck once it is an hour old.
const (
	abandonedLockAfter = 10 * time.Minute
	staleLockAfter     = time.Hour
)

// checkout is repo-sync's ownership of a working tree for one operation.
//
// It takes Git's own index lock: `<git-dir>/index.lock`, created exclusively
// the way every Git command does. While we hold it, any other Git process that
// would change the checkout (checkout, switch, commit, merge, rebase, reset,
// cherry-pick) is refused by Git with its usual "index.lock exists" message
// and nothing in the worktree changes under our feet. Our own commands keep
// running because they use the lock file itself as their index, through
// GIT_INDEX_FILE. The file starts as a copy of the real index and, when we are
// done, is renamed over it, which is exactly how Git installs a new index.
//
// Git skips the index lock for one thing: creating a branch at the current
// commit (`git checkout -b`). The callers handle that single case by checking
// HEAD after the mutation and undoing it with compare-and-swap ref updates.
type checkout struct {
	s      gitSyncer
	path   string
	allow  []string
	branch string
	base   string // commit of refs/heads/<branch> when the lock was taken
	index  string
	lock   string
	done   bool
}

func lockSkip(lock, worktree string, now time.Time) *skipError {
	info, err := os.Stat(lock)
	if err != nil {
		return &skipError{reason: "another git process is using the checkout (index.lock exists); will retry"}
	}
	age, since := now.Sub(info.ModTime()), info.ModTime().Local().Format("2006-01-02 15:04")
	if age >= abandonedLockAfter && !gitBusy(worktree, filepath.Dir(lock)) && os.Remove(lock) == nil {
		return &skipError{reason: fmt.Sprintf("removed %s, abandoned by a stopped Git command since %s; will retry", lock, since)}
	}
	if age >= staleLockAfter {
		return &skipError{reason: fmt.Sprintf("Git's index.lock has existed since %s and a Git process may still be using it; finish or quit that Git command, or delete %s", since, lock), stuck: true}
	}
	return &skipError{reason: "another git process is using the checkout (index.lock exists); will retry"}
}

// gitBusy reports whether a Git process may be working in one of dirs. When
// that cannot be determined, it assumes one is.
func gitBusy(dirs ...string) bool {
	for i, dir := range dirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return true
		}
		dirs[i] = resolved
	}
	busy, err := gitRunningIn(dirs...)
	return busy || err != nil
}

// lockCheckout takes the lock and then verifies, now that nothing can change,
// that no Git operation is in progress and HEAD is the default branch.
func (s gitSyncer) lockCheckout(ctx context.Context, repo repoConfig, branch string) (*checkout, error) {
	output, err := runGit(ctx, s.runner, repo.Path, "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	index := strings.TrimSpace(output)
	if !filepath.IsAbs(index) {
		index = filepath.Join(repo.Path, index)
	}
	current, err := os.ReadFile(index)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	c := &checkout{s: s, path: repo.Path, allow: repo.Allow, branch: branch, index: index, lock: index + ".lock"}
	file, err := os.OpenFile(c.lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, lockSkip(c.lock, repo.Path, time.Now())
		}
		return nil, err
	}
	_, err = file.Write(current)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		c.discard()
		return nil, fmt.Errorf("copy index: %w", err)
	}
	busy, operation, err := s.inProgress(ctx, repo.Path)
	if err == nil && busy {
		err = &skipError{reason: operation + " is in progress; repository was not touched"}
	}
	if err == nil {
		err = s.checkBranch(ctx, repo, branch)
	}
	if err == nil {
		c.base, err = s.revParse(ctx, repo.Path, "refs/heads/"+branch)
	}
	if err != nil {
		c.discard()
		return nil, err
	}
	return c, nil
}

// git runs a Git command against the locked index.
func (c *checkout) git(ctx context.Context, stdin string, args ...string) (string, error) {
	return c.s.runner.run(ctx, c.path, stdin, "env", append([]string{"GIT_INDEX_FILE=" + c.lock, "git"}, args...)...)
}

func (c *checkout) changes(ctx context.Context) ([]change, []change, error) {
	output, err := c.git(ctx, "", statusArgs...)
	if err != nil {
		return nil, nil, err
	}
	allowed, blocked := splitChanges(output, c.allow)
	return allowed, blocked, nil
}

func (c *checkout) head(ctx context.Context) (string, error) {
	return c.s.currentBranch(ctx, c.path)
}

// install makes the locked index the real one. Required after any command
// that changed the index or moved HEAD; a plain discard would leave the
// worktree looking modified in reverse.
func (c *checkout) install() error {
	c.done = true
	if err := os.Rename(c.lock, c.index); err != nil {
		return fmt.Errorf("install index: %w", err)
	}
	return nil
}

// discard releases the lock without touching the real index. Safe after
// install, and after any failure that left the real index authoritative.
func (c *checkout) discard() {
	if c.done {
		return
	}
	c.done = true
	_ = os.Remove(c.lock)
}
