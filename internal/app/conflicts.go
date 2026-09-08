package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type conflictIncident struct {
	SaveError  string        `json:"-"`
	Version    int           `json:"version"`
	RepoPath   string        `json:"repo_path"`
	Remote     string        `json:"remote"`
	DetectedAt time.Time     `json:"detected_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Notified   bool          `json:"notified"`
	Conflict   conflictError `json:"conflict"`
}

func conflictsPath(configPath string) string {
	return strings.TrimSuffix(statusPath(configPath), ".json") + "-conflicts"
}

func conflictRecordPath(dir string, repo repoConfig) string {
	return filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(repo.Path+"\x00"+repo.Remote))))
}

func readConflict(dir string, repo repoConfig) (*conflictIncident, error) {
	if dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(conflictRecordPath(dir, repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var incident conflictIncident
	if err := json.Unmarshal(data, &incident); err != nil {
		return nil, err
	}
	if incident.Version != 1 || incident.RepoPath != repo.Path || incident.Remote != repo.Remote || len(incident.Conflict.Files) == 0 {
		return nil, errors.New("invalid saved conflict record")
	}
	return &incident, nil
}

func writeConflict(dir string, repo repoConfig, incident *conflictIncident) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(incident)
	if err != nil {
		return err
	}
	return writeFileAtomic(conflictRecordPath(dir, repo), append(data, '\n'), 0o600)
}

func (d *daemon) loadConflicts() {
	for _, state := range d.states {
		incident, err := readConflict(d.conflictDir, state.config)
		if err != nil {
			state.conflictLoadError = "could not read saved conflict details; check service logs"
			d.logger.Printf("%s: read saved conflict: %v", state.config.Name, err)
			continue
		}
		state.conflict = incident // startup only, before any goroutines
	}
}

func (d *daemon) noteConflict(state *repoState, conflict *conflictError) {
	now := d.now()
	incident := &conflictIncident{Version: 1, RepoPath: state.config.Path, Remote: state.config.Remote, DetectedAt: now, UpdatedAt: now, Conflict: *conflict}
	state.mu.Lock()
	if old := state.conflict; old != nil {
		incident.DetectedAt, incident.Notified = old.DetectedAt, old.Notified
	}
	state.conflict = incident
	state.conflictLoadError = ""
	state.lastSkip = ""
	state.mu.Unlock()
	save := func() {
		err := validateConflictDirectory(d.conflictDir, d.cfg.Repositories)
		if err == nil {
			err = writeConflict(d.conflictDir, state.config, incident)
		}
		state.mu.Lock()
		incident.SaveError = ""
		if err != nil {
			incident.SaveError = "could not save conflict details; check service logs"
		}
		state.mu.Unlock()
		if err != nil {
			d.logger.Printf("%s: save conflict details: %v", state.config.Name, err)
		}
	}
	save()
	if !incident.Notified {
		message := conflictNotification(state.config.Name, d.configPath, conflict.AbortError == "")
		if err := d.notify(d.opCtx, d.runner, message); err != nil {
			d.logger.Printf("%s: conflict notification failed: %v", state.config.Name, err)
		} else {
			// Delivery and local persistence cannot be one transaction. Save
			// only after the notifier succeeds so failures can retry delivery.
			state.mu.Lock()
			incident.Notified = true
			state.mu.Unlock()
			save()
		}
	}
	d.logger.Printf("%s: %v; run %s for help", state.config.Name, conflict, configCommand("conflicts", d.configPath))
}

func conflictNotification(name, configPath string, aborted bool) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
	if runes := []rune(name); len(runes) > 48 {
		name = string(runes[:45]) + "..."
	}
	detail := "Local changes are saved."
	if !aborted {
		detail = "Rebase needs attention."
	}
	return fmt.Sprintf("Conflict in %s. %s Run %s for help.", name, detail, configCommand("conflicts", configPath))
}

// Called only after a cycle actually integrated/pushed. A skip or a fetch
// failure never clears the saved incident, including during a manual repair.
func (d *daemon) clearConflict(state *repoState) error {
	if d.conflictDir != "" {
		path := conflictRecordPath(d.conflictDir, state.config)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// A rendered guide is a historical snapshot, never live Git state.
		_ = os.Remove(strings.TrimSuffix(path, ".json") + ".md")
	}
	state.mu.Lock()
	state.conflict = nil
	state.mu.Unlock()
	return nil
}

func conflictDetail(conflict *conflictIncident) string {
	paths := make([]string, len(conflict.Conflict.Files))
	for i, file := range conflict.Conflict.Files {
		paths[i] = fmt.Sprintf("%q", file.Path)
	}
	detail := "needs repair: " + strings.Join(paths, ", ")
	if conflict.SaveError != "" {
		detail += "; " + conflict.SaveError
	}
	if conflict.Conflict.AbortError != "" {
		detail += "; rebase needs attention"
	}
	return detail
}

// Reports may contain private content from multiple repositories. Refuse a
// cache location covered by any configured sync root, including symlink aliases.
func validateConflictDirectory(dir string, repos []repoConfig) error {
	if dir == "" {
		return nil
	}
	cache := canonicalInstallPath(dir)
	for _, repo := range repos {
		rel, err := filepath.Rel(canonicalInstallPath(repo.Path), cache)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("conflict cache is inside a synced repository; private reports cannot be saved there")
		}
	}
	return nil
}
