package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const updateCommandStopTimeout = 5 * time.Second

// The supervisor keeps inherited flock descriptions open until the package
// command and its descendants stop, even when the updater itself is killed.
func runSupervisedUpdateCommand(ctx context.Context, base execCommandRunner, lockFiles []*os.File, program string, args ...string) (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return runSupervisedUpdateCommandWith(ctx, base, lockFiles, []string{binary, "update-command-supervisor"}, program, args...)
}

func runSupervisedUpdateCommandWith(ctx context.Context, base execCommandRunner, lockFiles []*os.File, supervisor []string, program string, args ...string) (string, error) {
	if len(lockFiles) == 0 || len(supervisor) == 0 || !filepath.IsAbs(program) {
		return "", fmt.Errorf("supervised update commands require held locks and an absolute program path")
	}
	timeout := base.timeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	liveness, alive, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer liveness.Close()
	defer alive.Close()
	arguments := append(append([]string(nil), supervisor[1:]...), strconv.Itoa(len(lockFiles)), timeout.String(), program)
	arguments = append(arguments, args...)
	cmd := base.command(commandCtx, supervisor[0], arguments...)
	// Homebrew creates further process groups. A private session contains all
	// of them, including children orphaned when an intermediate process exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = append(append([]*os.File(nil), lockFiles...), liveness)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSL_VERSION=")
	var stdout, stderr bytes.Buffer
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer outRead.Close()
	defer outWrite.Close()
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer errRead.Close()
	defer errWrite.Close()
	cmd.Stdout, cmd.Stderr = outWrite, errWrite
	// The supervisor handles SIGTERM while retaining the locks. The fallback
	// drains its whole session, never just the process that holds those locks.
	cancelDone := make(chan struct{})
	var cancelWorker sync.WaitGroup
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		cancelWorker.Add(1)
		go func() {
			defer cancelWorker.Done()
			timer := time.NewTimer(updateCommandStopTimeout)
			defer timer.Stop()
			select {
			case <-cancelDone:
			case <-timer.C:
				if drainUpdateCommandSession(cmd.Process.Pid, cmd.Process.Pid) == nil {
					_ = cmd.Process.Kill()
				}
			}
		}()
		return err
	}
	if err := cmd.Start(); err != nil {
		close(cancelDone)
		return "", err
	}
	liveness.Close()
	outWrite.Close()
	errWrite.Close()
	copied := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&stdout, outRead); copied <- struct{}{} }()
	go func() { _, _ = io.Copy(&stderr, errRead); copied <- struct{}{} }()
	err = cmd.Wait()
	close(cancelDone)
	cancelWorker.Wait()
	// Also clean up if the supervisor crashed independently of the updater.
	waitForUpdateCommandSession(cmd.Process.Pid, 0)
	<-copied
	<-copied
	if commandCtx.Err() != nil {
		return stdout.String(), fmt.Errorf("%s interrupted: %w", program, commandCtx.Err())
	}
	if err != nil {
		diagnostics := redactCredentials(strings.TrimSpace(stderr.String() + "\n" + stdout.String()))
		return stdout.String(), fmt.Errorf("%s: %w: %s", commandLine(program, args), err, diagnostics)
	}
	if warning := strings.TrimSpace(stderr.String()); warning != "" && base.warn != nil {
		base.warn("%s: %s", commandLine(program, args), redactCredentials(warning))
	}
	return stdout.String(), nil
}

// Private CLI: update-command-supervisor <lock-count> <timeout> <program> [args...].
// Inherited descriptors 3.. hold the locks; the next descriptor watches the
// updater's lifetime. The private session includes Homebrew's process groups.
func runUpdateCommandSupervisor(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("invalid update command supervisor arguments")
	}
	lockCount, err := strconv.Atoi(args[0])
	if err != nil || lockCount < 1 || lockCount > 16 {
		return fmt.Errorf("invalid update command lock count")
	}
	timeout, err := time.ParseDuration(args[1])
	if err != nil || timeout <= 0 || !filepath.IsAbs(args[2]) {
		return fmt.Errorf("invalid update command timeout or program")
	}
	if session, err := syscall.Getsid(0); err != nil || session != os.Getpid() {
		return fmt.Errorf("update command supervisor must own its session")
	}
	for index := 0; index < lockCount; index++ {
		syscall.CloseOnExec(3 + index)
		lock := os.NewFile(uintptr(3+index), "update-command-lock")
		if _, err := lock.Stat(); err != nil {
			return fmt.Errorf("read inherited update lock: %w", err)
		}
		defer lock.Close()
	}
	syscall.CloseOnExec(3 + lockCount)
	liveness := os.NewFile(uintptr(3+lockCount), "updater-lifetime")
	defer liveness.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, liveness)
		cancel()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var commandErr error
	finished := false
	select {
	case commandErr = <-done:
		finished = true
	case <-ctx.Done():
		commandErr = fmt.Errorf("package operation interrupted: %w", ctx.Err())
	}
	waitForUpdateCommandSession(os.Getpid(), os.Getpid())
	if !finished {
		<-done
	}
	return commandErr
}

func waitForUpdateCommandSession(session, keepPID int) {
	reported := false
	for {
		err := drainUpdateCommandSession(session, keepPID)
		if err == nil {
			return
		}
		if !reported {
			fmt.Fprintln(os.Stderr, "Waiting for package processes to stop; update locks remain held: "+redactCredentials(err.Error()))
			reported = true
		}
		// An inspection failure must not release exclusion while a package
		// process might still be writing. Retry with the locks still held.
		time.Sleep(100 * time.Millisecond)
	}
}

func drainUpdateCommandSession(session, keepPID int) error {
	deadline := time.Now().Add(updateCommandStopTimeout)
	for {
		members, err := updateCommandSessionMembers(session, keepPID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		for _, process := range members {
			current, err := readNativeProcessInfo(process.PID)
			if processGone(err) {
				continue
			}
			if err != nil {
				return err
			}
			sessionNow, err := syscall.Getsid(process.PID)
			if processGone(err) {
				continue
			}
			if err != nil {
				return err
			}
			if sessionNow != session || current.StartedAt != process.StartedAt || current.UID != process.UID {
				continue
			}
			if err := syscall.Kill(process.PID, syscall.SIGKILL); err != nil && !processGone(err) {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("update command descendants did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func updateCommandSessionMembers(session, keepPID int) ([]ownedProcess, error) {
	pids, err := (nativeProcessInspector{}).pids()
	if err != nil {
		return nil, err
	}
	var members []ownedProcess
	for _, pid := range pids {
		if pid == keepPID {
			continue
		}
		candidateSession, err := syscall.Getsid(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if candidateSession != session {
			continue
		}
		process, err := readNativeProcessInfo(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if process.UID == uint32(os.Getuid()) {
			members = append(members, process)
		}
	}
	return members, nil
}
