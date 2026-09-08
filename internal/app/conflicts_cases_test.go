package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type expectedConflictSnapshot struct {
	base, upstream, local *string
	mode                  string
}

func TestConflictCasesPreserveVersionsAndAbort(t *testing.T) {
	t.Parallel()
	content := func(value string) *string { return &value }
	base, localText, upstreamText := content("original\n"), content("local edit\n"), content("remote edit\n")
	cases := []struct {
		name, path                  string
		base, local, upstream       *string
		localRename, upstreamRename string
		executable                  bool
	}{
		{name: "edit_edit", path: "shared.txt", base: base, local: localText, upstream: upstreamText},
		{name: "add_add", path: "new.txt", local: localText, upstream: upstreamText},
		{name: "local_delete_remote_modify", path: "shared.txt", base: base, upstream: upstreamText},
		{name: "local_modify_remote_delete", path: "shared.txt", base: base, local: localText},
		{name: "binary", path: "drawing.bin", base: content("\x00\xfforiginal\n"), local: content("\x00\xfelocal\n"), upstream: content("\x00\xfdremote\n")},
		{name: "empty_local_remote_delete", path: "shared.txt", base: base, local: content("")},
		{name: "empty_remote_local_delete", path: "shared.txt", base: base, upstream: content("")},
		{name: "spaces", path: "team notes.txt", base: base, local: localText, upstream: upstreamText},
		{name: "tab", path: "team\tnotes.txt", base: base, local: localText, upstream: upstreamText},
		{name: "newline", path: "team\nnotes.txt", base: base, local: localText, upstream: upstreamText},
		{name: "unicode", path: "会议笔记-🚀.txt", base: base, local: localText, upstream: upstreamText},
		{name: "pathspec_characters", path: "[team]*?.txt", base: base, local: localText, upstream: upstreamText},
		{name: "local_rename_remote_delete", path: "old.txt", base: base, local: base, localRename: "local-name.txt"},
		{name: "remote_rename_local_delete", path: "old.txt", base: base, upstream: base, upstreamRename: "remote-name.txt"},
		{name: "rename_rename", path: "old.txt", base: base, local: base, upstream: base, localRename: "local-name.txt", upstreamRename: "remote-name.txt"},
		{name: "executable", path: "script.sh", base: base, local: localText, upstream: upstreamText, executable: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remote, local := makeGitFixture(t)
			if test.base != nil {
				if err := os.WriteFile(filepath.Join(local, test.path), []byte(*test.base), 0o644); err != nil {
					t.Fatal(err)
				}
				if test.executable {
					if err := os.Chmod(filepath.Join(local, test.path), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				gitRun(t, local, "--literal-pathspecs", "add", "--", test.path)
				gitRun(t, local, "commit", "-qm", "base document")
				gitRun(t, local, "push", "-q", "origin", "main")
			}
			other := filepath.Join(t.TempDir(), "other")
			gitRun(t, "", "clone", "-q", remote, other)
			configureGitUser(t, other)
			changeDocument := func(repo string, value *string, renamed string) {
				t.Helper()
				if value == nil {
					gitRun(t, repo, "--literal-pathspecs", "rm", "--", test.path)
				} else {
					name := test.path
					if renamed != "" {
						gitRun(t, repo, "--literal-pathspecs", "mv", "--", name, renamed)
						name = renamed
					}
					if err := os.WriteFile(filepath.Join(repo, name), []byte(*value), 0o644); err != nil {
						t.Fatal(err)
					}
					gitRun(t, repo, "--literal-pathspecs", "add", "--", name)
				}
				gitRun(t, repo, "commit", "-qm", "change document")
			}
			changeDocument(local, test.local, test.localRename)
			changeDocument(other, test.upstream, test.upstreamRename)
			gitRun(t, other, "push", "-q", "origin", "main")
			localHead := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
			remoteHead := strings.TrimSpace(gitOutput(t, other, "rev-parse", "HEAD"))
			mode := "100644"
			if test.executable {
				mode = "100755"
			}
			expected := map[string]expectedConflictSnapshot{}
			switch {
			case test.localRename != "" && test.upstreamRename != "":
				expected[test.path] = expectedConflictSnapshot{base: test.base, mode: mode}
				expected[test.localRename] = expectedConflictSnapshot{local: test.local, mode: mode}
				expected[test.upstreamRename] = expectedConflictSnapshot{upstream: test.upstream, mode: mode}
			case test.localRename != "":
				expected[test.localRename] = expectedConflictSnapshot{base: test.base, local: test.local, mode: mode}
			case test.upstreamRename != "":
				expected[test.upstreamRename] = expectedConflictSnapshot{base: test.base, upstream: test.upstream, mode: mode}
			default:
				expected[test.path] = expectedConflictSnapshot{base: test.base, upstream: test.upstream, local: test.local, mode: mode}
			}
			_, err := (gitSyncer{runner: execCommandRunner{}}).sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
			var conflict *conflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("sync error = %v; want conflict", err)
			}
			if conflict.LocalHead != localHead || conflict.RemoteHead != remoteHead || conflict.ReplayedHead != localHead || conflict.RemoteRef != "refs/remotes/origin/main" || conflict.Branch != "main" || conflict.AbortError != "" {
				t.Fatalf("conflict metadata = %+v; local=%s remote=%s", conflict, localHead, remoteHead)
			}
			assertConflictSnapshots(t, local, conflict, expected)
			assertConflictAbortRestored(t, local, remote, localHead, remoteHead)
		})
	}
}

func assertConflictSnapshots(t *testing.T, repo string, conflict *conflictError, expected map[string]expectedConflictSnapshot) {
	t.Helper()
	if len(conflict.Files) != len(expected) {
		t.Fatalf("conflicting files = %+v; expected paths = %v", conflict.Files, expected)
	}
	seen := map[string]bool{}
	for _, file := range conflict.Files {
		want, ok := expected[file.Path]
		if !ok || seen[file.Path] {
			t.Fatalf("unexpected or duplicate conflict path %q", file.Path)
		}
		seen[file.Path] = true
		for _, stage := range []struct {
			name string
			got  conflictVersion
			want *string
		}{{"base", file.Base, want.base}, {"upstream", file.Upstream, want.upstream}, {"local", file.Local, want.local}} {
			if stage.want == nil {
				if !reflect.DeepEqual(stage.got, conflictVersion{}) {
					t.Errorf("%q %s should be absent: %+v", file.Path, stage.name, stage.got)
				}
				continue
			}
			if stage.got.Object == "" || stage.got.Mode != want.mode || stage.got.Omitted != "" || string(stage.got.Data) != *stage.want {
				t.Errorf("%q %s = %+v; want mode=%s data=%q", file.Path, stage.name, stage.got, want.mode, *stage.want)
				continue
			}
			if objectData := gitOutput(t, repo, "cat-file", "blob", stage.got.Object); objectData != *stage.want {
				t.Errorf("%q %s object data = %q; want %q", file.Path, stage.name, objectData, *stage.want)
			}
		}
	}
}

func assertConflictAbortRestored(t *testing.T, local, remote, localHead, remoteHead string) {
	t.Helper()
	if got := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD")); got != localHead {
		t.Errorf("local HEAD changed: got %s; want %s", got, localHead)
	}
	if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteHead {
		t.Errorf("remote HEAD changed: got %s; want %s", got, remoteHead)
	}
	if got := gitOutput(t, local, "status", "--porcelain=v1", "-z"); got != "" {
		t.Errorf("abort left a dirty worktree/index: %q", got)
	}
	if got := gitOutput(t, local, "ls-files", "--unmerged", "-z"); got != "" {
		t.Errorf("abort left unresolved index entries: %q", got)
	}
	if busy, operation, err := (gitSyncer{runner: execCommandRunner{}}).inProgress(context.Background(), local); err != nil || busy {
		t.Errorf("operation after abort: busy=%v operation=%s err=%v", busy, operation, err)
	}
}

func TestConflictCasesMultiCommitRebaseStages(t *testing.T) {
	t.Parallel()
	remote, local := makeGitFixture(t)
	base := "first original\n1\n2\n3\n4\n5\nlast original\n"
	writeAndCommit(t, local, "shared.txt", base, "base")
	gitRun(t, local, "push", "-q", "origin", "main")
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "-q", remote, other)
	configureGitUser(t, other)
	first := strings.Replace(base, "first original", "first local", 1)
	writeAndCommit(t, local, "shared.txt", first, "first local commit")
	second := strings.Replace(first, "last original", "last local", 1)
	writeAndCommit(t, local, "shared.txt", second, "second local commit conflicts")
	replayed := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	third := strings.Replace(second, "first local", "first final", 1)
	writeAndCommit(t, local, "shared.txt", third, "third local commit not reached")
	upstream := strings.Replace(base, "last original", "last remote", 1)
	writeAndCommit(t, other, "shared.txt", upstream, "remote commit")
	gitRun(t, other, "push", "-q", "origin", "main")
	localHead := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(gitOutput(t, other, "rev-parse", "HEAD"))
	_, err := (gitSyncer{runner: execCommandRunner{}}).sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("sync error = %v; want conflict", err)
	}
	if conflict.ReplayedHead != replayed || conflict.LocalHead != localHead || conflict.RemoteHead != remoteHead {
		t.Fatalf("wrong replay position: %+v; want replayed=%s local=%s remote=%s", conflict, replayed, localHead, remoteHead)
	}
	// The upstream stage includes the first replayed local commit. Neither
	// side equals the corresponding branch tip at this intermediate conflict.
	mergedUpstream := strings.Replace(upstream, "first original", "first local", 1)
	assertConflictSnapshots(t, local, conflict, map[string]expectedConflictSnapshot{
		"shared.txt": {base: &first, upstream: &mergedUpstream, local: &second, mode: "100644"},
	})
	assertConflictAbortRestored(t, local, remote, localHead, remoteHead)
	if got, err := os.ReadFile(filepath.Join(local, "shared.txt")); err != nil || string(got) != third {
		t.Fatalf("final local version after abort = %q, %v; want %q", got, err, third)
	}
}

type conflictAbortFailRunner struct{ inner commandRunner }

func (r conflictAbortFailRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	git := gitArgs(name, args)
	if slices.Contains(git, "rebase") && slices.Contains(git, "--abort") {
		return "", errors.New("simulated abort failure")
	}
	return r.inner.run(ctx, dir, stdin, name, args...)
}

func TestConflictCasesAbortFailurePreservesManualRepair(t *testing.T) {
	t.Parallel()
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "shared.txt", "local\n", "local work")
	pushFromTeammate(t, remote, "shared.txt", "remote\n")
	localHead := strings.TrimSpace(gitOutput(t, local, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))
	syncer := gitSyncer{runner: conflictAbortFailRunner{inner: execCommandRunner{}}}
	_, err := syncer.sync(context.Background(), repoConfig{Name: "notes", Path: local, Remote: "origin"}, false)
	var conflict *conflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.AbortError, "simulated abort failure") {
		t.Fatalf("sync error = %v; want conflict with abort failure", err)
	}
	if strings.Contains(conflict.Error(), "rebase aborted") {
		t.Fatalf("error claimed abort succeeded: %v", conflict)
	}
	if got := gitOutput(t, local, "ls-files", "--unmerged", "-z"); got == "" {
		t.Fatal("abort failure discarded the unresolved index needed for manual repair")
	}
	if got := gitOutput(t, local, "show", ":2:shared.txt"); got != "remote\n" {
		t.Errorf("upstream stage after abort failure = %q", got)
	}
	if got := gitOutput(t, local, "show", ":3:shared.txt"); got != "local\n" {
		t.Errorf("local stage after abort failure = %q", got)
	}
	gitRun(t, local, "rebase", "--abort")
	assertConflictAbortRestored(t, local, remote, localHead, remoteHead)
}
