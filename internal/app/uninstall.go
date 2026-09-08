package app

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type uninstallOptions struct {
	configPath        string
	binary            string
	yes               bool
	keepBinary        bool
	in                io.Reader
	out               io.Writer
	service           *launchService
	binaryPaths       []string // nil searches standard locations; tests supply isolated candidates
	discoverProcesses func(context.Context, []string) ([]ownedProcess, error)
	stopProcesses     func(context.Context, []ownedProcess, time.Duration) error
}

type binaryRemoval struct {
	executable, brew, kind string
	paths                  []string
	aliases                []binaryAlias
}

func planBinaryRemoval(binary string) (binaryRemoval, error) {
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return binaryRemoval{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return binaryRemoval{}, err
	}
	if !isRepoSyncBinary(resolved) {
		return binaryRemoval{}, fmt.Errorf("cannot remove unrecognized executable %s; use --keep-binary", resolved)
	}
	for _, item := range []struct{ dir, kind string }{{"Caskroom", "--cask"}, {"Cellar", "--formula"}} {
		marker := string(filepath.Separator) + item.dir + "/repo-sync/"
		if prefix, _, found := strings.Cut(resolved, marker); found {
			brew := filepath.Join(prefix, "bin", "brew")
			info, err := os.Stat(brew)
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				return binaryRemoval{}, fmt.Errorf("Homebrew installation found but %s is unavailable; repair Homebrew or use --keep-binary", brew)
			}
			return binaryRemoval{brew: brew, kind: item.kind}, nil
		}
	}
	return binaryRemoval{executable: resolved}, nil
}

func runUninstall(ctx context.Context, opts uninstallOptions) error {
	var report cleanupReport
	defer report.print(opts.out)
	fail := func(err error) error { report.failed = append(report.failed, err.Error()); return err }
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	service := opts.service
	if service == nil {
		service = defaultService()
	}
	plan, err := buildCleanupPlan(home, opts, service)
	report.preserved = append(report.preserved, plan.preserved...)
	if err != nil {
		return fail(err)
	}
	discover := opts.discoverProcesses
	if discover == nil {
		discover = discoverRepoSyncProcesses
	}
	stop := opts.stopProcesses
	if stop == nil {
		stop = stopRepoSyncProcesses
	}
	processes, err := discover(ctx, plan.executables)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(opts.out, "Uninstall scope: this user's recorded and standard install locations.")
	fmt.Fprintln(opts.out, "Files to remove:")
	for _, path := range plan.files {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(opts.out, "  %s\n", path)
		}
	}
	for _, binary := range plan.binaries {
		for _, path := range binary.paths {
			fmt.Fprintf(opts.out, "  %s\n", path)
		}
	}
	if !opts.keepBinary {
		if _, err := os.Lstat(installRecordPath()); err == nil {
			fmt.Fprintf(opts.out, "  %s\n", installRecordPath())
		}
	}
	for _, process := range processes {
		fmt.Fprintf(opts.out, "Stop process: %d (%s)\n", process.PID, process.Executable)
	}
	fmt.Fprintln(opts.out, "Unchanged managed agent skills will be removed; customized folders are preserved.")
	fmt.Fprintln(opts.out, "Repositories, Git history, shared tools, and credentials are preserved.")
	if !opts.yes {
		fmt.Fprint(opts.out, "Type yes to uninstall: ")
		line, err := bufio.NewReader(opts.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(err)
		}
		if strings.TrimSpace(line) != "yes" {
			fmt.Fprintln(opts.out, "Uninstall cancelled.")
			return nil
		}
	}
	unlockUpdates, err := acquireUpdateLock()
	if err != nil {
		return fail(err)
	}
	defer unlockUpdates()
	updater := updaterService(service)
	if err := updater.stop(ctx); err != nil {
		return fail(err)
	}
	if err := service.stop(ctx); err != nil {
		return fail(err)
	}
	if err := stop(ctx, processes, 45*time.Second); err != nil {
		return fail(err)
	}
	if remaining, err := discover(ctx, plan.executables); err != nil {
		return fail(err)
	} else if len(remaining) > 0 {
		return fail(fmt.Errorf("repo-sync processes are still running; no files were removed"))
	}
	// A process may finish a write while the user confirms or shutdown is pending.
	if err := scanCleanupArtifacts(home, service, &plan); err != nil {
		return fail(err)
	}
	report.preserved = append(report.preserved, plan.preserved...)
	// Keep skill operations serialized until final lock-file cleanup. The
	// Homebrew uninstall hook sees the removed receipt and becomes a no-op.
	unlockSkills, err := lockUpdateFile(skillLockPath(), syscall.LOCK_EX)
	if err != nil {
		return fail(err)
	}
	defer unlockSkills()
	skillRecord, err := readSkillRecord()
	if err != nil {
		return fail(err)
	}
	if err := (skillManager{out: opts.out}).uninstall(skillRecord); err != nil {
		return fail(fmt.Errorf("remove agent skill: %w", err))
	}
	// Save discoveries before deleting the plist, so a partial uninstall can be retried.
	if err := writeInstallRecord(plan.record); err != nil {
		return fail(fmt.Errorf("save cleanup record: %w", err))
	}
	for _, path := range plan.files {
		report.remove(path)
	}
	report.verify(plan.files)
	for _, dir := range append(append([]string{}, plan.emptyDirs...), appDirectories(home)[1:]...) {
		report.removeEmptyDirectory(dir)
	}
	if state, err := service.inspect(ctx); err != nil {
		report.failed = append(report.failed, err.Error())
	} else if state.loaded {
		report.failed = append(report.failed, "background service is still registered")
	}
	if remaining, err := discover(ctx, plan.executables); err != nil {
		report.failed = append(report.failed, err.Error())
	} else if len(remaining) > 0 {
		report.failed = append(report.failed, "repo-sync processes restarted during cleanup")
	}
	// Keep the program available for retry if any data could not be removed.
	if len(report.failed) == 0 {
		for _, binary := range plan.binaries {
			removeBinary(ctx, service.runner, binary, &report)
			if len(report.failed) > 0 {
				break
			}
		}
	}
	if state, err := updater.inspect(ctx); err != nil {
		report.failed = append(report.failed, err.Error())
	} else if state.loaded {
		report.failed = append(report.failed, "update service is still registered")
	}
	// Verify the namespace again, including anything a package hook recreated.
	if err := scanCleanupArtifacts(home, service, &plan); err != nil {
		report.failed = append(report.failed, err.Error())
	}
	report.preserved = append(report.preserved, plan.preserved...)
	report.verify(plan.files)
	for _, dir := range append(append([]string{}, plan.emptyDirs...), appDirectories(home)[1:]...) {
		report.removeEmptyDirectory(dir)
	}
	if len(report.failed) == 0 {
		if opts.keepBinary {
			plan.record.ConfigPaths = nil
			if err := writeInstallRecord(plan.record); err != nil {
				report.failed = append(report.failed, err.Error())
			} else {
				report.preserved = append(report.preserved, installRecordPath()+" (tracks retained binaries)")
			}
		} else {
			report.remove(installRecordPath())
			report.verify([]string{installRecordPath()})
		}
	}
	if len(report.failed) == 0 {
		// Both jobs and owned processes are stopped before removing lock files.
		for _, name := range []string{"sync.lock", "update.lock", "skill.lock"} {
			path := filepath.Join(home, "Library", "Caches", "repo-sync", name)
			if err := safeRemovalPath(home, path); err != nil {
				report.failed = append(report.failed, err.Error())
				continue
			}
			report.remove(path)
		}
		report.removeEmptyDirectory(filepath.Join(home, "Library", "Caches", "repo-sync"))
	}
	report.removeEmptyDirectory(filepath.Dir(installRecordPath()))
	if len(report.failed) > 0 {
		if _, err := os.Stat(installRecordPath()); errors.Is(err, os.ErrNotExist) {
			if err := writeInstallRecord(plan.record); err != nil {
				report.failed = append(report.failed, fmt.Sprintf("retain cleanup record: %v", err))
			}
		}
		if _, err := os.Stat(installRecordPath()); err == nil {
			report.preserved = append(report.preserved, installRecordPath()+" (needed to retry cleanup)")
		}
		for _, binary := range plan.binaries {
			for _, path := range binary.paths {
				if _, err := os.Lstat(path); err == nil {
					report.preserved = append(report.preserved, path+" (cleanup incomplete)")
				}
			}
		}
		return fmt.Errorf("uninstall incomplete: %s", strings.Join(report.failed, "; "))
	}
	fmt.Fprintln(opts.out, "repo-sync uninstalled.")
	return nil
}

func removeBinary(ctx context.Context, runner commandRunner, binary binaryRemoval, report *cleanupReport) {
	for _, alias := range binary.aliases {
		if err := alias.verify(); err != nil {
			report.failed = append(report.failed, err.Error())
			return
		}
	}
	for _, path := range binary.paths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if !isRepoSyncBinary(path) {
			report.failed = append(report.failed, path+" (executable changed; preserved)")
			return
		}
	}
	if binary.brew != "" {
		args := []string{"uninstall", binary.kind}
		if binary.kind == "--formula" {
			args = append(args, "--force")
		}
		args = append(args, "repo-sync")
		if _, err := runner.run(ctx, "", "", binary.brew, args...); err != nil {
			report.failed = append(report.failed, fmt.Sprintf("Homebrew removal failed: %v; run `%s %s`", err, binary.brew, strings.Join(args, " ")))
			return
		}
		removeBinaryAliases(binary.aliases, report)
		report.verify(binary.paths)
		for _, path := range binary.paths {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				report.removed = append(report.removed, path)
			}
		}
		return
	}
	// Remove symlink aliases individually, then the executable itself.
	before := len(report.failed)
	removeBinaryAliases(binary.aliases, report)
	if len(report.failed) != before {
		return
	}
	if binary.executable != "" {
		report.remove(binary.executable)
	}
	report.verify(binary.paths)
}

func uniquePaths(paths []string) []string {
	found := make(map[string]bool)
	for _, path := range paths {
		found[filepath.Clean(path)] = true
	}
	result := make([]string, 0, len(found))
	for path := range found {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func safeRemovalPath(home, path string) error {
	// Removing a symlink itself is safe; following a symlinked parent to delete
	// user data elsewhere is not. HOME itself may be a legitimate macOS alias.
	parent := filepath.Dir(path)
	for parent != home && parent != filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			// macOS exposes these system directories as aliases under /private.
			// Other symlinked parents still require the user to use the real path.
			resolved, resolveErr := filepath.EvalSymlinks(parent)
			systemAlias := (parent == "/var" || parent == "/tmp" || parent == "/etc") && resolveErr == nil && resolved == "/private"+parent
			if !systemAlias {
				return fmt.Errorf("refusing cleanup through symlinked directory %s; use its real path", parent)
			}
		}
		parent = filepath.Dir(parent)
	}
	info, err := os.Lstat(path)
	if err == nil && info.IsDir() {
		return fmt.Errorf("refusing to remove directory where a file was expected: %s", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func installedArguments(path string) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read installed service: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return nil, err
		}
		if key != "ProgramArguments" {
			continue
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return nil, err
			}
			start, ok = token.(xml.StartElement)
			if ok {
				break
			}
		}
		var array struct {
			Args []string `xml:"string"`
		}
		if err := decoder.DecodeElement(&array, &start); err != nil {
			return nil, err
		}
		return array.Args, nil
	}
}

func installedConfigPath(path string) (string, error) {
	args, err := installedArguments(path)
	if err != nil {
		return "", err
	}
	for i, arg := range args {
		if arg == "--config" && i+1 < len(args) {
			return args[i+1], nil
		}
		if strings.HasPrefix(arg, "--config=") {
			return strings.TrimPrefix(arg, "--config="), nil
		}
	}
	return "", nil
}
