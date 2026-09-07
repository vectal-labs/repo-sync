package app

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
// origin remote. Every repository appears in a parent-folder group, ranked by
// size. The second return value is retained for existing callers and is nil.
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
	for parent, members := range byParent {
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
		groups = append(groups, repoGroup{Parent: parent, Repos: members})
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Repos) != len(groups[j].Repos) {
			return len(groups[i].Repos) > len(groups[j].Repos)
		}
		return groups[i].Parent < groups[j].Parent
	})
	return groups, nil, nil
}

// selectRepos shows every discovered repository once, numbered, and asks
// which ones to sync. Nothing is preselected: automatic commits are opt-in.
// Already-synced repositories are shown but cannot be chosen again.
func selectRepos(in io.Reader, out io.Writer, groups []repoGroup, cfg config) ([]discoveredRepo, error) {
	var options []discoveredRepo
	if len(groups) > 0 {
		fmt.Fprintln(out, "Found these repositories:")
	} else {
		fmt.Fprintln(out, "No repositories found. You can add a path below.")
	}
	for _, group := range groups {
		fmt.Fprintf(out, "\n%s\n", group.Parent)
		for _, repo := range group.Repos {
			if repositoryIsSynced(cfg, repo.Path) {
				fmt.Fprintf(out, "      %s (already synced)\n", repo.Name)
				continue
			}
			options = append(options, repo)
			fmt.Fprintf(out, "  %2d. %s\n", len(options), repo.Name)
		}
	}
	if len(options) == 0 && len(groups) > 0 {
		fmt.Fprintln(out, "\nNo new repositories listed. You can add a path below.")
	}
	reader, ok := in.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(in)
	}
	for {
		fmt.Fprint(out, "\nChoose numbers (e.g. 1 3 5-7), 'all', or 'none'. Enter an absolute or ~/ path to add it to this list. Press Enter for none: ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		input := strings.TrimSpace(line)
		if filepath.IsAbs(input) || strings.HasPrefix(input, "~/") {
			repo, err := manualRepository(input)
			if err != nil {
				fmt.Fprintln(out, err)
				continue
			}
			if repositoryIsSynced(cfg, repo.Path) {
				fmt.Fprintf(out, "%s is already synced. Choose another repository.\n", repo.Path)
				continue
			}
			index := 0
			for i, option := range options {
				if canonicalRepoPath(option.Path) == canonicalRepoPath(repo.Path) {
					index = i + 1
					break
				}
			}
			if index == 0 {
				options = append(options, repo)
				index = len(options)
				fmt.Fprintf(out, "  %2d. %s (%s)\n", index, repo.Name, repo.Path)
			} else {
				fmt.Fprintf(out, "%s is already listed as %d.\n", repo.Path, index)
			}
			fmt.Fprintf(out, "Nothing selected yet. Enter %d or 'all' to select it, or add another path.\n", index)
			continue
		}
		indexes, err := parseSelection(input, len(options))
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

func manualRepository(path string) (discoveredRepo, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return discoveredRepo{}, fmt.Errorf("find home directory: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	root, err := repoRoot(path)
	if err != nil {
		return discoveredRepo{}, err
	}
	if _, err := runGit(context.Background(), execCommandRunner{}, root, "remote", "get-url", "origin"); err != nil {
		return discoveredRepo{}, fmt.Errorf("%s has no origin remote; add one with `git remote add origin <url>`", root)
	}
	return discoveredRepo{Path: root, Name: filepath.Base(root)}, nil
}

func canonicalRepoPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func repositoryIsSynced(cfg config, path string) bool {
	path = canonicalRepoPath(path)
	for _, repo := range cfg.Repositories {
		if canonicalRepoPath(repo.Path) == path {
			return true
		}
	}
	return false
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
	if count == 0 {
		return nil, fmt.Errorf("no numbered repositories yet; enter an absolute or ~/ path, or press Enter for none")
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
