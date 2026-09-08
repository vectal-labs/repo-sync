package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const latestReleaseURL = "https://api.github.com/repos/vectal-labs/repo-sync/releases/latest"
const homebrewCask = "vectal-labs/tap/repo-sync"

var stableVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func stableReleaseVersion(value string) bool {
	if !stableVersionPattern.MatchString(value) {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "v"), ".") {
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func compareReleaseVersions(a, b string) int {
	aa, bb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.ParseUint(aa[i], 10, 64)
		y, _ := strconv.ParseUint(bb[i], 10, 64)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func fetchLatestRelease(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "repo-sync/"+appVersion())
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("check releases: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check releases: HTTP %d; will retry", response.StatusCode)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("read release: %w", err)
	}
	if release.Draft || release.Prerelease || !stableReleaseVersion(release.Tag) {
		return "", fmt.Errorf("release is not a stable version")
	}
	return "v" + strings.TrimPrefix(release.Tag, "v"), nil
}

type brewUpdateInfo struct {
	Version   string `json:"version"`
	Tap       string `json:"tap"`
	Installed string `json:"installed"`
	Pinned    bool   `json:"pinned"`
	Disabled  bool   `json:"disabled"`
}

func readBrewUpdateInfo(ctx context.Context, runner commandRunner, brew string) (brewUpdateInfo, error) {
	output, err := runner.run(ctx, "", "", brew, "info", "--json=v2", "--cask", homebrewCask)
	if err != nil {
		return brewUpdateInfo{}, err
	}
	var result struct {
		Casks []brewUpdateInfo `json:"casks"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return brewUpdateInfo{}, fmt.Errorf("read Homebrew version: %w", err)
	}
	if len(result.Casks) != 1 || result.Casks[0].Tap != "vectal-labs/tap" || !stableReleaseVersion(result.Casks[0].Version) {
		return brewUpdateInfo{}, fmt.Errorf("Homebrew returned an unrecognized repo-sync release")
	}
	return result.Casks[0], nil
}
