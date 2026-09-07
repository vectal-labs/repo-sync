package app

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func initRepo(t *testing.T, path string, withOrigin bool) {
	t.Helper()
	gitRun(t, "", "init", "-q", "--initial-branch=main", path)
	if withOrigin {
		gitRun(t, path, "remote", "add", "origin", "https://example.com/acme/"+filepath.Base(path)+".git")
	}
}

func TestDiscoverReposGroupsRanksAndSkips(t *testing.T) {
	home := t.TempDir()
	for _, path := range []string{"code/notes", "code/blog", "code/api", "work/a", "work/b", "alone/solo", "x/y/deep1", "x/y/deep2"} {
		initRepo(t, filepath.Join(home, path), true)
	}
	initRepo(t, filepath.Join(home, "code", "noremote"), false)
	initRepo(t, filepath.Join(home, "Library", "Caches", "cached"), true)
	initRepo(t, filepath.Join(home, ".config", "dotrepo"), true)
	initRepo(t, filepath.Join(home, "code", "app", "node_modules", "dep"), true)
	initRepo(t, filepath.Join(home, "Dropbox", "synced"), true)
	initRepo(t, filepath.Join(home, "a", "b", "c", "d", "toodeep"), true)
	if err := os.MkdirAll(filepath.Join(home, "code", "notes", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, filepath.Join(home, "code", "notes", "nested", "inner"), true)

	groups, singles, err := discoverRepos(context.Background(), execCommandRunner{}, home)
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, group := range groups {
		rel, _ := filepath.Rel(home, group.Parent)
		var names []string
		for _, repo := range group.Repos {
			names = append(names, repo.Name)
		}
		summary = append(summary, rel+":"+strings.Join(names, ","))
	}
	want := []string{"code:api,blog,notes", "work:a,b", "x/y:deep1,deep2", "alone:solo"}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("groups = %v, want %v", summary, want)
	}
	if len(singles) != 0 {
		t.Fatalf("repositories must all appear in groups; singles = %+v", singles)
	}
}

func TestParseSelection(t *testing.T) {
	tests := []struct {
		input string
		want  []int
		bad   bool
	}{
		{"", nil, false},
		{"none", nil, false},
		{"all", []int{1, 2, 3, 4, 5}, false},
		{"1 3 5", []int{1, 3, 5}, false},
		{"2-4, 1", []int{1, 2, 3, 4}, false},
		{"0", nil, true},
		{"6", nil, true},
		{"4-2", nil, true},
		{"x", nil, true},
	}
	for _, test := range tests {
		got, err := parseSelection(test.input, 5)
		if (err != nil) != test.bad || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseSelection(%q) = %v, %v; want %v, bad=%v", test.input, got, err, test.want, test.bad)
		}
	}
}

func TestSelectReposNothingPreselected(t *testing.T) {
	groups := []repoGroup{{Parent: "/home/me/code", Repos: []discoveredRepo{
		{Path: "/home/me/code/a", Name: "a"}, {Path: "/home/me/code/b", Name: "b"}, {Path: "/home/me/code/c", Name: "c"},
	}}}
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "b", Path: "/home/me/code/b", Remote: "origin"}}

	var out strings.Builder
	selected, err := selectRepos(strings.NewReader("\n"), &out, groups, cfg)
	if err != nil || len(selected) != 0 {
		t.Fatalf("pressing Enter must select nothing: %v %v", selected, err)
	}
	if !strings.Contains(out.String(), "b (already synced)") {
		t.Fatalf("already-synced repo not marked:\n%s", out.String())
	}
	out.Reset()
	selected, err = selectRepos(strings.NewReader("9\nall\n"), &out, groups, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, repo := range selected {
		names = append(names, repo.Name)
	}
	if !reflect.DeepEqual(names, []string{"a", "c"}) {
		t.Fatalf("selected = %v, want a and c after an invalid retry", names)
	}
	if !strings.Contains(out.String(), "invalid choice") {
		t.Fatalf("invalid input was not reported:\n%s", out.String())
	}
}

func TestDiscoverAndSelectLoneRepository(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "projects", "solo")
	initRepo(t, path, true)
	groups, _, err := discoverRepos(context.Background(), execCommandRunner{}, home)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader("all\n"), &out, groups, newDefaultConfig())
	if err != nil || len(selected) != 1 || selected[0].Path != path {
		t.Fatalf("lone repository must be selectable: selected=%v error=%v output=\n%s", selected, err, out.String())
	}
}

func TestSelectReposAddsManualPathsAlongsideDiscovered(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	listed := filepath.Join(home, "projects", "listed")
	manual := filepath.Join(home, ".hidden", "my notes")
	second := filepath.Join(home, ".hidden", "second")
	initRepo(t, listed, true)
	initRepo(t, manual, true)
	initRepo(t, second, true)
	groups, _, err := discoverRepos(context.Background(), execCommandRunner{}, home)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader(manual+"\n"+second+"\n1 2-3\n"), &out, groups, newDefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := []discoveredRepo{
		{Path: listed, Name: "listed"},
		{Path: canonicalRepoPath(manual), Name: "my notes"},
		{Path: canonicalRepoPath(second), Name: "second"},
	}
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("selected = %v, want %v; output=\n%s", selected, want, out.String())
	}
	if !strings.Contains(out.String(), "Nothing selected yet.") {
		t.Fatalf("manual path must explain the final selection step:\n%s", out.String())
	}
}

func TestSelectReposManualPathDoesNotPreselect(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	initRepo(t, path, true)
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader(path+"\n\n"), &out, nil, newDefaultConfig())
	if err != nil || len(selected) != 0 {
		t.Fatalf("pressing Enter after adding a path must select nothing: %v %v", selected, err)
	}
}

func TestSelectReposManualHomePathWithSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "my projects", "notes")
	initRepo(t, path, true)
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader("~/my projects/notes\nall\n"), &out, nil, newDefaultConfig())
	if err != nil || len(selected) != 1 || selected[0].Path != canonicalRepoPath(path) {
		t.Fatalf("home path must be selectable: %v %v; output=\n%s", selected, err, out.String())
	}
}

func TestSelectReposRetriesInvalidManualPaths(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	missing := filepath.Join(home, "missing")
	noOrigin := filepath.Join(home, "no-origin")
	valid := filepath.Join(home, "valid")
	initRepo(t, noOrigin, false)
	initRepo(t, valid, true)
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader(missing+"\n"+noOrigin+"\n"+valid+"\n1\n"), &out, nil, newDefaultConfig())
	if err != nil || len(selected) != 1 || selected[0].Path != canonicalRepoPath(valid) {
		t.Fatalf("valid retry must be selected: %v %v; output=\n%s", selected, err, out.String())
	}
	for _, want := range []string{"is not inside a Git repository", "has no origin remote"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing error %q in output:\n%s", want, out.String())
		}
	}
}

func TestSelectReposManualPathDeduplicatesRootsAndAliases(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "repo")
	initRepo(t, path, true)
	subdir := filepath.Join(path, "docs")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	groups := []repoGroup{{Parent: home, Repos: []discoveredRepo{{Path: path, Name: "repo"}}}}
	var out strings.Builder
	selected, err := selectRepos(strings.NewReader(subdir+"\n"+alias+"\nall\n"), &out, groups, newDefaultConfig())
	if err != nil || len(selected) != 1 || selected[0].Path != path {
		t.Fatalf("subfolders and aliases must not add duplicates: %v %v; output=\n%s", selected, err, out.String())
	}
	if strings.Count(out.String(), "already listed as 1") != 2 {
		t.Fatalf("duplicate paths must be explained:\n%s", out.String())
	}

	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "repo", Path: alias, Remote: "origin"}}
	out.Reset()
	selected, err = selectRepos(strings.NewReader(path+"\nnone\n"), &out, groups, cfg)
	if err != nil || len(selected) != 0 {
		t.Fatalf("already-synced alias must not be selected again: %v %v", selected, err)
	}
	if !strings.Contains(out.String(), "repo (already synced)") || !strings.Contains(out.String(), "is already synced. Choose another repository.") {
		t.Fatalf("already-synced paths must be explained:\n%s", out.String())
	}
}

func TestSelectReposLeavesSharedInputForLaterPrompts(t *testing.T) {
	t.Parallel()
	in := bufio.NewReader(strings.NewReader("\nlogin-answer\n"))
	var out strings.Builder
	if _, err := selectRepos(in, &out, nil, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	remaining, err := in.ReadString('\n')
	if err != nil || remaining != "login-answer\n" {
		t.Fatalf("selection consumed the next prompt's input: %q %v", remaining, err)
	}
}
