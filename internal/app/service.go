package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const servicePATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

type launchService struct {
	runner commandRunner
	domain string
	label  string
}

type launchState struct {
	loaded bool
	pid    int
}

func defaultService() *launchService {
	return &launchService{runner: execCommandRunner{}, domain: launchdDomain(), label: launchAgentLabel}
}
func launchdDomain() string             { return "gui/" + strconv.Itoa(os.Getuid()) }
func (s *launchService) target() string { return s.domain + "/" + s.label }
func (s *launchService) plistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", s.label+".plist")
}

var launchPID = regexp.MustCompile(`(?m)^\s*pid = ([0-9]+)\s*$`)

func (s *launchService) inspect(ctx context.Context) (launchState, error) {
	output, err := s.runner.run(ctx, "", "", "/bin/launchctl", "print", s.target())
	if err != nil {
		if strings.Contains(output, "Could not find service") && strings.Contains(output, s.label) {
			return launchState{}, nil
		}
		return launchState{}, fmt.Errorf("check background service: %w", err)
	}
	state := launchState{loaded: true}
	if match := launchPID.FindStringSubmatch(output); match != nil {
		state.pid, _ = strconv.Atoi(match[1])
	}
	return state, nil
}

func (s *launchService) start(ctx context.Context, plistPath string) error {
	_, err := s.runner.run(ctx, "", "", "/bin/launchctl", "bootstrap", s.domain, plistPath)
	if err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func (s *launchService) stop(ctx context.Context) error {
	before, err := s.inspect(ctx)
	if err != nil || !before.loaded {
		return err
	}
	if _, err := s.runner.run(ctx, "", "", "/bin/launchctl", "bootout", s.target()); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		state, err := s.inspect(ctx)
		if err != nil {
			return err
		}
		processStopped := before.pid == 0 || errors.Is(syscall.Kill(before.pid, 0), syscall.ESRCH)
		if !state.loaded && processStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service did not stop; files were kept. Check `repo-sync status` and retry")
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func (s *launchService) waitReady(ctx context.Context, configPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stablePID := 0
	var stableSince time.Time
	var lastErr error
	for {
		_, err := s.readStatus(ctx, configPath)
		if err == nil {
			state, inspectErr := s.inspect(ctx)
			if inspectErr != nil {
				return inspectErr
			}
			if state.pid != stablePID {
				stablePID, stableSince = state.pid, time.Now()
			}
			if stablePID > 0 && time.Since(stableSince) >= 2*time.Second {
				return nil
			}
		} else {
			lastErr, stablePID = err, 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service did not become ready: %v. Check ~/Library/Logs/repo-sync/stderr.log", lastErr)
		}
		if err := waitContext(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func restartDaemon() error {
	s := defaultService()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := s.plistPath(home)
	configPath, err := installedConfigPath(plistPath)
	if err != nil {
		return err
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	ctx := context.Background()
	if err := s.stop(ctx); err != nil {
		return err
	}
	if err := s.start(ctx, plistPath); err != nil {
		return err
	}
	return s.waitReady(ctx, configPath, 15*time.Second)
}

type fileSnapshot struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotFile(path string) (fileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("%s must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	return fileSnapshot{data: data, mode: info.Mode().Perm(), exists: true}, err
}
func (f fileSnapshot) restore(path string) error {
	if f.exists {
		return writeFileAtomic(path, f.data, f.mode)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
