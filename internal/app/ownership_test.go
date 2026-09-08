package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func recordInstallation(configPath, binary string) error {
	record, err := loadInstallRecord()
	if err != nil {
		return err
	}
	record, err = mergeInstallation(record, []string{configPath}, binary)
	if err != nil {
		return err
	}
	return writeInstallRecord(record)
}

func TestLoadInstallRecordWhenAbsentDoesNotCreateFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	record, err := loadInstallRecord()
	if err != nil || record.Version != 1 || len(record.ConfigPaths) != 0 || len(record.BinaryPaths) != 0 {
		t.Fatalf("absent record = %+v, %v", record, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
		t.Fatalf("reading absent record created files: %v", err)
	}
}

func TestRecordInstallationMergesPathsAndBinaryAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	binary := filepath.Join(root, "version-1", "repo-sync")
	if err := writeFileAtomic(binary, []byte("program fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "repo-sync")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	firstConfig := filepath.Join(root, "first.json")
	secondConfig := filepath.Join(root, "second.json")
	if err := recordInstallation(root+"/unused/../first.json", link); err != nil {
		t.Fatal(err)
	}
	if err := recordInstallation(secondConfig, "/new/repo-sync"); err != nil {
		t.Fatal(err)
	}
	before := readPreflightFile(t, installRecordPath())
	if err := recordInstallation(firstConfig, link); err != nil {
		t.Fatal(err)
	}
	after := readPreflightFile(t, installRecordPath())
	if string(before) != string(after) {
		t.Fatal("repeated installation changed the record")
	}
	record, err := loadInstallRecord()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(record.ConfigPaths, uniquePaths([]string{firstConfig, secondConfig})) || !slices.Equal(record.BinaryPaths, uniquePaths([]string{link, resolved, "/new/repo-sync"})) {
		t.Fatalf("historical installation paths were lost or duplicated: %+v", record)
	}
	info, err := os.Stat(installRecordPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record must have private 0600 permissions: %v, %v", info, err)
	}
	if err := recordInstallation(link, link); err == nil {
		t.Fatal("config/program collision accepted")
	}
	if string(readPreflightFile(t, installRecordPath())) != string(after) {
		t.Fatal("failed installation changed the existing record")
	}
}

func TestLoadInstallRecordRejectsInvalidDataAndNonregularFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, data := range []string{
		"{", "null", `{}`, `{"version":2}`,
		`{"version":1,"config_paths":["relative.json"]}`,
		`{"version":1,"config_paths":[""]}`,
		`{"version":1,"config_paths":["/bad\u0000path"]}`,
		`{"version":1,"binary_paths":["repo-sync"]}`,
		`{"version":1,"binary_paths":[""]}`,
	} {
		if err := writeFileAtomic(installRecordPath(), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInstallRecord(); err == nil {
			t.Errorf("invalid record accepted: %s", data)
		}
	}
	if err := os.Remove(installRecordPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(installRecordPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallRecord(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("record directory accepted: %v", err)
	}
	if err := os.Remove(installRecordPath()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, installRecordPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallRecord(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink record accepted: %v", err)
	}
}

func TestInstallRecordRejectsReservedConfigPathsAndAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, "Library", "Caches", "repo-sync")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheAlias := filepath.Join(home, "cache-alias")
	if err := os.Symlink(cache, cacheAlias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		installRecordPath(),
		updateSettingsPath(),
		updaterService(nil).plistPath(home),
		filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"),
		filepath.Join(home, "Library", "Logs", "repo-sync", "custom.json"),
		filepath.Join(cache, "custom.json"),
		filepath.Join(cacheAlias, "not-created", "custom.json"),
	} {
		data, _ := json.Marshal(installRecord{Version: 1, ConfigPaths: []string{path}})
		if err := writeFileAtomic(installRecordPath(), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInstallRecord(); err == nil {
			t.Errorf("reserved config path accepted: %s", path)
		}
	}
	if err := validateInstallConfigPath(filepath.Join(home, "custom.status.json")); err != nil {
		t.Fatalf("unrelated config suffix must remain supported: %v", err)
	}
	binary := filepath.Join(home, "repo-sync")
	if err := os.WriteFile(binary, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "program-alias")
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallRecord(installRecord{Version: 1, ConfigPaths: []string{alias}, BinaryPaths: []string{binary}}); err == nil {
		t.Fatal("config symlink to recorded executable accepted")
	}
}
