package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyRepositoriesRequiresGitIdentity(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_, local := makeGitFixture(t)
	gitRun(t, local, "config", "--unset", "user.name")
	gitRun(t, local, "config", "--unset", "user.email")
	err := verifyRepositories(context.Background(), execCommandRunner{}, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "user.name") || !strings.Contains(err.Error(), "user.email") {
		t.Fatalf("setup must explain how to configure missing Git identity: %v", err)
	}
}

func TestVerifyRepositoriesRejectsFetchOnlyRemote(t *testing.T) {
	_, local := makeGitFixture(t)
	gitRun(t, local, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "missing.git"))
	err := verifyRepositories(context.Background(), execCommandRunner{}, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("setup must reject a repository that can fetch but cannot push: %v", err)
	}
}

func TestVerifyRepositoriesChecksDefaultBranchWithoutChangingRepository(t *testing.T) {
	remote, local := makeGitFixture(t)
	gitRun(t, local, "branch", "-m", "main", "trunk")
	gitRun(t, local, "push", "origin", "trunk")
	gitRun(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	gitRun(t, local, "remote", "set-head", "origin", "trunk")
	writeAndCommit(t, local, "ahead.txt", "local commit\n", "ahead of remote")
	gitRun(t, local, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(local, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(local, "staged.txt"), []byte("also unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "fetch", "origin")
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hook := []byte("#!/bin/sh\nprintf ran > " + preflightShellQuote(hookMarker) + "\nexit 1\n")
	for _, path := range []string{filepath.Join(local, ".git", "hooks", "pre-push"), filepath.Join(remote, "hooks", "pre-receive")} {
		if err := os.WriteFile(path, hook, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	localRefs := gitOutput(t, local, "show-ref")
	remoteRefs := gitOutput(t, "", "--git-dir", remote, "show-ref")
	status := gitOutput(t, local, "status", "--porcelain")
	index := readPreflightFile(t, filepath.Join(local, ".git", "index"))
	fetchHead := readPreflightFile(t, filepath.Join(local, ".git", "FETCH_HEAD"))
	var pushArgs []string
	runner := preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if name == "git" && args[0] == "push" {
			pushArgs = append([]string{}, args...)
		}
		return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
	})
	var out strings.Builder
	if err := verifyRepositories(context.Background(), runner, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pushArgs, []string{"push", "--dry-run", "--no-verify", "--porcelain", "origin", "refs/heads/trunk:refs/heads/trunk"}) {
		t.Fatalf("wrong push probe: %v", pushArgs)
	}
	if !bytes.Equal(index, readPreflightFile(t, filepath.Join(local, ".git", "index"))) || !bytes.Equal(fetchHead, readPreflightFile(t, filepath.Join(local, ".git", "FETCH_HEAD"))) {
		t.Fatal("preflight changed the Git index or FETCH_HEAD")
	}
	if gitOutput(t, local, "show-ref") != localRefs || gitOutput(t, "", "--git-dir", remote, "show-ref") != remoteRefs || gitOutput(t, local, "status", "--porcelain") != status {
		t.Fatal("preflight changed repository refs or local changes")
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("preflight ran a push hook: %v", err)
	}
	if !strings.Contains(out.String(), "cannot verify server branch rules or hooks") {
		t.Fatalf("dry-run limitation missing: %s", out.String())
	}
}

func TestVerifyRepositoriesWarnsWhenRemoteNeedsRebase(t *testing.T) {
	for _, followHead := range []string{"", "always"} {
		name := "default"
		if followHead != "" {
			name = followHead
		}
		t.Run(name, func(t *testing.T) {
			remote, local := makeGitFixture(t)
			if followHead != "" {
				gitRun(t, local, "config", "remote.origin.followRemoteHEAD", followHead)
			}
			other := filepath.Join(t.TempDir(), "other")
			gitRun(t, "", "clone", remote, other)
			configureGitUser(t, other)
			writeAndCommit(t, other, "other.txt", "remote changed\n", "remote ahead")
			gitRun(t, other, "push", "origin", "main")
			before := gitOutput(t, local, "show-ref")
			var out strings.Builder
			if err := verifyRepositories(context.Background(), execCommandRunner{}, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "needs a fetch/rebase") {
				t.Fatalf("missing warning about behind branch: %s", out.String())
			}
			if gitOutput(t, local, "show-ref") != before {
				t.Fatal("preflight updated refs while probing the remote")
			}
		})
	}
}

func TestVerifyRepositoriesPreservesRemoteHEADAndConfig(t *testing.T) {
	for _, test := range []struct {
		name         string
		remote       string
		existingHEAD bool
	}{
		{name: "origin missing HEAD", remote: "origin"},
		{name: "origin existing HEAD", remote: "origin", existingHEAD: true},
		{name: "upstream missing HEAD", remote: "upstream"},
		{name: "upstream existing HEAD", remote: "upstream", existingHEAD: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remote, local := makeGitFixture(t)
			if test.remote != "origin" {
				gitRun(t, local, "remote", "rename", "origin", test.remote)
			}
			gitRun(t, local, "branch", "trunk")
			gitRun(t, local, "push", test.remote, "trunk")
			gitRun(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			headRef := "refs/remotes/" + test.remote + "/HEAD"
			if test.existingHEAD {
				gitRun(t, local, "remote", "set-head", test.remote, "main")
			} else {
				gitRun(t, local, "update-ref", "--no-deref", "-d", headRef)
			}
			gitRun(t, local, "config", "remote."+test.remote+".followRemoteHEAD", "always")
			beforeRefs := gitOutput(t, local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
			configPath := filepath.Join(local, ".git", "config")
			beforeConfig := readPreflightFile(t, configPath)

			if err := verifyRepositories(context.Background(), execCommandRunner{}, []repoConfig{{Name: "notes", Path: local, Remote: test.remote}}, strings.NewReader(""), &strings.Builder{}); err != nil {
				t.Fatal(err)
			}
			if afterRefs := gitOutput(t, local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"); afterRefs != beforeRefs {
				t.Fatalf("preflight changed refs:\nbefore:\n%safter:\n%s", beforeRefs, afterRefs)
			}
			if !bytes.Equal(beforeConfig, readPreflightFile(t, configPath)) {
				t.Fatal("preflight changed saved Git configuration")
			}
		})
	}
}

func TestVerifyRepositoriesRejectsInvalidEffectiveIdentity(t *testing.T) {
	_, local := makeGitFixture(t)
	gitRun(t, local, "config", "user.name", "<>")
	err := verifyRepositories(context.Background(), execCommandRunner{}, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "cannot create commits") {
		t.Fatalf("invalid effective identity accepted: %v", err)
	}
}

func TestVerifyRepositoriesRequiresGitHubCLIOnlyForMissingGitHubCredentials(t *testing.T) {
	_, local := makeGitFixture(t)
	for _, test := range []struct {
		name       string
		remote     string
		fetchError error
		wantsGH    bool
	}{
		{name: "valid GitHub credentials", remote: "https://github.com/acme/notes.git"},
		{name: "missing GitHub credentials", remote: "https://github.com/acme/notes.git", fetchError: errors.New("authentication failed"), wantsGH: true},
		{name: "GitHub network error", remote: "https://github.com/acme/notes.git", fetchError: errors.New("could not resolve host")},
		{name: "other host credentials", remote: "https://example.com/acme/notes.git", fetchError: errors.New("authentication failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var ghCalls []string
			runner := preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
				if name == "gh" {
					ghCalls = append(ghCalls, strings.Join(args, " "))
					return "", errors.New("executable file not found")
				}
				if name == "git" && args[0] == "remote" {
					return test.remote, nil
				}
				if name == "git" && slices.Contains(args, "fetch") && test.fetchError != nil {
					return "", test.fetchError
				}
				return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
			})
			err := verifyRepositories(context.Background(), runner, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
			if test.fetchError == nil && err != nil {
				t.Fatal(err)
			}
			if test.fetchError != nil && err == nil {
				t.Fatal("fetch error must fail preflight")
			}
			if test.wantsGH {
				if !slices.Equal(ghCalls, []string{"--version"}) || !strings.Contains(err.Error(), "brew install gh") {
					t.Fatalf("missing CLI should explain installation without attempting login: %v; %v", ghCalls, err)
				}
			} else if len(ghCalls) != 0 {
				t.Fatalf("unnecessary GitHub CLI calls: %v", ghCalls)
			}
		})
	}
}

func TestCheckGitToolsExplainsMissingGit(t *testing.T) {
	runner := preflightRunnerFunc(func(_ context.Context, _, _, name string, args ...string) (string, error) {
		if name != "git" || !slices.Equal(args, []string{"--version"}) {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return "", errors.New("executable file not found")
	})
	if err := checkGitTools(context.Background(), runner); err == nil || !strings.Contains(err.Error(), "xcode-select --install") {
		t.Fatalf("missing Git needs actionable guidance: %v", err)
	}
}

func TestVerifyRepositoriesRepairsAuthenticationForSeparatePushURL(t *testing.T) {
	_, local := makeGitFixture(t)
	var ghCalls []string
	runner := preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if name == "git" && slices.Equal(args, []string{"remote", "get-url", "--push", "origin"}) {
			return "https://github.com/acme/notes.git", nil
		}
		if name == "git" && args[0] == "push" {
			return "", errors.New("authentication failed")
		}
		if name == "gh" {
			ghCalls = append(ghCalls, strings.Join(args, " "))
			return "", errors.New("executable file not found")
		}
		return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
	})
	err := verifyRepositories(context.Background(), runner, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "brew install gh") || !slices.Equal(ghCalls, []string{"--version"}) {
		t.Fatalf("GitHub push URL auth failure needs GitHub credentials even with another fetch URL: %v; %v", err, ghCalls)
	}
}

func TestVerifyRepositoriesDoesNotTreatPushPolicyFailureAsRebaseWarning(t *testing.T) {
	_, local := makeGitFixture(t)
	runner := preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if name == "git" && args[0] == "push" {
			return "", errors.New("[remote rejected] main -> main (permission denied)")
		}
		return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
	})
	err := verifyRepositories(context.Background(), runner, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}, strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("push policy failures must block setup: %v", err)
	}
}

func TestCheckRepositoriesBoundsConcurrentCommands(t *testing.T) {
	var active, peak atomic.Int32
	runner := preflightRunnerFunc(func(_ context.Context, _, _, _ string, _ ...string) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return "", errors.New("missing identity")
	})
	checks := checkRepositories(context.Background(), runner, make([]repoConfig, 12))
	if len(checks) != 12 || peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("checks=%d peak concurrent Git commands=%d, want at most four with concurrent work", len(checks), peak.Load())
	}
	for _, check := range checks {
		if check.err == nil {
			t.Fatal("failed repository was not reported")
		}
	}
}

type preflightRunnerFunc func(context.Context, string, string, string, ...string) (string, error)

func (f preflightRunnerFunc) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	return f(ctx, dir, stdin, name, args...)
}

func readPreflightFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
