package app

/*
#cgo LDFLAGS: -lproc
#include <libproc.h>
#include <sys/proc.h>
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
	"unsafe"
)

// StartedAt is the kernel's process start time in Unix microseconds. A PID alone
// is not an identity: macOS may reuse it while uninstall is waiting for exit.
type ownedProcess struct {
	PID        int
	UID        uint32
	Executable string
	StartedAt  uint64
}

type processInspector interface {
	pids() ([]int, error)
	inspect(int) (ownedProcess, error)
	signal(int, syscall.Signal) error
}

type nativeProcessInspector struct{}

func (nativeProcessInspector) pids() ([]int, error) {
	// The SDK specifies byte counts for proc_listpids. Leave room for processes
	// which start between sizing and reading; retry if the buffer is filled.
	size, err := C.proc_listpids(C.PROC_RUID_ONLY, C.uint32_t(os.Getuid()), nil, 0)
	if size <= 0 {
		return nil, nativeProcessError("list processes", err)
	}
	capacity := int(size)/int(C.sizeof_int) + 128
	for range 4 {
		buffer := make([]C.int, capacity)
		n, err := C.proc_listpids(C.PROC_RUID_ONLY, C.uint32_t(os.Getuid()), unsafe.Pointer(&buffer[0]), C.int(len(buffer)*int(C.sizeof_int)))
		if n <= 0 {
			return nil, nativeProcessError("list processes", err)
		}
		if int(n) >= len(buffer)*int(C.sizeof_int) {
			capacity *= 2
			continue
		}
		pids := make([]int, 0, int(n)/int(C.sizeof_int))
		for _, pid := range buffer[:int(n)/int(C.sizeof_int)] {
			if pid > 0 {
				pids = append(pids, int(pid))
			}
		}
		return pids, nil
	}
	return nil, fmt.Errorf("process list kept growing; retry uninstall")
}

func nativeProcessError(operation string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		err = syscall.EIO
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func readNativeProcessInfo(pid int) (ownedProcess, error) {
	var info C.struct_proc_bsdinfo
	n, err := C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&info), C.int(C.sizeof_struct_proc_bsdinfo))
	if n != C.int(C.sizeof_struct_proc_bsdinfo) {
		return ownedProcess{}, nativeProcessError(fmt.Sprintf("inspect PID %d", pid), err)
	}
	if info.pbi_status == C.SZOMB || int(info.pbi_pid) != pid {
		return ownedProcess{}, os.ErrProcessDone
	}
	return ownedProcess{
		PID: pid, UID: uint32(info.pbi_ruid),
		StartedAt: uint64(info.pbi_start_tvsec)*1_000_000 + uint64(info.pbi_start_tvusec),
	}, nil
}

func (nativeProcessInspector) inspect(pid int) (ownedProcess, error) {
	process, err := readNativeProcessInfo(pid)
	if err != nil || process.UID != uint32(os.Getuid()) {
		return process, err
	}
	path := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path)))
	if n <= 0 {
		return process, nativeProcessError(fmt.Sprintf("read executable for PID %d", pid), err)
	}
	end := bytes.IndexByte(path, 0)
	if end <= 0 {
		return process, fmt.Errorf("missing executable path for PID %d", pid)
	}
	process.Executable = string(path[:end])
	if canonical, err := filepath.EvalSymlinks(process.Executable); err == nil {
		process.Executable = canonical
	} else if !errors.Is(err, os.ErrNotExist) {
		return process, fmt.Errorf("resolve executable for PID %d: %w", pid, err)
	}
	// Identity and path are separate kernel reads. Reject a PID reused in between.
	after, err := readNativeProcessInfo(pid)
	if err != nil {
		return process, err
	}
	if after.UID != process.UID || after.StartedAt != process.StartedAt {
		return process, os.ErrProcessDone
	}
	return process, nil
}

func (nativeProcessInspector) signal(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

func discoverRepoSyncProcesses(ctx context.Context, allowedExecutables []string) ([]ownedProcess, error) {
	return discoverRepoSyncProcessesWith(ctx, allowedExecutables, nativeProcessInspector{}, isRepoSyncBinary)
}

func discoverRepoSyncProcessesWith(ctx context.Context, allowedExecutables []string, inspector processInspector, verifyBinary func(string) bool) ([]ownedProcess, error) {
	allowed := make(map[string]bool)
	for _, path := range allowedExecutables {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("resolve executable %s: %w", absolute, err)
			}
			resolved = absolute
		}
		allowed[resolved] = true
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pids, err := inspector.pids()
	if err != nil {
		return nil, err
	}
	var processes []ownedProcess
	seen := make(map[int]bool)
	for _, pid := range pids {
		if pid <= 1 || pid == os.Getpid() || seen[pid] {
			continue
		}
		seen[pid] = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		process, err := inspector.inspect(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			if allowed[process.Executable] {
				return nil, fmt.Errorf("cannot verify process %d before uninstall: %w", pid, err)
			}
			// macOS may hide unrelated processes even from their owning user. An
			// unreadable process with no known executable cannot be identified safely.
			continue
		}
		if process.UID != uint32(os.Getuid()) {
			continue
		}
		if !allowed[process.Executable] {
			continue
		}
		if !verifyBinary(process.Executable) {
			return nil, fmt.Errorf("cannot verify running process %d from %s; stop PID %d manually and retry uninstall", process.PID, process.Executable, process.PID)
		}
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func processGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

func stopRepoSyncProcesses(ctx context.Context, processes []ownedProcess, timeout time.Duration) error {
	return stopRepoSyncProcessesWith(ctx, processes, timeout, nativeProcessInspector{})
}

func stopRepoSyncProcessesWith(ctx context.Context, processes []ownedProcess, timeout time.Duration, inspector processInspector) error {
	if timeout <= 0 {
		return fmt.Errorf("process shutdown timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, min(timeout, 45*time.Second))
	defer cancel()
	for _, process := range processes {
		if process.PID <= 1 || process.PID == os.Getpid() || process.UID != uint32(os.Getuid()) || process.StartedAt == 0 || process.Executable == "" {
			return fmt.Errorf("refusing to stop unverified process %d", process.PID)
		}
	}
	var pending []ownedProcess
	seen := make(map[int]bool)
	for _, process := range processes {
		if seen[process.PID] {
			continue
		}
		seen[process.PID] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := inspector.inspect(process.PID)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("verify PID %d before stopping: %w", process.PID, err)
		}
		if current != process {
			continue
		}
		if err := inspector.signal(process.PID, syscall.SIGTERM); err != nil {
			if processGone(err) {
				continue
			}
			return fmt.Errorf("stop PID %d: %w", process.PID, err)
		}
		pending = append(pending, process)
	}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("repo-sync processes have not stopped; files must be kept: %w", err)
		}
		remaining := pending[:0]
		for _, process := range pending {
			current, err := inspector.inspect(process.PID)
			if processGone(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("verify PID %d exited: %w", process.PID, err)
			}
			if current == process {
				remaining = append(remaining, process)
			}
		}
		pending = remaining
		if len(pending) > 0 {
			if err := waitContext(ctx, 50*time.Millisecond); err != nil {
				return fmt.Errorf("repo-sync processes have not stopped; files must be kept: %w", err)
			}
		}
	}
	return nil
}
