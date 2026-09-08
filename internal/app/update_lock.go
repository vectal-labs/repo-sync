package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var errUpdateBusy = errors.New("another update or uninstall is in progress; retry later")

func acquireUpdateLock() (func(), error) {
	return lockUpdateFile(filepath.Join(updateCacheDir(), "update.lock"), syscall.LOCK_EX)
}

func updateGatePath() string { return filepath.Join(updateCacheDir(), "sync.lock") }

// Separate open file descriptions let independent sync cycles share the gate.
// The updater takes it exclusively; a crashed process releases it automatically.
func lockUpdateFile(path string, operation int) (func(), error) {
	file, err := lockUpdateFileHandle(path, operation)
	if err != nil {
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

func lockUpdateFileHandle(path string, operation int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, operation|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errUpdateBusy
		}
		return nil, fmt.Errorf("lock update state: %w", err)
	}
	// Closing the last descriptor releases flock. Do not explicitly unlock:
	// a supervised Homebrew child may still hold a duplicate after a crash.
	return file, nil
}
