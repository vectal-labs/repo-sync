package main

import (
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
	want := []string{"code:api,blog,notes", "work:a,b", "x/y:deep1,deep2"}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("groups = %v, want %v", summary, want)
	}
	if len(singles) != 1 || singles[0].Name != "solo" {
		t.Fatalf("singles = %+v, want only solo", singles)
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
