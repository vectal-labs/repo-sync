package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type config struct {
	IdleDebounce  duration     `json:"idle_debounce"`
	FetchInterval duration     `json:"fetch_interval"`
	Repositories  []repoConfig `json:"repositories"`
}

type repoConfig struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Remote string `json:"remote"`
	// Allow lists repo-relative paths or globs that the secret guard must not block.
	Allow []string `json:"allow,omitempty"`
}

type duration struct {
	time.Duration
}

func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func newDefaultConfig() config {
	return config{
		IdleDebounce:  duration{60 * time.Second},
		FetchInterval: duration{60 * time.Second},
	}
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, "Library", "Application Support", "repo-sync", "config.json")
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// loadOrDefaultConfig returns the existing config, or a fresh default when no
// config file exists yet. Setup uses it so re-running setup never drops repos.
func loadOrDefaultConfig(path string) (config, error) {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		return newDefaultConfig(), nil
	}
	return cfg, err
}

func validateConfig(cfg config) error {
	if cfg.IdleDebounce.Duration <= 0 {
		return fmt.Errorf("idle_debounce must be positive")
	}
	if cfg.FetchInterval.Duration <= 0 {
		return fmt.Errorf("fetch_interval must be positive")
	}
	names := make(map[string]bool)
	paths := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		if repo.Name == "" || repo.Path == "" || repo.Remote == "" {
			return fmt.Errorf("every repository needs name, path, and remote")
		}
		if !filepath.IsAbs(repo.Path) {
			return fmt.Errorf("repository %s path must be absolute", repo.Name)
		}
		if names[repo.Name] {
			return fmt.Errorf("repository %s appears more than once", repo.Name)
		}
		clean := filepath.Clean(repo.Path)
		if paths[clean] {
			return fmt.Errorf("path %s appears more than once", clean)
		}
		names[repo.Name] = true
		paths[clean] = true
	}
	return nil
}

func writeConfig(path string, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repo-sync-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type configStore struct {
	path string
	mu   sync.Mutex
}

func (s *configStore) load() (config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadConfig(s.path)
}

// update applies fn to the current config and writes the result atomically.
func (s *configStore) update(fn func(*config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := loadOrDefaultConfig(s.path)
	if err != nil {
		return err
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	return writeConfig(s.path, cfg)
}

// findRepo returns the configured repository whose path matches, if any.
func (cfg config) findRepo(path string) (int, bool) {
	clean := filepath.Clean(path)
	for i, repo := range cfg.Repositories {
		if filepath.Clean(repo.Path) == clean {
			return i, true
		}
	}
	return -1, false
}

// addRepository registers a local clone. The name is derived from the folder
// name and made unique when needed.
func addRepository(cfg *config, path string) (repoConfig, error) {
	if !filepath.IsAbs(path) {
		return repoConfig{}, fmt.Errorf("path must be absolute: %s", path)
	}
	if _, exists := cfg.findRepo(path); exists {
		return repoConfig{}, fmt.Errorf("%s is already synced", path)
	}
	base := filepath.Base(path)
	name := base
	for i := 2; nameTaken(cfg.Repositories, name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	repo := repoConfig{Name: name, Path: filepath.Clean(path), Remote: "origin"}
	cfg.Repositories = append(cfg.Repositories, repo)
	sort.Slice(cfg.Repositories, func(i, j int) bool {
		return cfg.Repositories[i].Name < cfg.Repositories[j].Name
	})
	return repo, nil
}

func nameTaken(repos []repoConfig, name string) bool {
	for _, repo := range repos {
		if repo.Name == name {
			return true
		}
	}
	return false
}

// allowPath adds a secret-guard override for one repo-relative path.
func allowPath(cfg *config, repoPath, rel string) error {
	i, ok := cfg.findRepo(repoPath)
	if !ok {
		return fmt.Errorf("%s is not a synced repository; run `repo-sync add` first", repoPath)
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return fmt.Errorf("allow path must be inside the repository: %s", rel)
	}
	for _, existing := range cfg.Repositories[i].Allow {
		if existing == rel {
			return nil
		}
	}
	cfg.Repositories[i].Allow = append(cfg.Repositories[i].Allow, rel)
	sort.Strings(cfg.Repositories[i].Allow)
	return nil
}
