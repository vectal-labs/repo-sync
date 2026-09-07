//go:build darwin

package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type uninstallChild struct {
	command *exec.Cmd
	done    chan struct{}
	err     error // read only after done is closed
	logPath string
}

func startUninstallChild(t *testing.T, executable string, args ...string) *uninstallChild {
	t.Helper()
	child := &uninstallChild{command: exec.Command(executable, args...), done: make(chan struct{}), logPath: filepath.Join(t.TempDir(), "process.log")}
	log, err := os.Create(child.logPath)
	if err != nil {
		t.Fatal(err)
	}
	child.command.Stdout, child.command.Stderr = log, log
	if err := child.command.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	go func() {
		child.err = child.command.Wait()
		close(child.done)
	}()
	t.Cleanup(func() {
		select {
		case <-child.done:
		default:
			_ = child.command.Process.Signal(syscall.SIGTERM)
			select {
			case <-child.done:
			case <-time.After(5 * time.Second):
				// Cleanup owns this exact child, never an existing user process.
				_ = child.command.Process.Kill()
				<-child.done
			}
		}
		_ = log.Close()
	})
	return child
}

func waitUninstallChildReady(t *testing.T, child *uninstallChild, configPath string, cfg config) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-child.done:
			t.Fatalf("foreground repo-sync exited before readiness: %v\n%s", child.err, mustRead(t, child.logPath))
		default:
		}
		data, err := os.ReadFile(statusPath(configPath))
		var status serviceStatus
		if err == nil && json.Unmarshal(data, &status) == nil && status.PID == child.command.Process.Pid && status.ConfigHash == configHash(cfg) && len(status.Repositories) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("foreground repo-sync did not report readiness: %v\n%s", err, mustRead(t, child.logPath))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestUninstallStopsRealForegroundCopiesAndPreservesOtherProcesses(t *testing.T) {
	binaryData := uninstallBinaryBytes(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	remote := filepath.Join(home, "remote.git")
	repo := filepath.Join(home, "notes")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", remote)
	gitRun(t, "", "clone", remote, repo)
	configureGitUser(t, repo)
	writeAndCommit(t, repo, "note.md", "initial\n", "initial")
	gitRun(t, repo, "push", "-u", "origin", "main")
	write(t, repo, "note.md", "keep this uncommitted edit\n")
	write(t, repo, "draft.md", "keep this staged draft\n")
	gitRun(t, repo, "add", "draft.md")
	beforeHead := gitOutput(t, repo, "rev-parse", "HEAD")
	beforeStatus := gitOutput(t, repo, "status", "--porcelain=v1")
	beforeStaged := gitOutput(t, repo, "diff", "--cached", "--binary")
	beforeRemote := gitOutput(t, "", "--git-dir", remote, "rev-parse", "main")

	binaries := []string{filepath.Join(home, "bin", "repo-sync"), filepath.Join(home, "old-install", "repo-sync")}
	configs := []string{filepath.Join(home, "settings", "active.json"), filepath.Join(home, "settings", "older.json")}
	cfg := newDefaultConfig()
	// These dirty files must remain user work while uninstall runs.
	cfg.IdleDebounce.Duration = time.Hour
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	children := make([]*uninstallChild, 0, len(binaries))
	for i, binary := range binaries {
		uninstallWrite(t, binary, string(binaryData), 0o755)
		if err := writeConfig(configs[i], cfg); err != nil {
			t.Fatal(err)
		}
		if err := recordInstallation(configs[i], binary); err != nil {
			t.Fatal(err)
		}
		children = append(children, startUninstallChild(t, binary, "run", "--config", configs[i]))
	}
	for i, child := range children {
		waitUninstallChildReady(t, child, configs[i], cfg)
	}
	unrelated := startUninstallChild(t, "/bin/sleep", "60")
	unrelatedIdentity, err := (nativeProcessInspector{}).inspect(unrelated.command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	runner := &uninstallRunner{label: "test.repo-sync.foreground-uninstall", loaded: true}
	service := &launchService{runner: runner, domain: "gui/test", label: runner.label}
	logs := filepath.Join(home, "Library", "Logs", "repo-sync")
	plist := service.plistPath(home)
	uninstallWrite(t, plist, launchAgentPlist(binaries[0], configs[0], logs), 0o644)
	uninstallWrite(t, filepath.Join(logs, "stdout.log"), "old log\n", 0o600)
	uninstallWrite(t, filepath.Join(logs, "stderr.log.1"), "rotated log\n", 0o600)
	unknownFile := filepath.Join(home, "settings", "keep.md")
	uninstallWrite(t, unknownFile, "unrelated settings document\n", 0o600)
	var out strings.Builder
	opts := uninstallOptions{
		configPath: configs[0], binary: binaries[0], yes: true,
		in: strings.NewReader(""), out: &out, service: service,
		binaryPaths: []string{}, // Never discover installations on the host.
		// Nil hooks intentionally exercise native process discovery and shutdown.
		discoverProcesses: nil, stopProcesses: nil,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runUninstall(ctx, opts); err != nil {
		t.Fatalf("uninstall real foreground processes: %v\n%s", err, out.String())
	}
	for _, child := range children {
		select {
		case <-child.done:
			if child.err != nil {
				t.Errorf("foreground daemon did not exit gracefully: %v\n%s", child.err, mustRead(t, child.logPath))
			}
		case <-time.After(time.Second):
			t.Error("uninstall returned while a foreground daemon was still running")
		}
	}
	if got, err := (nativeProcessInspector{}).inspect(unrelated.command.Process.Pid); err != nil || got != unrelatedIdentity {
		t.Fatalf("unrelated process was affected: %+v, %v", got, err)
	}
	uninstallAssertMissing(t, binaries...)
	uninstallAssertMissing(t, configs...)
	uninstallAssertMissing(t, plist, installRecordPath(), statusPath(configs[0]), statusPath(configs[1]))
	uninstallAssertMissing(t, appDirectories(home)...)
	if got := string(mustRead(t, unknownFile)); got != "unrelated settings document\n" {
		t.Fatal("uninstall changed an unrelated config-directory file")
	}
	if gitOutput(t, repo, "rev-parse", "HEAD") != beforeHead || gitOutput(t, repo, "status", "--porcelain=v1") != beforeStatus || gitOutput(t, repo, "diff", "--cached", "--binary") != beforeStaged || gitOutput(t, "", "--git-dir", remote, "rev-parse", "main") != beforeRemote {
		t.Fatal("uninstall changed repository work, staging, or Git history")
	}
	if got := string(mustRead(t, filepath.Join(repo, "note.md"))); got != "keep this uncommitted edit\n" {
		t.Fatal("uninstall changed the user's uncommitted file")
	}
}
