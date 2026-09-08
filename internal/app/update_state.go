package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type updateSettings struct {
	Automatic bool `json:"automatic"`
}

type updateState struct {
	LatestVersion string    `json:"latest_version,omitempty"`
	CheckedAt     time.Time `json:"checked_at,omitempty"`
	AttemptedAt   time.Time `json:"attempted_at,omitempty"`
	SucceededAt   time.Time `json:"succeeded_at,omitempty"`
	NextCheck     time.Time `json:"next_check,omitempty"`
	Result        string    `json:"result,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	FailureSince  time.Time `json:"failure_since,omitempty"`
	Notified      string    `json:"notified,omitempty"`
}

func updateSettingsPath() string {
	return filepath.Join(filepath.Dir(defaultConfigPath()), "update-settings.json")
}

func updateCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Caches", "repo-sync")
}

func updateStatePath(configPath string) string {
	absolute, _ := filepath.Abs(configPath)
	return filepath.Join(updateCacheDir(), fmt.Sprintf("updates-%x.json", sha256.Sum256([]byte(absolute))))
}

func loadUpdateSettings() (updateSettings, error) {
	settings := updateSettings{Automatic: true}
	err := readUpdateJSON(updateSettingsPath(), &settings)
	return settings, err
}

func loadUpdateState(configPath string) (updateState, error) {
	var state updateState
	err := readUpdateJSON(updateStatePath(configPath), &state)
	return state, err
}

func readUpdateJSON(path string, into any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func writeUpdateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func writeUpdateState(configPath string, state updateState) error {
	return writeUpdateJSON(updateStatePath(configPath), state)
}

func setAutomaticUpdates(enabled bool, out io.Writer) error {
	if _, err := loadUpdateSettings(); err != nil {
		return err
	}
	if err := writeUpdateJSON(updateSettingsPath(), updateSettings{Automatic: enabled}); err != nil {
		return err
	}
	if enabled {
		fmt.Fprintln(out, "Automatic updates enabled for Homebrew installations. Other installations receive update notifications.")
	} else {
		fmt.Fprintln(out, "Automatic updates disabled. Update notifications stay enabled.")
	}
	return nil
}

func printUpdateStatus(configPath string, out io.Writer) error {
	settings, err := loadUpdateSettings()
	if err != nil {
		return err
	}
	mode := "automatic for Homebrew; notifications for other installations"
	if !settings.Automatic {
		mode = "notifications only (automatic updates disabled)"
	}
	fmt.Fprintln(out, "Updates: "+mode+".")
	state, err := loadUpdateState(configPath)
	if err != nil {
		return err
	}
	if state.LatestVersion != "" {
		fmt.Fprintln(out, "Latest release: "+state.LatestVersion)
	}
	if !state.CheckedAt.IsZero() {
		fmt.Fprintln(out, "Last update check: "+state.CheckedAt.Local().Format(time.RFC3339))
	}
	if state.Result != "" {
		fmt.Fprintf(out, "Last update: %s (%s)\n", state.Result, state.Detail)
	}
	if state.Result == "failed" || state.Result == "installing" {
		return fmt.Errorf("update needs attention; run `%s`", configCommand("update", configPath))
	}
	return nil
}
