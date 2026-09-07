//go:build darwin

package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeProcessInspector struct {
	list       []int
	processes  map[int]ownedProcess
	inspectErr map[int]error
	listErr    error
	signalErr  error
	signals    []int
	onSignal   func(int)
}

func (f *fakeProcessInspector) pids() ([]int, error) { return f.list, f.listErr }
func (f *fakeProcessInspector) inspect(pid int) (ownedProcess, error) {
	if err := f.inspectErr[pid]; err != nil {
		return f.processes[pid], err
	}
	if process, ok := f.processes[pid]; ok {
		return process, nil
	}
	return ownedProcess{}, syscall.ESRCH
}
func (f *fakeProcessInspector) signal(pid int, signal syscall.Signal) error {
	if signal != syscall.SIGTERM {
		panic("shutdown must never send a signal other than SIGTERM")
	}
	f.signals = append(f.signals, pid)
	if f.onSignal != nil {
		f.onSignal(pid)
	}
	return f.signalErr
}

func testOwnedProcess(pid int, executable string) ownedProcess {
	return ownedProcess{PID: pid, UID: uint32(os.Getuid()), Executable: executable, StartedAt: 1_700_000_000_000_000}
}

func TestDiscoverProcessesRequiresAllowedPathAndBinaryIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	approved := filepath.Join(root, "repo-sync")
	alias := filepath.Join(root, "repo-sync-link")
	if err := os.WriteFile(approved, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(approved, alias); err != nil {
		t.Fatal(err)
	}
	lookalike := filepath.Join(root, "other", "repo-sync")
	fake := &fakeProcessInspector{
		list:       []int{101, 102, 103, 104, os.Getpid(), 101, 999},
		inspectErr: map[int]error{104: syscall.EPERM},
		processes: map[int]ownedProcess{
			101:         testOwnedProcess(101, approved),
			102:         testOwnedProcess(102, lookalike),
			103:         {PID: 103, UID: uint32(os.Getuid()) + 1, Executable: approved, StartedAt: 1},
			os.Getpid(): testOwnedProcess(os.Getpid(), approved),
		},
	}
	got, err := discoverRepoSyncProcessesWith(context.Background(), []string{alias}, fake, func(path string) bool { return path == approved })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ownedProcess{fake.processes[101]}) {
		t.Fatalf("only a verified allowed executable owned by this user may match: %+v", got)
	}
}

func TestDiscoverProcessesRejectsMissingOrReplacedRecordedBinary(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "missing"
		if replaced {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(root, "repo-sync")
			if replaced {
				if err := os.WriteFile(executable, []byte("unrelated replacement"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			process := testOwnedProcess(101, executable)
			fake := &fakeProcessInspector{list: []int{101}, processes: map[int]ownedProcess{101: process}}
			got, err := discoverRepoSyncProcessesWith(context.Background(), []string{executable}, fake, isRepoSyncBinary)
			if err == nil || !strings.Contains(err.Error(), "stop PID 101 manually and retry uninstall") || len(got) != 0 {
				t.Fatalf("%s running executable must block cleanup with an actionable error: %+v, %v", name, got, err)
			}
			if len(fake.signals) != 0 {
				t.Fatal("an unverified process must never be signaled")
			}
		})
	}
}

func TestDiscoverProcessesFailsClosedOnInspectionFailure(t *testing.T) {
	process := testOwnedProcess(101, "/known/repo-sync")
	fake := &fakeProcessInspector{list: []int{101}, processes: map[int]ownedProcess{101: process}, inspectErr: map[int]error{101: syscall.EACCES}}
	_, err := discoverRepoSyncProcessesWith(context.Background(), []string{process.Executable}, fake, func(string) bool { return true })
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("unreadable targeted processes must not be silently treated as stopped: %v", err)
	}
	fake.listErr = syscall.EPERM
	_, err = discoverRepoSyncProcessesWith(context.Background(), []string{process.Executable}, fake, func(string) bool { return true })
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("process enumeration failure must be reported: %v", err)
	}
}

func TestStopProcessesNeverSignalsReusedPIDOrChangedIdentity(t *testing.T) {
	original := testOwnedProcess(101, "/known/repo-sync")
	for _, field := range []string{"start", "uid", "executable"} {
		t.Run(field, func(t *testing.T) {
			changed := original
			switch field {
			case "start":
				changed.StartedAt++
			case "uid":
				changed.UID++
			case "executable":
				changed.Executable = "/bin/sleep"
			}
			fake := &fakeProcessInspector{processes: map[int]ownedProcess{101: changed}}
			if err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{original}, time.Second, fake); err != nil {
				t.Fatal(err)
			}
			if len(fake.signals) != 0 {
				t.Fatalf("PID with changed %s was signaled: %v", field, fake.signals)
			}
		})
	}
}

func TestStopProcessesSignalsAllThenUsesOneBoundedWait(t *testing.T) {
	first, second := testOwnedProcess(101, "/known/repo-sync"), testOwnedProcess(102, "/known/repo-sync")
	fake := &fakeProcessInspector{processes: map[int]ownedProcess{101: first, 102: second}}
	started := time.Now()
	err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{first, second}, 30*time.Millisecond, fake)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stubborn processes must time out without force-killing: %v", err)
	}
	if !reflect.DeepEqual(fake.signals, []int{101, 102}) {
		t.Fatalf("every verified process should get SIGTERM before waiting: %v", fake.signals)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown ignored its deadline")
	}
}

func TestStopProcessesTreatsPIDReuseAfterSignalAsExit(t *testing.T) {
	original := testOwnedProcess(101, "/known/repo-sync")
	fake := &fakeProcessInspector{processes: map[int]ownedProcess{101: original}}
	fake.onSignal = func(pid int) {
		changed := original
		changed.StartedAt++
		fake.processes[pid] = changed
	}
	if err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{original}, time.Second, fake); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.signals, []int{101}) {
		t.Fatalf("replacement process was signaled: %v", fake.signals)
	}
}

func TestStopProcessesRejectsUnsafeTargetsBeforeAnySignal(t *testing.T) {
	valid := testOwnedProcess(101, "/known/repo-sync")
	for _, invalid := range []ownedProcess{
		testOwnedProcess(os.Getpid(), "/known/repo-sync"),
		{PID: 102, UID: uint32(os.Getuid()) + 1, StartedAt: 1, Executable: "/known/repo-sync"},
		{PID: 102, UID: uint32(os.Getuid()), Executable: "/known/repo-sync"},
	} {
		fake := &fakeProcessInspector{processes: map[int]ownedProcess{101: valid}}
		if err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{valid, invalid}, time.Second, fake); err == nil {
			t.Fatal("unsafe shutdown target was accepted")
		}
		if len(fake.signals) != 0 {
			t.Fatalf("validation must finish before any signal: %v", fake.signals)
		}
	}
}

func TestStopProcessesHonorsCancellationAndInspectionFailure(t *testing.T) {
	process := testOwnedProcess(101, "/known/repo-sync")
	fake := &fakeProcessInspector{processes: map[int]ownedProcess{101: process}, inspectErr: map[int]error{101: syscall.EACCES}}
	if err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{process}, time.Second, fake); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("inspection denial must stop cleanup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stopRepoSyncProcessesWith(ctx, []ownedProcess{process}, time.Second, fake); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled shutdown must return promptly: %v", err)
	}
	if len(fake.signals) != 0 {
		t.Fatalf("an unverified or cancelled target was signaled: %v", fake.signals)
	}
}

func TestNativeProcessIdentityAndGracefulShutdown(t *testing.T) {
	// This is our own harmless child, never the real repo-sync service.
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	inspector := nativeProcessInspector{}
	process, err := inspector.inspect(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if process.PID != child.Process.Pid || process.UID != uint32(os.Getuid()) || process.StartedAt == 0 || process.Executable != "/bin/sleep" {
		t.Fatalf("unexpected kernel process identity: %+v", process)
	}
	second, err := inspector.inspect(child.Process.Pid)
	if err != nil || second != process {
		t.Fatalf("identity changed while the process was running: %+v, %v", second, err)
	}
	pids, err := inspector.pids()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pid := range pids {
		found = found || pid == child.Process.Pid
	}
	if !found {
		t.Fatal("native process enumeration missed our child")
	}
	if isRepoSyncBinary(process.Executable) {
		t.Fatal("native sleep binary was identified as repo-sync")
	}
	if err := stopRepoSyncProcessesWith(context.Background(), []ownedProcess{process}, 3*time.Second, inspector); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.inspect(child.Process.Pid); !processGone(err) {
		t.Fatalf("child should have exited after SIGTERM: %v", err)
	}
}
