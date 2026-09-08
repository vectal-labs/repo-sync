package app

import (
	"context"
	"errors"
	"strings"
)

func macOSNotify(ctx context.Context, runner commandRunner, message string) error {
	escape := func(value string) string {
		value = strings.ReplaceAll(value, "\\", "\\\\")
		return strings.ReplaceAll(value, "\"", "\\\"")
	}
	script := `display notification "` + escape(message) + `" with title "repo-sync"`
	// osascript can exit successfully while stderr reports that it could not
	// reach Notification Center. Keep that failure retryable by the caller.
	var deliveryFailed bool
	if execRunner, ok := runner.(execCommandRunner); ok {
		previousWarn := execRunner.warn
		execRunner.warn = func(format string, args ...any) {
			for _, arg := range args {
				if text, ok := arg.(string); ok {
					lower := strings.ToLower(text)
					if strings.Contains(lower, "connection invalid") || strings.Contains(lower, "connection to notification center invalid") || strings.Contains(lower, "serverconnectionfailure") {
						deliveryFailed = true
					}
				}
			}
			if previousWarn != nil {
				previousWarn(format, args...)
			}
		}
		runner = execRunner
	}
	_, err := runner.run(ctx, "", "", "osascript", "-e", script)
	if err == nil && deliveryFailed {
		return errors.New("could not reach macOS Notification Center")
	}
	return err
}
