package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacOSNotifyDetectsConnectionFailureDespiteZeroExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "osascript")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' 'Connection to notification center invalid. ServerConnectionFailure: 1' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := macOSNotify(context.Background(), execCommandRunner{path: dir + ":/usr/bin:/bin"}, "test")
	if err == nil || !strings.Contains(err.Error(), "Notification Center") {
		t.Fatalf("delivery failure = %v", err)
	}
}
