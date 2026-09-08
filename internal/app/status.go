package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type serviceStatus struct {
	PID          int                `json:"pid"`
	Version      string             `json:"version"`
	ConfigHash   string             `json:"config_hash"`
	UpdatedAt    time.Time          `json:"updated_at"`
	Repositories []repositoryStatus `json:"repositories"`
}
type repositoryStatus struct {
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Detail      string    `json:"detail,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
}

func statusPath(configPath string) string {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		absolute = configPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Caches", "repo-sync", fmt.Sprintf("status-%x.json", sha256.Sum256([]byte(absolute))))
}
func configHash(cfg config) string {
	data, _ := json.Marshal(cfg)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (d *daemon) statusSnapshot() serviceStatus {
	status := serviceStatus{PID: os.Getpid(), Version: appVersion(), ConfigHash: configHash(d.cfg), UpdatedAt: time.Now()}
	for _, state := range d.states {
		state.mu.Lock()
		repo := repositoryStatus{Name: state.config.Name, State: "waiting", LastSuccess: state.lastSuccess}
		switch {
		case state.conflict != nil:
			repo.State, repo.Detail = "conflict", conflictDetail(state.conflict)
		case state.conflictLoadError != "":
			repo.State, repo.Detail = "retrying", state.conflictLoadError
		case state.syncing:
			repo.State = "syncing"
		case state.incident != "":
			repo.State, repo.Detail = "retrying", redactCredentials(state.incident)
		case state.lastSkip != "":
			repo.State, repo.Detail = "waiting", state.lastSkip
		case !state.lastSuccess.IsZero():
			repo.State = "ok"
		default:
			repo.Detail = "waiting for the first sync cycle"
		}
		if state.timer != nil && repo.State == "ok" {
			repo.State, repo.Detail = "waiting", "local changes or a retry are scheduled"
		}
		state.mu.Unlock()
		status.Repositories = append(status.Repositories, repo)
	}
	sort.Slice(status.Repositories, func(i, j int) bool { return status.Repositories[i].Name < status.Repositories[j].Name })
	return status
}

func (d *daemon) publishStatus() error {
	if d.statusFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(d.statusSnapshot(), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(d.statusFile, append(data, '\n'), 0o600)
}
func (d *daemon) statusLoop(done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if err := d.publishStatus(); err != nil {
				d.logger.Printf("write service status: %v", err)
			}
		}
	}
}
func (s *launchService) readStatus(ctx context.Context, configPath string) (serviceStatus, error) {
	state, err := s.inspect(ctx)
	if err != nil {
		return serviceStatus{}, err
	}
	if !state.loaded || state.pid == 0 {
		return serviceStatus{}, fmt.Errorf("service is not running; run `%s`", configCommand("setup", configPath))
	}
	data, err := os.ReadFile(statusPath(configPath))
	if err != nil {
		return serviceStatus{}, fmt.Errorf("service is running but has not reported readiness; run `%s` if this continues", configCommand("setup", configPath))
	}
	var status serviceStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return status, fmt.Errorf("read service status: %w", err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return status, err
	}
	age := time.Since(status.UpdatedAt)
	if status.PID != state.pid || age > 10*time.Second || age < -time.Second {
		return status, fmt.Errorf("service status is stale; run `%s`", configCommand("setup", configPath))
	}
	if status.ConfigHash != configHash(cfg) {
		return status, fmt.Errorf("service is using different settings; run `%s` to apply this config", configCommand("setup", configPath))
	}
	return status, nil
}

func runStatus(ctx context.Context, configPath string, service *launchService, out io.Writer) error {
	installedVersion := appVersion()
	fmt.Fprintf(out, "Installed version: %s\n", installedVersion)
	updateErr := printUpdateStatus(configPath, out)
	status, err := service.readStatus(ctx, configPath)
	if err != nil {
		fmt.Fprintln(out, "Running version: unavailable")
		return errors.Join(updateErr, err)
	}
	if status.Version == "" {
		fmt.Fprintln(out, "Running version: unknown (this service predates version reporting)")
	} else {
		fmt.Fprintf(out, "Running version: %s\n", status.Version)
	}
	var versionErr error
	if stableReleaseVersion(installedVersion) && stableReleaseVersion(status.Version) && compareReleaseVersions(installedVersion, status.Version) != 0 {
		versionErr = fmt.Errorf("running version %s differs from installed version %s; run `%s`", status.Version, installedVersion, configCommand("update", configPath))
	}
	fmt.Fprintf(out, "Service is running (PID %d). Starts automatically at login.\n", status.PID)
	unhealthy := false
	for _, repo := range status.Repositories {
		fmt.Fprintf(out, "  %s: %s", repo.Name, repo.State)
		if repo.Detail != "" {
			fmt.Fprintf(out, " (%s)", repo.Detail)
		}
		if !repo.LastSuccess.IsZero() {
			fmt.Fprintf(out, "; last successful cycle %s", repo.LastSuccess.Local().Format("2006-01-02 15:04:05"))
		}
		fmt.Fprintln(out)
		if repo.State == "conflict" {
			fmt.Fprintf(out, "    Run `%s` for repair instructions.\n", configCommand("conflicts", configPath))
		}
		if repo.State == "retrying" || repo.State == "conflict" {
			unhealthy = true
		}
	}
	if len(status.Repositories) == 0 {
		fmt.Fprintln(out, "No repositories configured. Run `repo-sync setup`.")
	}
	if unhealthy {
		return errors.Join(updateErr, versionErr, fmt.Errorf("some repositories need attention; retries continue automatically"))
	}
	return errors.Join(updateErr, versionErr)
}

func configCommand(command, configPath string) string {
	result := "repo-sync " + command
	if filepath.Clean(configPath) != filepath.Clean(defaultConfigPath()) {
		result += " --config " + preflightShellQuote(configPath)
	}
	return result
}
