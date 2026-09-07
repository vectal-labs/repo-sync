package app

import (
	"bufio"
	"context"
	"encoding/json"
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
	configPath string
	binary     string
	yes        bool
	keepBinary bool
	in         io.Reader
	out        io.Writer
	service    *launchService
}

type binaryRemoval struct{ executable, link, brew, kind string }

func planBinaryRemoval(binary string) (binaryRemoval, error) {
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return binaryRemoval{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return binaryRemoval{}, err
	}
	if filepath.Base(resolved) != "repo-sync" {
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
	plan := binaryRemoval{executable: resolved}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		plan.link = absolute
	}
	return plan, nil
}

func runUninstall(ctx context.Context, opts uninstallOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	service := opts.service
	if service == nil {
		service = defaultService()
	}
	plistPath := service.plistPath(home)
	configPaths := []string{defaultConfigPath(), opts.configPath}
	if installed, err := installedConfigPath(plistPath); err != nil {
		return err
	} else if installed != "" {
		// A custom config may live among unrelated user files. Only remove the
		// exact file recorded by this service, and require it to be a repo-sync config.
		if filepath.Clean(installed) != filepath.Clean(defaultConfigPath()) && filepath.Clean(installed) != filepath.Clean(opts.configPath) {
			if _, err := loadConfig(installed); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("cannot verify custom config %s: %w; use --config explicitly to remove it", installed, err)
			}
		}
		configPaths = append(configPaths, installed)
	}
	files := []string{plistPath, filepath.Join(home, "Library", "Logs", "repo-sync", "stdout.log"), filepath.Join(home, "Library", "Logs", "repo-sync", "stderr.log")}
	uniqueConfigs := make(map[string]bool)
	for _, path := range configPaths {
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		uniqueConfigs[absolute] = true
		files = append(files, absolute, statusPath(absolute))
	}
	files = uniquePaths(files)
	for _, path := range files {
		if err := safeRemovalPath(home, path); err != nil {
			return err
		}
	}
	var binary binaryRemoval
	if !opts.keepBinary {
		binary, err = planBinaryRemoval(opts.binary)
		if err != nil {
			return err
		}
	}
	fmt.Fprintln(opts.out, "This removes repo-sync's background service and these files:")
	for _, path := range files {
		fmt.Fprintf(opts.out, "  %s\n", path)
	}
	if binary.brew != "" {
		fmt.Fprintf(opts.out, "Program: Homebrew uninstall %s repo-sync\n", binary.kind)
	} else if binary.executable != "" {
		fmt.Fprintf(opts.out, "Program: %s\n", binary.executable)
	}
	fmt.Fprintln(opts.out, "Your repositories and shared Git credentials stay in place.")
	if !opts.yes {
		fmt.Fprint(opts.out, "Type yes to uninstall: ")
		line, err := bufio.NewReader(opts.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if strings.TrimSpace(line) != "yes" {
			fmt.Fprintln(opts.out, "Uninstall cancelled.")
			return nil
		}
	}
	before, err := service.inspect(ctx)
	if err != nil {
		return err
	}
	for path := range uniqueConfigs {
		data, err := os.ReadFile(statusPath(path))
		if err != nil {
			continue
		}
		var status serviceStatus
		if json.Unmarshal(data, &status) == nil && status.PID > 0 && status.PID != before.pid && time.Since(status.UpdatedAt) < 10*time.Second && syscall.Kill(status.PID, 0) == nil {
			return fmt.Errorf("a foreground repo-sync process is active (PID %d); stop it before uninstalling", status.PID)
		}
	}
	if err := service.stop(ctx); err != nil {
		return err
	}
	// Remove only known files. Never recursively delete a config's parent folder.
	for _, path := range files {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w; service is stopped, rerun uninstall to finish", path, err)
		}
	}
	for _, dir := range []string{filepath.Dir(defaultConfigPath()), filepath.Join(home, "Library", "Logs", "repo-sync"), filepath.Join(home, "Library", "Caches", "repo-sync")} {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	if binary.brew != "" {
		// Do not use --zap: we already removed our exact files, while zap may erase
		// unrelated files a user placed in the same directory.
		if _, err := service.runner.run(ctx, "", "", binary.brew, "uninstall", binary.kind, "repo-sync"); err != nil {
			return fmt.Errorf("service and data removed, but Homebrew removal failed: %w; run `%s uninstall %s repo-sync`", err, binary.brew, binary.kind)
		}
	} else if binary.executable != "" {
		if binary.link != "" {
			if err := os.Remove(binary.link); err != nil {
				return err
			}
		}
		if err := os.Remove(binary.executable); err != nil {
			return fmt.Errorf("service and data removed, but remove program: %w", err)
		}
	}
	fmt.Fprintln(opts.out, "repo-sync uninstalled.")
	return nil
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

func installedConfigPath(path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read installed service: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return "", err
		}
		if key != "ProgramArguments" {
			continue
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return "", err
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
			return "", err
		}
		for i, arg := range array.Args {
			if arg == "--config" && i+1 < len(array.Args) {
				return array.Args[i+1], nil
			}
			if strings.HasPrefix(arg, "--config=") {
				return strings.TrimPrefix(arg, "--config="), nil
			}
		}
		return "", nil
	}
}
