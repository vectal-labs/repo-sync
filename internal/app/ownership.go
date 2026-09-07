package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type installRecord struct {
	Version     int      `json:"version"`
	ConfigPaths []string `json:"config_paths"`
	BinaryPaths []string `json:"binary_paths"`
}

func installRecordPath() string {
	return filepath.Join(filepath.Dir(defaultConfigPath()), "install.json")
}

func loadInstallRecord() (installRecord, error) {
	snapshot, err := snapshotFile(installRecordPath())
	if err != nil {
		return installRecord{}, fmt.Errorf("read installation record: %w", err)
	}
	if !snapshot.exists {
		return installRecord{Version: 1}, nil
	}
	var record installRecord
	if err := json.Unmarshal(snapshot.data, &record); err != nil {
		return installRecord{}, fmt.Errorf("read installation record: %w", err)
	}
	if err := validateInstallRecord(record); err != nil {
		return installRecord{}, fmt.Errorf("read installation record: %w", err)
	}
	return record, nil
}

func mergeInstallation(record installRecord, configPaths []string, binary string) (installRecord, error) {
	if err := validateInstallRecord(record); err != nil {
		return installRecord{}, err
	}
	for _, path := range configPaths {
		absolute, err := absoluteInstallPath(path)
		if err != nil {
			return installRecord{}, err
		}
		record.ConfigPaths = append(record.ConfigPaths, absolute)
	}
	absolute, err := absoluteInstallPath(binary)
	if err != nil {
		return installRecord{}, err
	}
	record.BinaryPaths = append(record.BinaryPaths, absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		record.BinaryPaths = append(record.BinaryPaths, resolved)
	} else if !errors.Is(err, os.ErrNotExist) {
		return installRecord{}, fmt.Errorf("resolve installed program: %w", err)
	}
	record.ConfigPaths = uniquePaths(record.ConfigPaths)
	record.BinaryPaths = uniquePaths(record.BinaryPaths)
	if err := validateInstallRecord(record); err != nil {
		return installRecord{}, err
	}
	return record, nil
}

func writeInstallRecord(record installRecord) error {
	if err := validateInstallRecord(record); err != nil {
		return err
	}
	if _, err := snapshotFile(installRecordPath()); err != nil {
		return fmt.Errorf("write installation record: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(installRecordPath(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write installation record: %w", err)
	}
	return nil
}

func validateInstallRecord(record installRecord) error {
	if record.Version != 1 {
		return fmt.Errorf("unsupported installation record version %d", record.Version)
	}
	for _, path := range append(append([]string{}, record.ConfigPaths...), record.BinaryPaths...) {
		if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) {
			return fmt.Errorf("installation record paths must be nonempty absolute paths: %q", path)
		}
	}
	for _, configPath := range record.ConfigPaths {
		if err := validateInstallConfigPath(configPath); err != nil {
			return err
		}
		for _, binary := range record.BinaryPaths {
			if canonicalInstallPath(configPath) == canonicalInstallPath(binary) {
				return fmt.Errorf("config path %s conflicts with installed program %s", configPath, binary)
			}
		}
	}
	return nil
}

func validateInstallConfigPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) {
		return fmt.Errorf("config path must be a nonempty absolute path: %q", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	canonical := canonicalInstallPath(path)
	for _, reserved := range []string{installRecordPath(), filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")} {
		if canonical == canonicalInstallPath(reserved) {
			return fmt.Errorf("config path %s conflicts with repo-sync's installation files", path)
		}
	}
	for _, dir := range []string{filepath.Join(home, "Library", "Logs", "repo-sync"), filepath.Join(home, "Library", "Caches", "repo-sync")} {
		reserved := canonicalInstallPath(dir)
		if canonical == reserved || strings.HasPrefix(canonical, reserved+string(filepath.Separator)) {
			return fmt.Errorf("config path %s is inside repo-sync's logs or cache", path)
		}
	}
	return nil
}

func absoluteInstallPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("installation paths must not be empty or contain NUL")
	}
	return filepath.Abs(path)
}

// Resolve existing parents too, so a new config cannot hide a reserved path
// behind a directory symlink before the config itself has been created.
func canonicalInstallPath(path string) string {
	path = filepath.Clean(path)
	for parent, tail := path, ""; ; parent = filepath.Dir(parent) {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, tail)
		}
		if parent == filepath.Dir(parent) {
			return path
		}
		tail = filepath.Join(filepath.Base(parent), tail)
	}
}
