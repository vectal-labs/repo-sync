package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// A network blip fails every repository in the same second and heals within
// minutes. Alerting on the first failure turned each blip into one popup per
// repository. So: offline errors never alert, other failures alert only once
// they have lasted a while, and repositories crossing that line together
// share a single popup.
const (
	failureNotifyAfter = 10 * time.Minute
	alertCoalesce      = 30 * time.Second
)

var offlineMarkers = []string{
	"could not resolve host", "failed to connect", "couldn't connect",
	"connection reset", "recv failure", "network is unreachable",
	"timed out", "connection refused",
}

// isOfflineError reports a failure caused by the network itself. Being
// offline resolves on its own and is never worth a popup.
func isOfflineError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range offlineMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

type failedRepo struct {
	name string
	err  error
}

// failureAlerts collects repositories that just became alert-worthy and sends
// one notification for all of them after a short window.
type failureAlerts struct {
	mu      sync.Mutex
	pending []failedRepo
	timer   *time.Timer
	window  time.Duration
	send    func(string)
}

func (a *failureAlerts) add(name string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, failedRepo{name: name, err: err})
	if a.timer == nil {
		a.timer = time.AfterFunc(a.window, a.flush)
	}
}

func (a *failureAlerts) flush() {
	a.mu.Lock()
	pending := a.pending
	a.pending, a.timer = nil, nil
	a.mu.Unlock()
	if len(pending) > 0 {
		a.send(alertMessage(pending))
	}
}

func (a *failureAlerts) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
}

func alertMessage(failed []failedRepo) string {
	names := make([]string, 0, len(failed))
	for _, repo := range failed {
		names = append(names, repo.name)
	}
	sort.Strings(names)
	since := failureNotifyAfter.String()
	if len(failed) == 1 {
		return fmt.Sprintf("%s has been failing to sync for %s, retrying automatically. %v", names[0], since, failed[0].err)
	}
	// The whole burst shares one cause, so the first error speaks for all.
	return fmt.Sprintf("%d repositories have been failing to sync for %s (%s), retrying automatically. %v",
		len(failed), since, strings.Join(names, ", "), firstError(failed))
}

func firstError(failed []failedRepo) error {
	for _, repo := range failed {
		if repo.err != nil {
			return repo.err
		}
	}
	return errors.New("unknown error")
}
