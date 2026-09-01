package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIsOfflineError(t *testing.T) {
	offline := []string{
		"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com",
		"fatal: unable to access 'https://github.com/x/y.git/': Failed to connect to github.com port 443 after 75003 ms: Couldn't connect to server",
		"fatal: unable to access 'https://github.com/x/y.git/': Recv failure: Connection reset by peer",
		"git timed out: context deadline exceeded",
	}
	for _, message := range offline {
		if !isOfflineError(errors.New(message)) {
			t.Errorf("%q should count as offline", message)
		}
	}
	notOffline := []string{
		"fatal: unable to access 'https://github.com/x/y.git/': LibreSSL/3.3.6: error:1404B42E:SSL routines:ST_CONNECT:tlsv1 alert protocol version",
		"fatal: could not read Username for 'https://github.com': Device not configured",
		"error: RPC failed; HTTP 408",
	}
	for _, message := range notOffline {
		if isOfflineError(errors.New(message)) {
			t.Errorf("%q must not count as offline; it needs a popup if it persists", message)
		}
	}
	if isOfflineError(nil) {
		t.Error("nil is not an error")
	}
}

func TestFailureAlertsCoalesceWithinWindow(t *testing.T) {
	sent := make(chan string, 4)
	alerts := &failureAlerts{window: 20 * time.Millisecond, send: func(message string) { sent <- message }}
	alerts.add("legal", errors.New("boom"))
	alerts.add("comp", errors.New("boom"))
	select {
	case message := <-sent:
		if !strings.HasPrefix(message, "2 repositories") || !strings.Contains(message, "comp, legal") {
			t.Fatalf("message = %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alert never sent")
	}
	select {
	case message := <-sent:
		t.Fatalf("second popup for the same burst: %q", message)
	case <-time.After(60 * time.Millisecond):
	}

	alerts.add("ideas", errors.New("later"))
	select {
	case message := <-sent:
		if !strings.HasPrefix(message, "ideas has been failing") {
			t.Fatalf("single-repo message = %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a later failure must open a new window")
	}
}
