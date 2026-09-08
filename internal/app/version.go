package app

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
)

// Releases set buildVersion through -ldflags. Go installations instead carry
// their module version in build information.
var buildVersion string

func appVersion() string {
	info, _ := debug.ReadBuildInfo()
	return resolveAppVersion(buildVersion, info)
}

func resolveAppVersion(version string, info *debug.BuildInfo) string {
	if stableReleaseVersion(version) {
		return "v" + strings.TrimPrefix(version, "v")
	}
	if version != "" {
		return "dev"
	}
	if info != nil && stableReleaseVersion(info.Main.Version) {
		return "v" + strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}

func binaryVersion(ctx context.Context, runner commandRunner, binary string) (string, error) {
	output, err := runner.run(ctx, "", "", binary, "version")
	if err != nil {
		return "", fmt.Errorf("read installed version: %w", err)
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(output), "repo-sync ")
	if !ok || !stableReleaseVersion(version) {
		return "", fmt.Errorf("installed program did not report a stable repo-sync version")
	}
	return "v" + strings.TrimPrefix(version, "v"), nil
}
