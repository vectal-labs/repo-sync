package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceInspectRecognizesMissingServiceOnStderr(t *testing.T) {
	for _, test := range []struct {
		name, message string
		wantError     bool
	}{
		{"missing", `Could not find service "test.repo-sync" in domain for user gui: 501`, false},
		{"permission denied", "Operation not permitted", true},
		{"different service", `Could not find service "test.repo-sync-other" in domain for user gui: 501`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := preflightRunnerFunc(func(ctx context.Context, _, _, _ string, _ ...string) (string, error) {
				return (execCommandRunner{}).run(ctx, "", test.message, "/bin/sh", "-c", "cat >&2; exit 113")
			})
			service := &launchService{runner: runner, domain: "gui/test", label: "test.repo-sync"}
			state, err := service.inspect(context.Background())
			if (err != nil) != test.wantError || state.loaded {
				t.Fatalf("inspect returned %+v, %v", state, err)
			}
		})
	}
}

type lifecycleRunner struct {
	loaded      bool
	pid         int
	failStart   bool
	failStop    bool
	failInspect bool
	starts      int
}

func (r *lifecycleRunner) run(_ context.Context, _, _, _ string, args ...string) (string, error) {
	switch args[0] {
	case "print":
		if r.failInspect {
			return "permission denied", errors.New("permission denied")
		}
		if !r.loaded {
			return "Could not find service com.vectal-labs.repo-sync", errors.New("not found")
		}
		return fmt.Sprintf("state = running\npid = %d\n", r.pid), nil
	case "bootout":
		if r.failStop {
			return "", errors.New("stop refused")
		}
		r.loaded = false
		return "", nil
	case "bootstrap":
		r.starts++
		if r.failStart && r.starts == 1 {
			return "", errors.New("start refused")
		}
		r.loaded = true
		return "", nil
	}
	return "", fmt.Errorf("unexpected call: %v", args)
}
func fakeService(r *lifecycleRunner) *launchService {
	return &launchService{runner: r, domain: "gui/test", label: launchAgentLabel}
}

func TestServiceInspectionDistinguishesAbsentFromDenied(t *testing.T) {
	r := &lifecycleRunner{}
	s := fakeService(r)
	state, err := s.inspect(context.Background())
	if err != nil || state.loaded {
		t.Fatalf("absent = %+v %v", state, err)
	}
	r.failInspect = true
	if _, err := s.inspect(context.Background()); err == nil {
		t.Fatal("permission failure treated as absent service")
	}
}
func TestSetupRestoresExistingFilesAndServiceWhenStartFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, repo := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(configPath)
	r := &lifecycleRunner{loaded: true, failStart: true}
	s := fakeService(r)
	plistPath := s.plistPath(home)
	oldPlist := launchAgentPlist("/old/repo-sync", configPath, filepath.Join(home, "logs"))
	if err := writeFileAtomic(plistPath, []byte(oldPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &out, runner: execCommandRunner{}, service: s})
	if err == nil || !strings.Contains(err.Error(), "previous setup restored") {
		t.Fatalf("setup = %v\n%s", err, out.String())
	}
	after, _ := os.ReadFile(configPath)
	plist, _ := os.ReadFile(plistPath)
	if string(before) != string(after) || string(plist) != oldPlist || !r.loaded || r.starts != 2 {
		t.Fatalf("previous setup not restored: loaded=%v starts=%d", r.loaded, r.starts)
	}
}
func TestSetupWithNoSelectionMakesNoFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out strings.Builder
	if err := runSetup(context.Background(), setupOptions{configPath: defaultConfigPath(), binary: "/test/repo-sync", noLaunch: true, in: strings.NewReader("\n"), out: &out, runner: execCommandRunner{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty setup created files: %v", err)
	}
	if !strings.Contains(out.String(), "Setup made no changes") {
		t.Fatal(out.String())
	}
}
func TestStatusRejectsStaleWrongProcessAndChangedConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := newDefaultConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	r := &lifecycleRunner{loaded: true, pid: 456}
	s := fakeService(r)
	status := serviceStatus{PID: 456, ConfigHash: configHash(cfg), UpdatedAt: time.Now()}
	writeSnapshot := func() {
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(statusPath(path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSnapshot()
	if _, err := s.readStatus(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	status.UpdatedAt = time.Now().Add(-time.Minute)
	writeSnapshot()
	if _, err := s.readStatus(context.Background(), path); err == nil {
		t.Fatal("stale status accepted")
	}
	status.UpdatedAt = time.Now()
	status.PID = 457
	writeSnapshot()
	if _, err := s.readStatus(context.Background(), path); err == nil {
		t.Fatal("different process accepted")
	}
	status.PID = 456
	writeSnapshot()
	cfg.IdleDebounce.Duration = time.Second
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readStatus(context.Background(), path); err == nil {
		t.Fatal("changed config accepted")
	}
}
