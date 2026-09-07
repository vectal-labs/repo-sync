package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type binaryAlias struct {
	path, target string
	resolved     string // empty only for a recorded alias whose recorded target is absent
}

// Resolve the parent, not the final component: a package manager can remove an
// intermediate symlink, but the original alias must still name the same target.
func normalizedBinaryPath(path string) string {
	return filepath.Join(canonicalInstallPath(filepath.Dir(path)), filepath.Base(path))
}

func readBinaryAlias(path string) (binaryAlias, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return binaryAlias{}, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return binaryAlias{path: path, target: normalizedBinaryPath(filepath.Clean(target))}, nil
}

func captureBinaryAliases(path, resolved string) ([]binaryAlias, error) {
	var aliases []binaryAlias
	for range 255 {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return aliases, nil
		}
		alias, err := readBinaryAlias(path)
		if err != nil {
			return nil, err
		}
		alias.resolved = resolved
		aliases = append(aliases, alias)
		path = alias.target
	}
	return nil, fmt.Errorf("executable symlink chain changed while planning cleanup")
}

func (alias binaryAlias) verify() error {
	current, err := readBinaryAlias(alias.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || current.target != alias.target {
		return fmt.Errorf("%s (symlink changed; preserved)", alias.path)
	}
	resolved, err := filepath.EvalSymlinks(alias.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || alias.resolved == "" || resolved != alias.resolved {
		return fmt.Errorf("%s (symlink target changed; preserved)", alias.path)
	}
	return nil
}

func removeBinaryAliases(aliases []binaryAlias, report *cleanupReport) {
	for _, alias := range aliases {
		if err := alias.verify(); err != nil {
			report.failed = append(report.failed, err.Error())
			continue
		}
		report.remove(alias.path)
	}
}
