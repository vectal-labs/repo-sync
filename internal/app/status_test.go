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
