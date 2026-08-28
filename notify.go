package main

import (
	"context"
	"strings"
)

func macOSNotify(ctx context.Context, runner commandRunner, message string) error {
	escape := func(value string) string {
		value = strings.ReplaceAll(value, "\\", "\\\\")
		return strings.ReplaceAll(value, "\"", "\\\"")
	}
	script := `display notification "` + escape(message) + `" with title "repo-sync"`
	_, err := runner.run(ctx, "", "", "osascript", "-e", script)
	return err
}
