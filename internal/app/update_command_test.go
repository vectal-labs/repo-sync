package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisedUpdateCancellationStopsNestedProcessGroups(t *testing.T) {
	testSupervisedUpdateInterruption(t, false)
}

func TestSupervisedUpdateTimeoutStopsNestedProcessGroups(t *testing.T) {
	testSupervisedUpdateInterruption(t, true)
}

func testSupervisedUpdateInterruption(t *testing.T, timeout bool) {
	t.Helper()
	root := t.TempDir()
	lock := updateCommandTestLock(t, filepath.Join(root, "update.lock"))
	supervisorPIDFile := filepath.Join(root, "supervisor.pid")
	workerPIDFile := filepath.Join(root, "worker.pid")
	childPIDFile := filepath.Join(root, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := execCommandRunner{timeout: 20 * time.Second, env: updateCommandTestEnvironment(supervisorPIDFile)}
	if timeout {
		base.timeout = 2 * time.Second
	}
	done := make(chan error, 1)
	go func() {
		_, err := runSupervisedUpdateCommandWith(ctx, base, []*os.File{lock}, updateCommandTestSupervisor(), os.Args[0], "-test.run=TestUpdateCommandHelperProcess", "--", "worker", workerPIDFile, childPIDFile)
		done <- err
	}()
	supervisor := updateCommandTestPID(t, supervisorPIDFile)
	t.Cleanup(func() { _ = drainUpdateCommandSession(supervisor, 0) })
	worker := updateCommandTestPID(t, workerPIDFile)
	child := updateCommandTestPID(t, childPIDFile)
	childGroup, err := syscall.Getpgid(child)
	if err != nil || childGroup == supervisor {
		t.Fatalf("fixture must use Homebrew-style nested process groups: %d, %v", childGroup, err)
	}
	wantErr := context.Canceled
	if timeout {
		wantErr = context.DeadlineExceeded
	} else {
		cancel()
	}
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("update command did not finish after cancellation")
	}
	updateCommandTestStopped(t, worker)
	updateCommandTestStopped(t, child)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	probe := updateCommandTestLock(t, filepath.Join(root, "update.lock"))
	probe.Close()
}

func TestSupervisedUpdateRetainsLocksAndCleansUpWhenUpdaterDies(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "update.lock")
	supervisorPIDFile := filepath.Join(root, "supervisor.pid")
	workerPIDFile := filepath.Join(root, "worker.pid")
	childPIDFile := filepath.Join(root, "child.pid")
	parent := exec.Command(os.Args[0], "-test.run=TestUpdateCommandHelperProcess", "--", "parent", lockPath, supervisorPIDFile, workerPIDFile, childPIDFile)
	parent.Env = updateCommandTestEnvironment(supervisorPIDFile)
	parent.Stdout, parent.Stderr = io.Discard, io.Discard
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill() })
	supervisor := updateCommandTestPID(t, supervisorPIDFile)
	t.Cleanup(func() { _ = syscall.Kill(supervisor, syscall.SIGCONT); _ = drainUpdateCommandSession(supervisor, 0) })
	worker := updateCommandTestPID(t, workerPIDFile)
	child := updateCommandTestPID(t, childPIDFile)
	// Pause only the guardian to make retained-lock verification deterministic.
	if err := syscall.Kill(supervisor, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("killed updater released exclusion before its guardian cleaned up: %v", err)
	}
	if err := syscall.Kill(supervisor, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	updateCommandTestStopped(t, supervisor)
	updateCommandTestStopped(t, worker)
	updateCommandTestStopped(t, child)
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("supervisor leaked inherited lock after cleanup: %v", err)
	}
}

func TestSupervisedUpdatePreservesOutputAndFailure(t *testing.T) {
	lock := updateCommandTestLock(t, filepath.Join(t.TempDir(), "update.lock"))
	base := execCommandRunner{env: updateCommandTestEnvironment("")}
	out, err := runSupervisedUpdateCommandWith(context.Background(), base, []*os.File{lock}, updateCommandTestSupervisor(), "/bin/sh", "-c", "printf 'package output'; printf 'download failed' >&2; exit 7")
	if out != "package output" || err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("supervised failure = %q, %v", out, err)
	}
}

func TestSupervisedUpdateKeepsSuccessfulStderrOutOfStdout(t *testing.T) {
	lock := updateCommandTestLock(t, filepath.Join(t.TempDir(), "update.lock"))
	var warning string
	base := execCommandRunner{env: updateCommandTestEnvironment(""), warn: func(format string, args ...any) { warning = fmt.Sprintf(format, args...) }}
	out, err := runSupervisedUpdateCommandWith(context.Background(), base, []*os.File{lock}, updateCommandTestSupervisor(), "/bin/sh", "-c", "printf 'package output'; printf 'package warning' >&2")
	if err != nil || out != "package output" || !strings.Contains(warning, "package warning") {
		t.Fatalf("stdout = %q, warning = %q, error = %v", out, warning, err)
	}
}

func updateCommandTestSupervisor() []string {
	return []string{os.Args[0], "-test.run=TestUpdateCommandHelperProcess", "--", "supervisor"}
}

func updateCommandTestEnvironment(pidFile string) []string {
	return append(os.Environ(), "REPO_SYNC_UPDATE_COMMAND_TEST=1", "REPO_SYNC_UPDATE_COMMAND_PID_FILE="+pidFile)
}

func updateCommandTestLock(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return file
}

func updateCommandTestPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not report PID at %s", path)
	return 0
}

func updateCommandTestStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := readNativeProcessInfo(pid); processGone(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("update descendant PID %d survived cleanup", pid)
}

func TestUpdateCommandHelperProcess(t *testing.T) {
	if os.Getenv("REPO_SYNC_UPDATE_COMMAND_TEST") != "1" {
		return
	}
	index := 0
	for index < len(os.Args) && os.Args[index] != "--" {
		index++
	}
	args := os.Args[index+1:]
	var err error
	switch args[0] {
	case "supervisor":
		if path := os.Getenv("REPO_SYNC_UPDATE_COMMAND_PID_FILE"); path != "" {
			err = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		if err == nil {
			err = runUpdateCommandSupervisor(args[1:])
		}
	case "worker":
		child := exec.Command(os.Args[0], "-test.run=TestUpdateCommandHelperProcess", "--", "leaf", args[2])
		child.Env = os.Environ()
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err = child.Start(); err == nil {
			err = os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
			if err == nil {
				err = child.Wait()
			}
		}
	case "leaf":
		err = os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
		if err == nil {
			time.Sleep(30 * time.Second)
		}
	case "parent":
		var lock *os.File
		lock, err = os.OpenFile(args[1], os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil {
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		}
		if err == nil {
			base := execCommandRunner{timeout: 20 * time.Second, env: updateCommandTestEnvironment(args[2])}
			_, err = runSupervisedUpdateCommandWith(context.Background(), base, []*os.File{lock}, updateCommandTestSupervisor(), os.Args[0], "-test.run=TestUpdateCommandHelperProcess", "--", "worker", args[3], args[4])
		}
	default:
		err = fmt.Errorf("unknown helper mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
