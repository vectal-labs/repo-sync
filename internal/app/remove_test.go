package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemovePreservesRepositoryAndOtherSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, repo := makeGitFixture(t)
	write(t, repo, "staged.md", "staged\n")
	gitRun(t, repo, "add", "staged.md")
	write(t, repo, "staged.md", "unstaged revision\n")
	write(t, repo, "untracked.md", "untracked\n")
	beforeHead := gitOutput(t, repo, "rev-parse", "HEAD")
	beforeStatus := gitOutput(t, repo, "status", "--porcelain=v1")
	beforeIndex := gitOutput(t, repo, "diff", "--cached", "--binary")
	beforeRemote := gitOutput(t, repo, "--git-dir", remote, "rev-parse", "main")
	cfg := newDefaultConfig()
	remaining := repoConfig{Name: "other", Path: filepath.Join(t.TempDir(), "other"), Remote: "backup", Allow: []string{".env.test"}}
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}, remaining}
	if err := writeConfig(defaultConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runRemove(context.Background(), defaultConfigPath(), repo, fakeService(&lifecycleRunner{}), &out); err != nil {
		t.Fatal(err)
	}
	actual, err := loadConfig(defaultConfigPath())
	cfg.Repositories = []repoConfig{remaining}
	if err != nil || !reflect.DeepEqual(actual, cfg) {
		t.Fatalf("remaining config changed: %+v, %v", actual, err)
	}
	if gitOutput(t, repo, "rev-parse", "HEAD") != beforeHead || gitOutput(t, repo, "status", "--porcelain=v1") != beforeStatus || gitOutput(t, repo, "diff", "--cached", "--binary") != beforeIndex || gitOutput(t, repo, "--git-dir", remote, "rev-parse", "main") != beforeRemote {
		t.Fatal("removal changed Git history, the index, or working files")
	}
	if string(mustRead(t, filepath.Join(repo, "staged.md"))) != "unstaged revision\n" || string(mustRead(t, filepath.Join(repo, "untracked.md"))) != "untracked\n" {
		t.Fatal("removal changed file contents")
	}
	if !strings.Contains(out.String(), "background service is not running") || strings.Contains(out.String(), "Stopped syncing") {
		t.Fatalf("unverified live removal claimed: %s", out.String())
	}
}

func TestRemovalResolvesPaths(t *testing.T) {
	_, repo := makeGitFixture(t)
	subdir := filepath.Join(repo, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(repo, "missing-clone")
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}, {Name: "missing", Path: missing, Remote: "origin"}}
	t.Chdir(subdir)
	for _, test := range []struct {
		path string
		want int
	}{{repo, 0}, {subdir, 0}, {"", 0}, {"..", 0}, {alias, 0}, {missing, 1}, {filepath.Join(alias, "missing-clone"), 1}} {
		index, err := removalIndex(cfg, test.path)
		if err != nil || index != test.want {
			t.Errorf("remove %q = %d, %v; want %d", test.path, index, err, test.want)
		}
	}
	if _, err := removalIndex(cfg, t.TempDir()); err == nil {
		t.Fatal("unregistered path matched")
	}
}

func TestRemoveFailureDoesNotClaimSuccess(t *testing.T) {
	for _, scenario := range []string{"missing config", "unknown repo", "inspect failure", "different config", "stop failure", "start failure", "readiness failure"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			repo := filepath.Join(home, "missing-repo")
			cfg := newDefaultConfig()
			cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
			configPath := defaultConfigPath()
			if scenario != "missing config" {
				if err := writeConfig(configPath, cfg); err != nil {
					t.Fatal(err)
				}
			}
			runner := &lifecycleRunner{loaded: true}
			service := fakeService(runner)
			installed := configPath
			switch scenario {
			case "unknown repo":
				repo = filepath.Join(home, "unknown")
			case "inspect failure":
				runner.failInspect = true
			case "different config":
				installed = filepath.Join(home, "other.json")
			case "stop failure":
				runner.failStop = true
			case "start failure":
				runner.failStart = true
			}
			if err := writeFileAtomic(service.plistPath(home), []byte(launchAgentPlist("/repo-sync", installed, filepath.Join(home, "logs"))), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if scenario == "readiness failure" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var out strings.Builder
			err := runRemove(ctx, configPath, repo, service, &out)
			if err == nil || out.Len() != 0 {
				t.Fatalf("failure returned %v with misleading output %q", err, out.String())
			}
			if scenario == "missing config" {
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatal("remove created a missing config")
				}
				return
			}
			actual, loadErr := loadConfig(configPath)
			want := 1
			if scenario == "start failure" || scenario == "readiness failure" {
				want = 0
				if !strings.Contains(err.Error(), "removal saved") {
					t.Fatalf("saved removal not explained: %v", err)
				}
			}
			if loadErr != nil || len(actual.Repositories) != want {
				t.Fatalf("unexpected config after failure: %+v %v", actual, loadErr)
			}
		})
	}
}

func TestRemovePreservesConfigSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "custom.json")
	alias := filepath.Join(t.TempDir(), "alias.json")
	repo := filepath.Join(t.TempDir(), "missing")
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configPath, alias); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runRemove(context.Background(), alias, repo, fakeService(&lifecycleRunner{}), &out); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(alias); err != nil || target != configPath {
		t.Fatal("config symlink was replaced")
	}
	if actual, err := loadConfig(configPath); err != nil || len(actual.Repositories) != 0 {
		t.Fatalf("target config was not updated: %+v %v", actual, err)
	}
}

func TestRemovePreservesConcurrentConfigChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "notes")
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	lifecycle := &lifecycleRunner{loaded: true}
	service := fakeService(lifecycle)
	service.runner = preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		if args[0] == "bootout" {
			cfg.Repositories[0].Allow = []string{"approved.pem"}
			if err := writeConfig(configPath, cfg); err != nil {
				t.Fatal(err)
			}
		}
		return lifecycle.run(ctx, dir, stdin, name, args...)
	})
	if err := writeFileAtomic(service.plistPath(home), []byte(launchAgentPlist("/repo-sync", configPath, filepath.Join(home, "logs"))), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runRemove(context.Background(), configPath, repo, service, &out)
	if err == nil || !strings.Contains(err.Error(), "config changed") || out.Len() != 0 {
		t.Fatalf("concurrent edit not reported: %v, %s", err, out.String())
	}
	actual, err := loadConfig(configPath)
	if err != nil || !reflect.DeepEqual(actual, cfg) || !lifecycle.loaded || lifecycle.starts != 1 {
		t.Fatalf("concurrent edit or service was not preserved: %+v %v", actual, err)
	}
}

func TestRemoveRejectsDuplicateAliases(t *testing.T) {
	_, repo := makeGitFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}, {Name: "alias", Path: alias, Remote: "origin"}}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{repo, alias} {
		if _, err := removalIndex(cfg, path); err == nil || !strings.Contains(err.Error(), "multiple registrations") {
			t.Fatalf("ambiguous removal for %s was accepted: %v", path, err)
		}
	}
}
