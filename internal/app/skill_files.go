package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type skillRecord struct {
	Version int         `json:"version"`
	Offered bool        `json:"offered"`
	Copies  []skillCopy `json:"copies"`
}

type skillCopy struct {
	Path     string            `json:"path"`
	Parent   string            `json:"resolved_parent"`
	Hashes   map[string]string `json:"hashes"`
	Pending  map[string]string `json:"pending,omitempty"`
	Removing bool              `json:"removing,omitempty"`
}

func readSkillRecord() (skillRecord, error) {
	snapshot, err := snapshotFile(skillRecordPath())
	if err != nil {
		return skillRecord{}, err
	}
	record := skillRecord{Version: 1}
	if snapshot.exists {
		record.Version = 0
		if err := json.Unmarshal(snapshot.data, &record); err != nil {
			return record, fmt.Errorf("read agent skill record: %w", err)
		}
	}
	return record, validateSkillRecord(record)
}

func validateSkillRecord(record skillRecord) error {
	if record.Version != 1 {
		return fmt.Errorf("unsupported agent skill record version %d", record.Version)
	}
	seen := map[string]bool{}
	for _, copy := range record.Copies {
		if !filepath.IsAbs(copy.Path) || filepath.Clean(copy.Path) != copy.Path || filepath.Base(copy.Path) != "repo-sync" || filepath.Base(filepath.Dir(copy.Path)) != "skills" || !filepath.IsAbs(copy.Parent) || filepath.Clean(copy.Parent) != copy.Parent || strings.ContainsRune(copy.Path+copy.Parent, 0) {
			return fmt.Errorf("invalid managed skill path %q", copy.Path)
		}
		if seen[copy.Path] {
			return fmt.Errorf("duplicate managed skill path %q", copy.Path)
		}
		seen[copy.Path] = true
		if !copy.Removing && copy.Hashes["SKILL.md"] == "" && copy.Pending["SKILL.md"] == "" {
			return fmt.Errorf("missing SKILL.md ownership for %s", copy.Path)
		}
		for _, hashes := range []map[string]string{copy.Hashes, copy.Pending} {
			for path, hash := range hashes {
				if !fs.ValidPath(path) || path == "." || strings.ContainsRune(path, '\\') {
					return fmt.Errorf("invalid skill file %q", path)
				}
				if hash != "directory" {
					decoded, err := hex.DecodeString(hash)
					if err != nil || len(decoded) != sha256.Size {
						return fmt.Errorf("invalid skill hash for %s", path)
					}
				} else if path == "SKILL.md" {
					return errors.New("SKILL.md must be a file")
				}
				for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
					if hashes[parent] != "directory" {
						return fmt.Errorf("missing skill directory ownership for %s", parent)
					}
				}
			}
		}
	}
	return nil
}

func writeSkillRecord(record skillRecord) error {
	if err := validateSkillRecord(record); err != nil {
		return err
	}
	if _, err := snapshotFile(skillRecordPath()); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(skillRecordPath(), append(data, '\n'), 0o600)
}

func skillHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func readSkillBundle(bundle fs.FS) (map[string][]byte, map[string]string, error) {
	if bundle == nil {
		return nil, nil, errors.New("this build has no bundled agent skill")
	}
	files := map[string][]byte{}
	hashes := map[string]string{}
	err := fs.WalkDir(bundle, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if entry.IsDir() {
			hashes[path] = "directory"
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("bundled skill must contain regular files: %s", path)
		}
		data, err := fs.ReadFile(bundle, path)
		if err != nil {
			return err
		}
		files[path], hashes[path] = data, skillHash(data)
		return nil
	})
	if err == nil && len(files["SKILL.md"]) == 0 {
		err = errors.New("bundled SKILL.md is missing or empty")
	}
	return files, hashes, err
}

// A changed parent or skill symlink is a customization, not permission to
// follow a different tree. Directory entries are included to preserve extras.
func inspectSkillCopy(copy skillCopy) (map[string]string, bool, error) {
	if canonicalInstallPath(filepath.Dir(copy.Path)) != copy.Parent {
		return map[string]string{"!parent": "changed"}, true, nil
	}
	info, err := os.Lstat(copy.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return map[string]string{"!root": "not a directory"}, true, nil
	}
	hashes := map[string]string{}
	err = filepath.WalkDir(copy.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == copy.Path {
			return nil
		}
		relative, err := filepath.Rel(copy.Path, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			hashes[relative] = "directory"
			return nil
		}
		if !entry.Type().IsRegular() {
			hashes[relative] = "not a regular file"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[relative] = skillHash(data)
		return nil
	})
	return hashes, true, err
}

func verifySkillCopy(copy skillCopy, expected map[string]string, exists bool) error {
	current, present, err := inspectSkillCopy(copy)
	if err != nil {
		return err
	}
	if present != exists || !maps.Equal(current, expected) {
		return errors.New("skill folder changed during operation; preserved remaining files")
	}
	return nil
}

func writeSkillCopy(copy skillCopy, current map[string]string, exists bool, files map[string][]byte) error {
	if err := verifySkillCopy(copy, current, exists); err != nil {
		return err
	}
	if !exists {
		if err := os.MkdirAll(filepath.Dir(copy.Path), 0o755); err != nil {
			return err
		}
		if err := verifySkillCopy(copy, current, false); err != nil {
			return err
		}
		if err := os.Mkdir(copy.Path, 0o755); err != nil {
			return err
		}
	}
	expected := maps.Clone(current)
	if expected == nil {
		expected = map[string]string{}
	}
	wanted := map[string]string{}
	for path, data := range files {
		wanted[path] = skillHash(data)
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			wanted[parent] = "directory"
		}
	}
	// Remove obsolete files and empty directories, deepest first. No recursive
	// deletion: unexpected files always survive even after a concurrent edit.
	for _, path := range skillEntryOrder(expected, true) {
		if wanted[path] == expected[path] || (wanted[path] != "" && wanted[path] != "directory" && expected[path] != "directory") {
			continue
		}
		if err := verifySkillCopy(copy, expected, true); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(copy.Path, path)); err != nil {
			return err
		}
		delete(expected, path)
	}
	for _, path := range skillEntryOrder(wanted, false) {
		if wanted[path] == expected[path] {
			continue
		}
		if err := verifySkillCopy(copy, expected, true); err != nil {
			return err
		}
		var err error
		if wanted[path] == "directory" {
			err = os.Mkdir(filepath.Join(copy.Path, path), 0o755)
		} else {
			err = writeFileAtomic(filepath.Join(copy.Path, path), files[path], 0o644)
		}
		if err != nil {
			return err
		}
		expected[path] = wanted[path]
	}
	return verifySkillCopy(copy, wanted, true)
}

func skillEntryOrder(entries map[string]string, descending bool) []string {
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		if descending {
			return paths[i] > paths[j]
		}
		return paths[i] < paths[j]
	})
	return paths
}

func removeSkillCopy(copy skillCopy, current map[string]string) error {
	expected := maps.Clone(current)
	for _, path := range skillEntryOrder(current, true) {
		if err := verifySkillCopy(copy, expected, true); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(copy.Path, path)); err != nil {
			return err
		}
		delete(expected, path)
	}
	if err := verifySkillCopy(copy, expected, true); err != nil {
		return err
	}
	return os.Remove(copy.Path)
}

// Persist intent before touching files. After an interrupted operation, only
// entries matching the authorized old/new contents are still owned.
func matchesManagedSkill(current map[string]string, copy skillCopy) bool {
	if copy.Pending == nil && !copy.Removing {
		return maps.Equal(current, copy.Hashes)
	}
	for path, hash := range current {
		if copy.Hashes[path] != hash && copy.Pending[path] != hash {
			return false
		}
	}
	return true
}

func managedSkillRecord(record skillRecord) skillRecord {
	copies := []skillCopy{}
	for _, copy := range record.Copies {
		if len(copy.Hashes) > 0 || copy.Pending != nil || copy.Removing {
			copies = append(copies, copy)
		}
	}
	record.Copies = copies
	return record
}
