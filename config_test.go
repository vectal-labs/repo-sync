package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfigHasNoRepositories(t *testing.T) {
	cfg := newDefaultConfig()
	if len(cfg.Repositories) != 0 {
		t.Fatalf("default config must not list repositories: %v", cfg.Repositories)
	}
	if cfg.IdleDebounce.Duration != 60*time.Second || cfg.FetchInterval.Duration != 60*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestConfigRoundTripAndUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	root := t.TempDir()
	cfg := newDefaultConfig()
	if _, err := addRepository(&cfg, filepath.Join(root, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store := &configStore{path: path}
	err := store.update(func(cfg *config) error {
		return allowPath(cfg, filepath.Join(root, "notes"), "config/.env")
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := loaded.Repositories[0]
	if repo.Name != "notes" || repo.Remote != "origin" || len(repo.Allow) != 1 || repo.Allow[0] != "config/.env" {
		t.Fatalf("unexpected loaded repository: %+v", repo)
	}
}

func TestAddRepositoryRejectsDuplicatesAndMakesNamesUnique(t *testing.T) {
	root := t.TempDir()
	cfg := newDefaultConfig()
	if _, err := addRepository(&cfg, filepath.Join(root, "a", "notes")); err != nil {
		t.Fatal(err)
	}
	if _, err := addRepository(&cfg, filepath.Join(root, "a", "notes")); err == nil {
		t.Fatal("adding the same path twice must fail")
	}
	second, err := addRepository(&cfg, filepath.Join(root, "b", "notes"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != "notes-2" {
		t.Fatalf("name = %q, want notes-2", second.Name)
	}
	if _, err := addRepository(&cfg, "relative/path"); err == nil {
		t.Fatal("relative paths must be rejected")
	}
}

func TestAllowPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	cfg := newDefaultConfig()
	if _, err := addRepository(&cfg, root); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../outside", "/abs/path", "."} {
		if err := allowPath(&cfg, root, bad); err == nil {
			t.Errorf("allowPath(%q) must fail", bad)
		}
	}
	if err := allowPath(&cfg, filepath.Join(root, "missing"), ".env"); err == nil {
		t.Error("allowPath for an unknown repository must fail")
	}
}

func TestLoadOrDefaultConfig(t *testing.T) {
	cfg, err := loadOrDefaultConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || cfg.IdleDebounce.Duration != 60*time.Second {
		t.Fatalf("missing config should fall back to defaults: %v %+v", err, cfg)
	}
}

func TestRedactCredentials(t *testing.T) {
	input := "fatal: https://user:secret@github.com/o/r?access_token=hidden"
	got := redactCredentials(input)
	if got != "fatal: https://***@github.com/o/r?access_token=***" {
		t.Fatalf("redacted output = %q", got)
	}
}
