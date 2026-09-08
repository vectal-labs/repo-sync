package app

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const repoSyncModulePath = "github.com/vectal-labs/repo-sync"

func isRepoSyncBinary(path string) bool {
	info, err := buildinfo.ReadFile(path)
	return err == nil && info.Path == repoSyncModulePath
}

type cleanupPlan struct {
	files       []string
	binaries    []binaryRemoval
	executables []string
	preserved   []string
	record      installRecord
}

type cleanupReport struct{ removed, preserved, failed []string }

var (
	ownedTemporary = regexp.MustCompile(`^\.repo-sync-[0-9]+$`)
	ownedLog       = regexp.MustCompile(`^(updates-)?(stdout|stderr)\.log(\.[0-9]+(\.gz)?)?$`)
	ownedStatus    = regexp.MustCompile(`^(status|updates)-[a-f0-9]{64}\.json$`)
)

func appDirectories(home string) []string {
	return []string{filepath.Dir(defaultConfigPath()), filepath.Join(home, "Library", "Logs", "repo-sync"), filepath.Join(home, "Library", "Caches", "repo-sync")}
}

func buildCleanupPlan(home string, opts uninstallOptions, service *launchService) (cleanupPlan, error) {
	record, err := loadInstallRecord()
	if err != nil {
		return cleanupPlan{}, err
	}
	plan := cleanupPlan{record: record}
	for _, path := range record.BinaryPaths {
		plan.executables = append(plan.executables, canonicalInstallPath(path))
	}
	configPaths := append([]string{defaultConfigPath(), opts.configPath}, record.ConfigPaths...)
	installed, err := installedConfigPath(service.plistPath(home))
	if err != nil {
		return plan, err
	}
	if installed != "" {
		configPaths = append(configPaths, installed)
	}
	if installed, err := installedConfigPath(updaterService(service).plistPath(home)); err != nil {
		return plan, err
	} else if installed != "" {
		configPaths = append(configPaths, installed)
	}
	plan.files = append(plan.files, service.plistPath(home), updaterService(service).plistPath(home), updateSettingsPath())
	for _, path := range uniquePaths(configPaths) {
		if path == "." || path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return plan, err
		}
		if err := validateInstallConfigPath(absolute); err != nil {
			return plan, err
		}
		if err := safeRemovalPath(home, absolute); err != nil {
			return plan, err
		}
		explicit, _ := filepath.Abs(opts.configPath)
		if absolute != defaultConfigPath() && absolute != explicit {
			if _, err := loadConfig(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
				return plan, fmt.Errorf("cannot verify custom config %s: %w; use --config explicitly to remove it", absolute, err)
			}
		}
		plan.record.ConfigPaths = append(plan.record.ConfigPaths, absolute)
		plan.files = append(plan.files, absolute, statusPath(absolute), updateStatePath(absolute))
	}
	if err := scanCleanupArtifacts(home, service, &plan); err != nil {
		return plan, err
	}
	candidates := append([]string{opts.binary}, record.BinaryPaths...)
	if opts.binaryPaths == nil {
		candidates = append(candidates, standardBinaryPaths(home)...)
	} else {
		candidates = append(candidates, opts.binaryPaths...)
	}
	// The plist can point at an older versioned binary after an interrupted upgrade.
	if path, err := installedBinaryPath(service.plistPath(home)); err != nil {
		return plan, err
	} else if path != "" {
		candidates = append(candidates, path)
		plan.executables = append(plan.executables, canonicalInstallPath(path))
	}
	groups := make(map[string]int)
	recordedBinaries := make(map[string]bool)
	for _, path := range record.BinaryPaths {
		recordedBinaries[normalizedBinaryPath(path)] = true
	}
	for _, path := range uniquePaths(candidates) {
		if path == "" || path == "." {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return plan, err
		}
		info, err := os.Lstat(absolute)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return plan, fmt.Errorf("inspect executable %s: %w", absolute, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			alias, err := readBinaryAlias(absolute)
			if err != nil {
				return plan, err
			}
			if _, err := os.Stat(alias.target); errors.Is(err, os.ErrNotExist) && recordedBinaries[normalizedBinaryPath(absolute)] && recordedBinaries[alias.target] {
				if opts.keepBinary {
					plan.preserved = append(plan.preserved, absolute+" (--keep-binary)")
				} else {
					plan.binaries = append(plan.binaries, binaryRemoval{paths: []string{absolute}, aliases: []binaryAlias{alias}})
				}
				continue
			}
		}
		if info.IsDir() || !isRepoSyncBinary(absolute) {
			plan.preserved = append(plan.preserved, absolute+" (not a verified repo-sync executable)")
			if absolute == opts.binary && !opts.keepBinary {
				return plan, fmt.Errorf("cannot verify running executable %s; use --keep-binary", absolute)
			}
			continue
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return plan, err
		}
		targetInfo, err := os.Stat(resolved)
		if err != nil {
			return plan, err
		}
		if stat, ok := targetInfo.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) && absolute != opts.binary {
			plan.preserved = append(plan.preserved, absolute+" (owned by another user)")
			continue
		}
		plan.executables = append(plan.executables, resolved)
		plan.record.BinaryPaths = append(plan.record.BinaryPaths, absolute, resolved)
		aliases, err := captureBinaryAliases(absolute, resolved)
		if err != nil {
			return plan, err
		}
		for _, alias := range aliases {
			plan.record.BinaryPaths = append(plan.record.BinaryPaths, alias.path, alias.target)
		}
		if opts.keepBinary {
			plan.preserved = append(plan.preserved, absolute+" (--keep-binary)")
			continue
		}
		removal, err := planBinaryRemoval(absolute)
		if err != nil {
			return plan, err
		}
		removal.paths = uniquePaths([]string{absolute, resolved})
		removal.aliases = aliases
		for _, alias := range aliases {
			removal.paths = append(removal.paths, alias.path)
		}
		removal.paths = uniquePaths(removal.paths)
		key := removal.executable
		if removal.brew != "" {
			key = removal.brew + " " + removal.kind
		}
		if index, ok := groups[key]; ok {
			plan.binaries[index].paths = uniquePaths(append(plan.binaries[index].paths, removal.paths...))
			plan.binaries[index].aliases = append(plan.binaries[index].aliases, removal.aliases...)
		} else {
			groups[key] = len(plan.binaries)
			plan.binaries = append(plan.binaries, removal)
		}
	}
	plan.files = uniquePaths(plan.files)
	for _, path := range plan.files {
		if err := safeRemovalPath(home, path); err != nil {
			return plan, err
		}
	}
	plan.executables = uniquePaths(plan.executables)
	plan.record.ConfigPaths = uniquePaths(plan.record.ConfigPaths)
	plan.record.BinaryPaths = uniquePaths(plan.record.BinaryPaths)
	plan.preserved = uniquePaths(plan.preserved)
	if err := validateInstallRecord(plan.record); err != nil {
		return plan, err
	}
	currentBinary := canonicalInstallPath(opts.binary)
	containsCurrent := func(binary binaryRemoval) bool {
		for _, path := range binary.paths {
			if canonicalInstallPath(path) == currentBinary {
				return true
			}
		}
		return false
	}
	// Keep the command available to retry cleanup if an older installation fails.
	sort.SliceStable(plan.binaries, func(i, j int) bool {
		return !containsCurrent(plan.binaries[i]) && containsCurrent(plan.binaries[j])
	})
	return plan, nil
}

func scanCleanupArtifacts(home string, service *launchService, plan *cleanupPlan) error {
	parents := []string{filepath.Dir(service.plistPath(home))}
	for _, path := range plan.record.ConfigPaths {
		parents = append(parents, filepath.Dir(path))
	}
	for _, dir := range uniquePaths(parents) {
		if err := scanCleanupDirectory(home, dir, false, ownedTemporary.MatchString, plan); err != nil {
			return err
		}
	}
	for i, dir := range appDirectories(home) {
		matches := ownedTemporary.MatchString
		if i == 1 {
			matches = ownedLog.MatchString
		}
		if i == 2 {
			matches = func(name string) bool { return ownedTemporary.MatchString(name) || ownedStatus.MatchString(name) }
		}
		if err := scanCleanupDirectory(home, dir, true, matches, plan); err != nil {
			return err
		}
	}
	plan.files = uniquePaths(plan.files)
	for _, path := range plan.files {
		if err := safeRemovalPath(home, path); err != nil {
			return err
		}
	}
	return nil
}

func scanCleanupDirectory(home, dir string, reportUnknown bool, matches func(string) bool, plan *cleanupPlan) error {
	if err := safeRemovalPath(home, filepath.Join(dir, ".repo-sync-inspection")); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cleanup directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == installRecordPath() || path == skillRecordPath() || path == skillLockPath() || path == filepath.Join(updateCacheDir(), "update.lock") || path == updateGatePath() || containsPath(plan.files, path) {
			continue
		}
		if matches(entry.Name()) && !entry.IsDir() {
			plan.files = append(plan.files, path)
		} else if reportUnknown {
			plan.preserved = append(plan.preserved, path+" (unrecognized file or directory)")
		}
	}
	return nil
}

func containsPath(paths []string, path string) bool {
	for _, existing := range paths {
		if existing == path {
			return true
		}
	}
	return false
}

func standardBinaryPaths(home string) []string {
	var paths []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			paths = append(paths, filepath.Join(dir, "repo-sync"))
		}
	}
	for _, dir := range []string{os.Getenv("GOBIN"), filepath.Join(home, "go", "bin"), filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin")} {
		if filepath.IsAbs(dir) {
			paths = append(paths, filepath.Join(dir, "repo-sync"))
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("GOPATH")) {
		if filepath.IsAbs(dir) {
			paths = append(paths, filepath.Join(dir, "bin", "repo-sync"))
		}
	}
	for _, prefix := range []string{"/opt/homebrew", "/usr/local"} {
		paths = append(paths, filepath.Join(prefix, "bin", "repo-sync"))
		for _, pattern := range []string{"Caskroom/repo-sync/*/repo-sync", "Cellar/repo-sync/*/bin/repo-sync"} {
			matches, _ := filepath.Glob(filepath.Join(prefix, pattern))
			paths = append(paths, matches...)
		}
	}
	return uniquePaths(paths)
}

func (r *cleanupReport) print(out io.Writer) {
	for _, section := range []struct {
		name  string
		paths []string
	}{{"Removed", r.removed}, {"Preserved", r.preserved}, {"Failed", r.failed}} {
		fmt.Fprintf(out, "%s:\n", section.name)
		if len(section.paths) == 0 {
			fmt.Fprintln(out, "  none")
			continue
		}
		paths := append([]string(nil), section.paths...)
		sort.Strings(paths)
		previous := ""
		for _, path := range paths {
			if path != previous {
				fmt.Fprintf(out, "  %s\n", path)
				previous = path
			}
		}
	}
}

func (r *cleanupReport) remove(path string) {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.failed = append(r.failed, fmt.Sprintf("%s: %v", path, err))
		return
	}
	r.removed = append(r.removed, path)
}

func (r *cleanupReport) removeEmptyDirectory(dir string) {
	err := os.Remove(dir)
	if err == nil {
		r.removed = append(r.removed, dir)
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		r.failed = append(r.failed, fmt.Sprintf("%s: %v", dir, err))
	}
}

func (r *cleanupReport) verify(paths []string) {
	for _, path := range uniquePaths(paths) {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				r.failed = append(r.failed, path+" (still present)")
			} else {
				r.failed = append(r.failed, fmt.Sprintf("%s: cannot verify removal: %v", path, err))
			}
		}
	}
}

func installedBinaryPath(path string) (string, error) {
	args, err := installedArguments(path)
	if err != nil || len(args) == 0 {
		return "", err
	}
	return strings.TrimSpace(args[0]), nil
}
