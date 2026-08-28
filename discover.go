package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const discoverMaxDepth = 4

// Skipped only directly under the home directory.
var skipHomeDirs = map[string]bool{
	"Library": true, "Applications": true, "Music": true, "Movies": true,
	"Pictures": true, "Public": true,
}

// Skipped at any depth: dependency folders, build output, cloud drives.
var skipAnywhereDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "venv": true, "build": true, "dist": true,
	"Dropbox": true, "Google Drive": true, "OneDrive": true, "Box": true, "iCloud Drive": true,
}

type discoveredRepo struct {
	Path string
	Name string
}

type repoGroup struct {
	Parent string
	Repos  []discoveredRepo
}

// discoverRepos scans shallowly under home for Git repositories that have an
// origin remote. Repositories are grouped by parent folder. Groups with at
// least two repositories are returned first, ranked by size; lone repositories
// are returned separately so setup can mention `repo-sync add`.
func discoverRepos(ctx context.Context, runner commandRunner, home string) ([]repoGroup, []discoveredRepo, error) {
	var repos []discoveredRepo
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == home {
				return walkErr
			}
			return nil
		}
		if !entry.IsDir() || path == home {
			return nil
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		name := entry.Name()
		if strings.HasPrefix(name, ".") || skipAnywhereDirs[name] || (depth == 1 && skipHomeDirs[name]) {
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			if _, err := runGit(ctx, runner, path, "remote", "get-url", "origin"); err == nil {
				repos = append(repos, discoveredRepo{Path: path, Name: name})
			}
			return filepath.SkipDir
		}
		if depth >= discoverMaxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	byParent := make(map[string][]discoveredRepo)
	for _, repo := range repos {
		parent := filepath.Dir(repo.Path)
		byParent[parent] = append(byParent[parent], repo)
	}
	var groups []repoGroup
	var singles []discoveredRepo
	for parent, members := range byParent {
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
		if len(members) < 2 {
			singles = append(singles, members...)
			continue
		}
		groups = append(groups, repoGroup{Parent: parent, Repos: members})
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Repos) != len(groups[j].Repos) {
			return len(groups[i].Repos) > len(groups[j].Repos)
		}
		return groups[i].Parent < groups[j].Parent
	})
	sort.Slice(singles, func(i, j int) bool { return singles[i].Path < singles[j].Path })
	return groups, singles, nil
}

// selectRepos shows every discovered repository once, numbered, and asks
// which ones to sync. Nothing is preselected: automatic commits are opt-in.
// Already-synced repositories are shown but cannot be chosen again.
func selectRepos(in io.Reader, out io.Writer, groups []repoGroup, cfg config) ([]discoveredRepo, error) {
	var options []discoveredRepo
	fmt.Fprintln(out, "Found these repositories:")
	for _, group := range groups {
		fmt.Fprintf(out, "\n%s\n", group.Parent)
		for _, repo := range group.Repos {
			if _, synced := cfg.findRepo(repo.Path); synced {
				fmt.Fprintf(out, "      %s (already synced)\n", repo.Name)
				continue
			}
			options = append(options, repo)
			fmt.Fprintf(out, "  %2d. %s\n", len(options), repo.Name)
		}
	}
	if len(options) == 0 {
		fmt.Fprintln(out, "\nNothing new to add.")
		return nil, nil
	}
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "\nWhich should repo-sync keep in sync? Enter numbers (e.g. 1 3 5-7), 'all', or press Enter for none: ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, err
			}
			return nil, nil
		}
		indexes, err := parseSelection(scanner.Text(), len(options))
		if err != nil {
			fmt.Fprintf(out, "%v\n", err)
			continue
		}
		var selected []discoveredRepo
		for _, i := range indexes {
			selected = append(selected, options[i-1])
		}
		return selected, nil
	}
}

// parseSelection turns "1 3 5-7" or "all" into sorted unique 1-based indexes.
func parseSelection(input string, count int) ([]int, error) {
	input = strings.TrimSpace(input)
	if input == "" || strings.EqualFold(input, "none") {
		return nil, nil
	}
	if strings.EqualFold(input, "all") {
		all := make([]int, count)
		for i := range all {
			all[i] = i + 1
		}
		return all, nil
	}
	chosen := make(map[int]bool)
	for _, token := range strings.FieldsFunc(input, func(r rune) bool { return r == ' ' || r == ',' }) {
		first, last := token, token
		if i := strings.Index(token, "-"); i > 0 {
			first, last = token[:i], token[i+1:]
		}
		from, err1 := strconv.Atoi(first)
		to, err2 := strconv.Atoi(last)
		if err1 != nil || err2 != nil || from < 1 || to > count || from > to {
			return nil, fmt.Errorf("invalid choice %q; use numbers between 1 and %d", token, count)
		}
		for i := from; i <= to; i++ {
			chosen[i] = true
		}
	}
	result := make([]int, 0, len(chosen))
	for i := range chosen {
		result = append(result, i)
	}
	sort.Ints(result)
	return result, nil
}
