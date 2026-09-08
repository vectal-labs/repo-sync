package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Index stages are captured before abort. During a rebase, stage 2 is the
// upstream plus earlier replayed commits; stage 3 is the stopped local commit.
type conflictVersion struct {
	Object  string `json:"object,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Omitted string `json:"omitted,omitempty"`
}

type conflictFile struct {
	Path     string          `json:"path"`
	Base     conflictVersion `json:"base"`
	Upstream conflictVersion `json:"upstream"`
	Local    conflictVersion `json:"local"`
}

type conflictError struct {
	Branch       string         `json:"branch"`
	RemoteRef    string         `json:"remote_ref"`
	LocalHead    string         `json:"local_head"`
	RemoteHead   string         `json:"remote_head"`
	ReplayedHead string         `json:"replayed_head"`
	Files        []conflictFile `json:"files"`
	AbortError   string         `json:"abort_error,omitempty"`
}

func (e *conflictError) Error() string {
	if e.AbortError != "" {
		return "rebase conflict with " + e.RemoteRef + "; rebase needs attention: " + e.AbortError
	}
	return "rebase conflict with " + e.RemoteRef + "; rebase aborted, will retry"
}

func (s gitSyncer) captureConflict(ctx context.Context, c *checkout, remoteRef string) (*conflictError, error) {
	output, err := c.git(ctx, "", "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	} // Hook/other failure, not a merge conflict.
	result := &conflictError{Branch: c.branch, RemoteRef: remoteRef, LocalHead: c.base}
	result.RemoteHead, err = s.revParse(ctx, c.path, remoteRef)
	if err != nil {
		return nil, err
	}
	result.ReplayedHead, err = s.revParse(ctx, c.path, "REBASE_HEAD")
	if err != nil {
		return nil, err
	}
	files := make(map[string]*conflictFile)
	for _, record := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		metadata, path, ok := strings.Cut(record, "\t")
		parts := strings.Fields(metadata)
		if !ok || len(parts) != 3 {
			return nil, fmt.Errorf("invalid unmerged index entry")
		}
		file := files[path]
		if file == nil {
			file = &conflictFile{Path: path}
			files[path] = file
		}
		version := conflictVersion{Mode: parts[0], Object: parts[1]}
		switch parts[2] {
		case "1":
			file.Base = version
		case "2":
			file.Upstream = version
		case "3":
			file.Local = version
		default:
			return nil, fmt.Errorf("invalid unmerged index stage")
		}
	}
	for _, file := range files {
		result.Files = append(result.Files, *file)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}

// Bound the incident size. Large blobs remain addressable by their recorded
// object IDs; no document contents are ever included in error messages.
func (s gitSyncer) captureConflictContents(ctx context.Context, path string, conflict *conflictError) {
	budget := 2 * 1024 * 1024
	for i := range conflict.Files {
		file := &conflict.Files[i]
		for _, version := range []*conflictVersion{&file.Base, &file.Upstream, &file.Local} {
			if version.Object == "" {
				continue
			}
			if version.Mode == "160000" {
				version.Omitted = "submodule commit; resolve in the submodule before updating its pointer"
				continue
			}
			sizeText, err := runGit(ctx, s.runner, path, "--no-replace-objects", "cat-file", "-s", version.Object)
			size, parseErr := strconv.Atoi(strings.TrimSpace(sizeText))
			if err != nil || parseErr != nil || size < 0 {
				version.Omitted = "object could not be read; inspect the recorded Git object"
				continue
			}
			if size > 256*1024 || size > budget {
				version.Omitted = "snapshot size limit; inspect the recorded Git object"
				continue
			}
			data, err := runGit(ctx, s.runner, path, "--no-replace-objects", "cat-file", "blob", version.Object)
			if err != nil {
				version.Omitted = "object could not be read; inspect the recorded Git object"
				continue
			}
			version.Data = []byte(data)
			budget -= len(data)
		}
	}
}
