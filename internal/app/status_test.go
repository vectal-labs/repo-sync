package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusShowsFailuresAndRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	td := newTestDaemon(t, "notes")
	path := filepath.Join(t.TempDir(), "config.json")
	td.statusFile = statusPath(path)
	if err := writeConfig(path, td.cfg); err != nil {
		t.Fatal(err)
	}
	state := td.states["notes"]
	td.handleResult(state, syncReport{}, errors.New("authentication failed https://user:secret@example.com/repo"))
	if err := td.publishStatus(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(td.statusFile)
	if strings.Contains(string(data), "secret") {
		t.Fatal("status leaked credentials")
	}
	var out strings.Builder
	service := fakeService(&lifecycleRunner{loaded: true, pid: os.Getpid()})
	if err := runStatus(context.Background(), path, service, &out); err == nil || !strings.Contains(out.String(), "retrying") {
		t.Fatalf("failed status = %v\n%s", err, out.String())
	}
	td.handleResult(state, syncReport{}, nil)
	if err := td.publishStatus(); err != nil {
		t.Fatal(err)
	}
	var status serviceStatus
	if err := json.Unmarshal(mustRead(t, td.statusFile), &status); err != nil {
		t.Fatal(err)
	}
	if status.Version != appVersion() {
		t.Fatalf("daemon version = %q; want %q", status.Version, appVersion())
	}
	if status.Repositories[0].LastSuccess.IsZero() {
		t.Fatal("successful cycle was not recorded")
	}
	out.Reset()
	if err := runStatus(context.Background(), path, service, &out); err != nil || !strings.Contains(out.String(), "last successful cycle") {
		t.Fatalf("recovered status = %v\n%s", err, out.String())
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadinessRequiresFreshStableProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := newDefaultConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	service := fakeService(&lifecycleRunner{loaded: true, pid: 345})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := service.waitReady(ctx, path, time.Second); err == nil {
		t.Fatal("service without heartbeat passed readiness")
	}
}

func TestStatusKeepsUpdateFailuresVisibleWhenServiceIsStopped(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "stopped"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := newDefaultConfig()
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			if err := writeFileAtomic(updateStatePath(path), []byte(`{"result":"failed","detail":"Homebrew is locked"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			service := fakeService(&lifecycleRunner{loaded: running, pid: os.Getpid()})
			if running {
				writeTestStatus(t, path, serviceStatus{PID: os.Getpid(), Version: appVersion(), ConfigHash: configHash(cfg), UpdatedAt: time.Now()})
			}
			var out strings.Builder
			err := runStatus(context.Background(), path, service, &out)
			if err == nil {
				t.Fatal("failed update returned a healthy status")
			}
			for _, want := range []string{"Installed version: " + appVersion(), "Updates:", "Last update: failed", "Homebrew is locked"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("status omitted %q: %v\n%s", want, err, out.String())
				}
			}
			if running && !strings.Contains(out.String(), "Service is running") {
				t.Fatalf("update failure hid running service\n%s", out.String())
			}
			if !running && (!strings.Contains(err.Error(), "service is not running") || !strings.Contains(out.String(), "Running version: unavailable")) {
				t.Fatalf("stopped service = %v\n%s", err, out.String())
			}
		})
	}
}

func TestStatusIdentifiesWrongRunningRelease(t *testing.T) {
	previousVersion := buildVersion
	buildVersion = "1.2.3"
	t.Cleanup(func() { buildVersion = previousVersion })
	for _, test := range []struct {
		name, version string
		mismatch      bool
	}{
		{"current", "v1.2.3", false},
		{"current unprefixed", "1.2.3", false},
		{"older", "v1.2.2", true},
		{"newer", "v1.2.4", true},
		{"development", "dev", false},
		{"legacy", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := newDefaultConfig()
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			writeTestStatus(t, path, serviceStatus{PID: os.Getpid(), Version: test.version, ConfigHash: configHash(cfg), UpdatedAt: time.Now()})
			service := fakeService(&lifecycleRunner{loaded: true, pid: os.Getpid()})
			if _, err := service.readStatus(context.Background(), path); err != nil {
				t.Fatalf("readiness must let the old updater observe a new release: %v", err)
			}
			var out strings.Builder
			err := runStatus(context.Background(), path, service, &out)
			if (err != nil) != test.mismatch {
				t.Fatalf("version mismatch = %v; want %v\n%s", err, test.mismatch, out.String())
			}
			if test.mismatch && !strings.Contains(err.Error(), "differs from installed version") {
				t.Fatalf("missing mismatch explanation: %v", err)
			}
			wantRunning := test.version
			if wantRunning == "" {
				wantRunning = "unknown"
			}
			for _, want := range []string{"Installed version: v1.2.3", "Running version: " + wantRunning, "Service is running"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("status omitted %q\n%s", want, out.String())
				}
			}
		})
	}
}

func writeTestStatus(t *testing.T, configPath string, status serviceStatus) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(statusPath(configPath), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
