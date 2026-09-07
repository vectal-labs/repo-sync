package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

func checkGitTools(ctx context.Context, runner commandRunner) error {
	if _, err := runner.run(ctx, "", "", "git", "--version"); err != nil {
		return fmt.Errorf("Git is unavailable to the background service. Install it with `xcode-select --install` or `brew install git`, then run `repo-sync setup` again: %w", err)
	}
	return nil
}

type repoAccessCheck struct {
	remoteURL string // URL of the operation that failed, including a separate push URL
	warning   string
	err       error
}

// verifyRepositories checks identity and transport access without changing the
// worktree, index, local refs, or remote. A dry-run cannot prove server policies.
func verifyRepositories(ctx context.Context, runner commandRunner, repos []repoConfig, in io.Reader, out io.Writer) error {
	if len(repos) == 0 {
		return nil
	}
	fmt.Fprintf(out, "\nChecking Git identity, fetch access, and push access for %d repositories...\n", len(repos))
	checks := checkRepositories(ctx, runner, repos)
	for _, check := range checks {
		if check.err != nil && isGitHubHTTPS(check.remoteURL) && isAuthenticationFailure(check.err) {
			if err := configureGitHubCredentials(ctx, runner, in, out); err != nil {
				return err
			}
			checks = checkRepositories(ctx, runner, repos)
			break
		}
	}

	var failures []string
	for i, check := range checks {
		if check.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", repos[i].Name, check.err))
		}
		if check.warning != "" {
			fmt.Fprintf(out, "Warning for %s: %s\n", repos[i].Name, check.warning)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("background Git access failed; config and service were not changed:\n- %s", strings.Join(failures, "\n- "))
	}
	fmt.Fprintln(out, "Background Git access verified with a fetch and push dry-run.")
	fmt.Fprintln(out, "A dry-run cannot verify server branch rules or hooks. An actual push may still be rejected.")
	return nil
}

func checkRepositories(ctx context.Context, runner commandRunner, repos []repoConfig) []repoAccessCheck {
	checks := make([]repoAccessCheck, len(repos))
	jobs := make(chan int)
	var wg sync.WaitGroup
	// Limit network and credential-helper processes when a home contains many repos.
	for range min(4, len(repos)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				checks[i] = checkRepository(ctx, runner, repos[i])
			}
		}()
	}
	for i := range repos {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return checks
}

func checkRepository(ctx context.Context, runner commandRunner, repo repoConfig) repoAccessCheck {
	var check repoAccessCheck
	if err := ctx.Err(); err != nil {
		check.err = err
		return check
	}
	if err := checkGitIdentity(ctx, runner, repo); err != nil {
		check.err = err
		return check
	}
	remoteURL, err := runGit(ctx, runner, repo.Path, "remote", "get-url", repo.Remote)
	check.remoteURL = strings.TrimSpace(remoteURL)
	if err != nil {
		check.err = fmt.Errorf("read remote %s; check `git -C %s remote -v`: %w", repo.Remote, preflightShellQuote(repo.Path), err)
		return check
	}
	// Newer Git can update the remote HEAD even during a dry-run. Keep this
	// override local to the probe so normal fetches retain the user's settings.
	if _, err := runGit(ctx, runner, repo.Path, "-c", "remote."+repo.Remote+".followRemoteHEAD=never", "fetch", "--dry-run", "--no-write-fetch-head", "--no-auto-maintenance", repo.Remote); err != nil {
		check.err = fmt.Errorf("fetch failed; check credentials and network access with `git -C %s fetch --dry-run %s`: %w", preflightShellQuote(repo.Path), preflightShellQuote(repo.Remote), err)
		return check
	}
	remoteURL, err = runGit(ctx, runner, repo.Path, "remote", "get-url", "--push", repo.Remote)
	check.remoteURL = strings.TrimSpace(remoteURL)
	if err != nil {
		check.err = fmt.Errorf("read push remote %s: %w", repo.Remote, err)
		return check
	}
	// Use precisely the default-branch rule used by the daemon. In particular,
	// never check pushing a feature branch just because it is currently checked out.
	branch := (gitSyncer{runner: runner}).defaultBranch(ctx, repo)
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := runGit(ctx, runner, repo.Path, "push", "--dry-run", "--no-verify", "--porcelain", repo.Remote, refspec); err != nil {
		if isPreflightNonFastForward(err) {
			check.warning = fmt.Sprintf("%s needs a fetch/rebase before it can be pushed. Push transport was reached; the daemon will attempt the rebase when this branch is checked out.", branch)
		} else {
			check.err = fmt.Errorf("push dry-run failed for %s; check write access and that the local branch exists with `git -C %s push --dry-run --no-verify %s %s`: %w", branch, preflightShellQuote(repo.Path), preflightShellQuote(repo.Remote), preflightShellQuote(refspec), err)
		}
	}
	return check
}

func checkGitIdentity(ctx context.Context, runner commandRunner, repo repoConfig) error {
	var missing []string
	for _, key := range []string{"user.name", "user.email"} {
		value, err := runGit(ctx, runner, repo.Path, "config", "--get", key)
		if err != nil || strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Git identity is missing %s. Set your identity with `git -C %s config user.name 'Your Name'` and `git -C %s config user.email 'you@example.com'` (or use `git config --global`), then run setup again", strings.Join(missing, " and "), preflightShellQuote(repo.Path), preflightShellQuote(repo.Path))
	}
	for _, identity := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		if _, err := runGit(ctx, runner, repo.Path, "var", identity); err != nil {
			return fmt.Errorf("Git cannot create commits with its configured identity; check user.name and user.email using `git -C %s config --show-origin --get-regexp '^user\\.'`: %w", preflightShellQuote(repo.Path), err)
		}
	}
	return nil
}

func preflightShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isPreflightNonFastForward(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "[rejected]") && (strings.Contains(message, "non-fast-forward") || strings.Contains(message, "fetch first"))
}

func isGitHubHTTPS(remote string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(remote)), "https://")
	if !ok {
		return false
	}
	authority, _, _ := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	host, _, _ := strings.Cut(authority, ":")
	return host == "github.com"
}

func isAuthenticationFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "invalid username or token", "returned error: 401",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func configureGitHubCredentials(ctx context.Context, runner commandRunner, in io.Reader, out io.Writer) error {
	if _, err := runner.run(ctx, "", "", "gh", "--version"); err != nil {
		return fmt.Errorf("GitHub HTTPS authentication needs GitHub CLI, but `gh` is unavailable to the background service. Run `brew install gh`, then run `repo-sync setup` again: %w", err)
	}
	fmt.Fprintln(out, "GitHub credentials are not available to background Git. Configuring them with GitHub CLI...")
	if _, err := runner.run(ctx, "", "", "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		fmt.Fprintln(out, "GitHub login is required. Follow the browser prompt.")
		var loginErr error
		if interactive, ok := runner.(interactiveCommandRunner); ok {
			loginErr = interactive.runInteractive(ctx, "", in, out, "gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web")
		} else {
			_, loginErr = runner.run(ctx, "", "", "gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web")
		}
		if loginErr != nil {
			return fmt.Errorf("GitHub login failed; run `gh auth login --hostname github.com --git-protocol https --web`, then run setup again: %w", loginErr)
		}
	}
	if _, err := runner.run(ctx, "", "", "gh", "auth", "setup-git", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("configure GitHub credentials for Git: %w", err)
	}
	return nil
}
