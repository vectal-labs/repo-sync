package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const skillUsage = `usage: repo-sync skill install|refresh|status|uninstall

  install     install the bundled skill for local agents
  refresh     update unchanged copies previously managed by repo-sync
  status      show managed copies and preserved customizations
  uninstall   remove unchanged managed copies; keep customized folders

Skills are shared across configs. Restart your agent session after installation.`

type skillManager struct {
	bundle fs.FS
	out    io.Writer
}

func skillRecordPath() string {
	return filepath.Join(filepath.Dir(defaultConfigPath()), "skill-install.json")
}
func skillLockPath() string { return filepath.Join(updateCacheDir(), "skill.lock") }

func (m skillManager) run(command string) error {
	if command == "status" {
		return m.status()
	}
	if command != "install" && command != "refresh" && command != "uninstall" {
		return errors.New(skillUsage)
	}
	// Package hooks must be a no-op for users who have never opted in.
	if command != "install" {
		if _, err := os.Lstat(skillRecordPath()); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
	}
	unlock, err := lockUpdateFile(skillLockPath(), syscall.LOCK_EX)
	if errors.Is(err, errUpdateBusy) {
		return errors.New("another skill operation is in progress; retry later")
	}
	if err != nil {
		return err
	}
	defer unlock()
	record, err := readSkillRecord()
	if err != nil {
		return err
	}
	if command == "uninstall" {
		return m.uninstall(record)
	}
	files, hashes, err := readSkillBundle(m.bundle)
	if err != nil {
		return err
	}
	if command == "install" {
		paths, err := skillInstallPaths()
		if err != nil {
			return err
		}
		for _, path := range paths {
			found := false
			for _, copy := range record.Copies {
				if canonicalInstallPath(copy.Path) == canonicalInstallPath(path) {
					found = true
				}
			}
			if !found {
				record.Copies = append(record.Copies, skillCopy{Path: path, Parent: canonicalInstallPath(filepath.Dir(path))})
			}
		}
		record.Offered = true
	}
	var failures []error
	for i := range record.Copies {
		copy := &record.Copies[i]
		current, exists, err := inspectSkillCopy(*copy)
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect %s: %w", copy.Path, err))
			continue
		}
		// An identical unowned copy can be adopted by explicit installation.
		// Interrupted operations retain their old/new hashes for safe retries.
		if exists && !matchesManagedSkill(current, *copy) && !(command == "install" && maps.Equal(current, hashes)) {
			fmt.Fprintf(m.out, "Preserved %s (customized or not managed).\n", copy.Path)
			continue
		}
		if copy.Removing && command == "refresh" {
			fmt.Fprintf(m.out, "Removal incomplete for %s; run repo-sync skill uninstall.\n", copy.Path)
			continue
		}
		if !exists && command == "refresh" && copy.Pending == nil {
			fmt.Fprintf(m.out, "Preserved missing %s; use skill install to restore it.\n", copy.Path)
			continue
		}
		if !exists || !maps.Equal(current, hashes) {
			copy.Hashes, copy.Pending, copy.Removing = current, hashes, false
			if err := writeSkillRecord(managedSkillRecord(record)); err != nil {
				return errors.Join(append(failures, err)...)
			}
			if err := writeSkillCopy(*copy, current, exists, files); err != nil {
				failures = append(failures, fmt.Errorf("install %s: %w", copy.Path, err))
				continue
			}
		}
		copy.Hashes, copy.Pending, copy.Removing = hashes, nil, false
		fmt.Fprintf(m.out, "Installed %s\n", copy.Path)
	}
	if err := writeSkillRecord(managedSkillRecord(record)); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (m skillManager) status() error {
	record, err := readSkillRecord()
	if err != nil {
		return err
	}
	if len(record.Copies) == 0 {
		fmt.Fprintln(m.out, "No managed skill copies. Run repo-sync skill install.")
		return nil
	}
	_, bundled, err := readSkillBundle(m.bundle)
	if err != nil {
		return err
	}
	for _, copy := range record.Copies {
		current, exists, err := inspectSkillCopy(copy)
		if err != nil {
			return err
		}
		state := "customized; preserved"
		if !exists {
			state = "missing"
		} else if matchesManagedSkill(current, copy) {
			state = "installed; current"
			if !maps.Equal(current, bundled) {
				state = "update available; run repo-sync skill refresh"
			}
		}
		if copy.Pending != nil && matchesManagedSkill(current, copy) {
			state = "installation incomplete; run repo-sync skill refresh"
		}
		if copy.Removing {
			state = "removal incomplete; run repo-sync skill uninstall"
		}
		fmt.Fprintf(m.out, "%s: %s\n", copy.Path, state)
	}
	return nil
}

func (m skillManager) uninstall(record skillRecord) error {
	var failures []error
	var remaining []skillCopy
	for i := range record.Copies {
		copy := &record.Copies[i]
		current, exists, err := inspectSkillCopy(*copy)
		if err == nil && exists {
			if !matchesManagedSkill(current, *copy) {
				fmt.Fprintf(m.out, "Preserved %s (customized).\n", copy.Path)
				continue
			}
			copy.Hashes, copy.Pending, copy.Removing = current, nil, true
			if err := writeSkillRecord(record); err != nil {
				return errors.Join(append(failures, err)...)
			}
			err = removeSkillCopy(*copy, current)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("remove %s: %w", copy.Path, err))
			remaining = append(remaining, *copy)
		} else {
			fmt.Fprintf(m.out, "Removed %s\n", copy.Path)
		}
	}
	if len(failures) > 0 {
		record.Copies = remaining
		if err := writeSkillRecord(record); err != nil {
			failures = append(failures, err)
		}
	} else if err := os.Remove(skillRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func skillInstallPaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(home, ".agents", "skills", "repo-sync")}
	for _, agent := range []struct{ env, dir, command string }{
		{"CLAUDE_CONFIG_DIR", ".claude", "claude"}, {"HERMES_HOME", ".hermes", "hermes"},
	} {
		root := os.Getenv(agent.env)
		explicit := root != ""
		if !explicit {
			root = filepath.Join(home, agent.dir)
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("%s must be an absolute path", agent.env)
		}
		info, err := os.Stat(root)
		detected := err == nil && info.IsDir()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		_, commandErr := exec.LookPath(agent.command)
		if explicit || detected || commandErr == nil {
			paths = append(paths, filepath.Join(root, "skills", "repo-sync"))
		}
	}
	// Older manual installs can shadow the shared skill. Include existing
	// native copies, but do not create redundant copies for new installations.
	piRoot := os.Getenv("PI_CODING_AGENT_DIR")
	if piRoot == "" {
		piRoot = filepath.Join(home, ".pi", "agent")
	}
	if !filepath.IsAbs(piRoot) {
		return nil, errors.New("PI_CODING_AGENT_DIR must be an absolute path")
	}
	for _, root := range []string{piRoot, filepath.Join(home, ".cursor")} {
		target := filepath.Join(root, "skills", "repo-sync")
		if _, err := os.Lstat(target); err == nil {
			paths = append(paths, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var unique []string
	for _, path := range paths {
		canonical := canonicalInstallPath(path)
		if !seen[canonical] {
			unique = append(unique, path)
			seen[canonical] = true
		}
	}
	return unique, nil
}

func (m skillManager) offer(input *bufio.Reader) {
	if m.bundle == nil {
		return
	}
	record, err := readSkillRecord()
	if err != nil {
		fmt.Fprintf(m.out, "Agent skill: %v\n", err)
		return
	}
	if len(record.Copies) > 0 {
		if err := m.run("refresh"); err != nil {
			fmt.Fprintf(m.out, "Agent skill refresh failed: %v\n", err)
		}
		return
	}
	if record.Offered {
		return
	}
	fmt.Fprint(m.out, "Install the repo-sync skill for your local AI agents? [y/N]: ")
	line, err := input.ReadString('\n')
	if err != nil {
		fmt.Fprintln(m.out, "Skipped. Install later with repo-sync skill install.")
		return
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer == "y" || answer == "yes" {
		if err := m.run("install"); err != nil {
			fmt.Fprintf(m.out, "Agent skill installation failed: %v\n", err)
		}
		return
	}
	unlock, err := lockUpdateFile(skillLockPath(), syscall.LOCK_EX)
	if err == nil {
		defer unlock()
		// Another process may have installed the skill while the prompt was open.
		record, err = readSkillRecord()
		if err == nil {
			record.Offered = true
			err = writeSkillRecord(record)
		}
	}
	if err != nil {
		fmt.Fprintf(m.out, "Could not save agent skill preference: %v\n", err)
	}
}
