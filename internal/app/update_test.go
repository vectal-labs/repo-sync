package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type updateFixture struct {
	t                               *testing.T
	u                               *updater
	config                          string
	now                             time.Time
	latest, tap, installed, running string
	httpStatus                      int
	checks, notes                   int
	pinned                          bool
	commands                        []string
	failCommand                     string
	onFetch                         func()
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f := &updateFixture{t: t, config: defaultConfigPath(), now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), latest: "v1.1.0", tap: "1.1.0", installed: "v1.0.0", running: "v1.0.0", httpStatus: 200}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.checks++
		w.WriteHeader(f.httpStatus)
		fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, f.latest)
	}))
	t.Cleanup(server.Close)
	runner := preflightRunnerFunc(f.run)
	service := &launchService{runner: runner, domain: "gui/test", label: "test.repo-sync"}
	f.u = &updater{runner: runner, client: server.Client(), releaseURL: server.URL, service: service, binary: filepath.Join(os.Getenv("HOME"), "bin", "repo-sync"), brew: "/test/brew", version: "v1.0.0", out: &strings.Builder{}, now: func() time.Time { return f.now }, gateWait: time.Millisecond, notify: func(context.Context, commandRunner, string) error { f.notes++; return nil }}
	if err := writeConfig(f.config, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	plist := launchAgentPlist(f.u.binary, f.config, filepath.Join(os.Getenv("HOME"), "logs"))
	if err := writeFileAtomic(service.plistPath(os.Getenv("HOME")), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	f.heartbeat()
	return f
}

func (f *updateFixture) heartbeat() {
	f.t.Helper()
	cfg, err := loadConfig(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	status := serviceStatus{PID: 999999, Version: f.running, ConfigHash: configHash(cfg), UpdatedAt: time.Now()}
	if err := writeUpdateJSON(statusPath(f.config), status); err != nil {
		f.t.Fatal(err)
	}
}

func (f *updateFixture) run(_ context.Context, _, _, name string, args ...string) (string, error) {
	if name == f.u.binary && len(args) == 1 && args[0] == "version" {
		return "repo-sync " + f.installed + "\n", nil
	}
	if name == "/bin/launchctl" {
		switch args[0] {
		case "print":
			f.heartbeat()
			return "state = running\npid = 999999\n", nil
		default:
			f.t.Fatalf("unexpected service mutation: %v", args)
		}
	}
	if name != f.u.brew {
		f.t.Fatalf("unexpected command: %s %v", name, args)
	}
	f.commands = append(f.commands, strings.Join(args, " "))
	if args[0] == f.failCommand {
		return "", errors.New("simulated Homebrew failure https://user:secret@example.com")
	}
	switch args[0] {
	case "update":
		return "", nil
	case "info":
		data, _ := json.Marshal(map[string]any{"casks": []any{map[string]any{"tap": "vectal-labs/tap", "version": f.tap, "installed": strings.TrimPrefix(f.installed, "v"), "pinned": f.pinned}}})
		return string(data), nil
	case "fetch":
		if f.onFetch != nil {
			f.onFetch()
		}
		return "", nil
	case "upgrade":
		f.installed, f.running = f.latest, f.latest
		f.heartbeat()
		return "", nil
	}
	f.t.Fatalf("unexpected brew action: %v", args)
	return "", nil
}

func (f *updateFixture) state() updateState {
	f.t.Helper()
	state, err := loadUpdateState(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}

func TestAutomaticUpdateRefreshesBrewAndVerifiesRunningVersion(t *testing.T) {
	f := newUpdateFixture(t)
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"update --quiet", "info --json=v2 --cask " + homebrewCask, "fetch --cask " + homebrewCask, "upgrade --cask " + homebrewCask}
	if fmt.Sprint(f.commands) != fmt.Sprint(want) {
		t.Fatalf("commands %v", f.commands)
	}
	state := f.state()
	if state.Result != "updated" || state.SucceededAt.IsZero() || f.running != "v1.1.0" || f.notes != 0 {
		t.Fatalf("result %+v, running=%s notes=%d", state, f.running, f.notes)
	}
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if f.checks != 1 {
		t.Fatal("daily check was not cached")
	}
}

func TestDisabledUpdatesNotifyOnceAndManualUpdateStillWorks(t *testing.T) {
	f := newUpdateFixture(t)
	if err := setAutomaticUpdates(false, f.u.out); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.u.run(context.Background(), f.config, true); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(25 * time.Hour)
	}
	if len(f.commands) != 0 || f.notes != 1 || f.state().Result != "available" {
		t.Fatalf("commands=%v notes=%d", f.commands, f.notes)
	}
	if err := f.u.run(context.Background(), f.config, false); err != nil {
		t.Fatal(err)
	}
	if f.state().Result != "updated" {
		t.Fatal(f.state())
	}
	settings, _ := loadUpdateSettings()
	if settings.Automatic {
		t.Fatal("manual update re-enabled automatic updates")
	}
}

func TestAutomaticOffDuringDownloadPreventsInstallation(t *testing.T) {
	f := newUpdateFixture(t)
	f.onFetch = func() {
		if err := setAutomaticUpdates(false, f.u.out); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.commands, "\n"), "upgrade --cask") || f.running != "v1.0.0" {
		t.Fatal(f.commands)
	}
}

func TestUpdateFailuresPersistRedactedAndNotifyWithoutSpam(t *testing.T) {
	f := newUpdateFixture(t)
	f.failCommand = "fetch"
	for i := 0; i < 3; i++ {
		if err := f.u.run(context.Background(), f.config, true); err == nil {
			t.Fatal("failure ignored")
		}
		f.now = f.now.Add(25 * time.Hour)
	}
	if f.notes != 1 || f.state().Result != "failed" {
		t.Fatalf("notes=%d state=%+v", f.notes, f.state())
	}
	if strings.Contains(string(mustRead(t, updateStatePath(f.config))), "secret") {
		t.Fatal("credentials leaked")
	}
	f.failCommand = ""
	if err := f.u.run(context.Background(), f.config, false); err != nil {
		t.Fatal(err)
	}
	if !f.state().FailureSince.IsZero() || f.state().Notified != "" {
		t.Fatal("failure did not clear")
	}
}

func TestFailedUpgradeNotifiesImmediatelyAndKeepsRunningService(t *testing.T) {
	f := newUpdateFixture(t)
	f.failCommand = "upgrade"
	if err := f.u.run(context.Background(), f.config, true); err == nil {
		t.Fatal("upgrade failure ignored")
	}
	if f.notes != 1 || f.running != "v1.0.0" || f.state().Result != "failed" {
		t.Fatal(f.state())
	}
}

func TestStaleTapRetriesThenNotifies(t *testing.T) {
	f := newUpdateFixture(t)
	f.tap = "1.0.0"
	if err := f.u.run(context.Background(), f.config, true); err == nil {
		t.Fatal("tap lag not recorded")
	}
	if f.notes != 0 || strings.Contains(strings.Join(f.commands, "\n"), "upgrade") {
		t.Fatal(f.commands)
	}
	f.now = f.now.Add(25 * time.Hour)
	if err := f.u.run(context.Background(), f.config, true); err == nil {
		t.Fatal("persistent lag ignored")
	}
	if f.notes != 1 {
		t.Fatal("persistent tap failure is invisible")
	}
}

func TestUpdateDefersWhileGitGateIsHeld(t *testing.T) {
	f := newUpdateFixture(t)
	unlock, err := lockUpdateFile(updateGatePath(), syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := f.u.run(context.Background(), f.config, true); err == nil || !strings.Contains(err.Error(), "Git work") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(strings.Join(f.commands, "\n"), "upgrade") || f.notes != 0 {
		t.Fatal(f.commands)
	}
}

func TestConcurrentUpdateCannotStart(t *testing.T) {
	f := newUpdateFixture(t)
	unlock, err := acquireUpdateLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if f.checks != 0 {
		t.Fatal("second updater made requests")
	}
	if err := f.u.run(context.Background(), f.config, false); !errors.Is(err, errUpdateBusy) {
		t.Fatal(err)
	}
}

func TestUpdateDoesNotDowngradeAfterConcurrentHomebrewChange(t *testing.T) {
	f := newUpdateFixture(t)
	f.onFetch = func() { f.installed, f.running = "v1.2.0", "v1.2.0"; f.heartbeat() }
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.commands, "\n"), "upgrade --cask") || f.running != "v1.2.0" {
		t.Fatal(f.commands)
	}
}

func TestPinnedCaskAndNonHomebrewInstallNeedManualAction(t *testing.T) {
	for _, kind := range []string{"pinned", "standalone"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			if kind == "pinned" {
				f.pinned = true
			} else {
				f.u.brew = ""
			}
			if err := f.u.run(context.Background(), f.config, true); err != nil {
				t.Fatal(err)
			}
			if f.state().Result != "available" || f.notes != 1 || strings.Contains(strings.Join(f.commands, "\n"), "upgrade") {
				t.Fatal(f.state())
			}
		})
	}
}

func TestUpdaterRejectsOtherConfigOrInstallationBeforeRecovery(t *testing.T) {
	for _, kind := range []string{"config", "binary"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			if err := writeUpdateState(f.config, updateState{Result: "installing"}); err != nil {
				t.Fatal(err)
			}
			if kind == "binary" {
				f.u.binary = "/different/repo-sync"
			} else {
				f.config = filepath.Join(t.TempDir(), "config.json")
			}
			if err := f.u.run(context.Background(), f.config, false); err == nil {
				t.Fatal("mismatched service accepted")
			}
			if f.checks != 0 || len(f.commands) != 0 {
				t.Fatal("mismatch performed work")
			}
		})
	}
}

func TestNotificationOnlyDoesNotRestartOutdatedDaemon(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.version, f.installed = "v1.1.0", "v1.1.0"
	if err := setAutomaticUpdates(false, f.u.out); err != nil {
		t.Fatal(err)
	}
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if f.running != "v1.0.0" || f.notes != 1 || f.state().Result != "available" {
		t.Fatal(f.state())
	}
}

func TestScheduledUpdateDoesNotRecreateUninstalledSetup(t *testing.T) {
	f := newUpdateFixture(t)
	if err := os.Remove(f.u.service.plistPath(os.Getenv("HOME"))); err != nil {
		t.Fatal(err)
	}
	if err := f.u.run(context.Background(), f.config, true); err != nil {
		t.Fatal(err)
	}
	if f.checks != 0 {
		t.Fatal("uninstalled setup was checked")
	}
}

func TestReleaseValidationAndNumericOrdering(t *testing.T) {
	for _, version := range []string{"v1.0.0-beta.1", "latest", "v01.2.3", "1.2", "1.2.18446744073709551616"} {
		if stableReleaseVersion(version) {
			t.Errorf("accepted %s", version)
		}
	}
	if compareReleaseVersions("v1.10.0", "1.9.9") <= 0 || compareReleaseVersions("v2.0.0", "1.99.0") <= 0 {
		t.Fatal("versions compared lexically")
	}
	for _, body := range []string{`{"tag_name":"v1.1.0","prerelease":true}`, `{"tag_name":"v1.1.0","draft":true}`, `{"tag_name":"latest"}`, `not-json`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, err := fetchLatestRelease(context.Background(), server.Client(), server.URL)
		server.Close()
		if err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestGitGateFilesystemFailureIsVisible(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := newTestDaemon(t, "notes")
	d.updateGate = filepath.Join(t.TempDir(), "gate")
	if err := os.Mkdir(d.updateGate, 0o700); err != nil {
		t.Fatal(err)
	}
	state := d.states["notes"]
	d.syncRepo(state, true)
	state.mu.Lock()
	incident := state.incident
	state.mu.Unlock()
	if !strings.Contains(incident, "coordinate with updater") {
		t.Fatalf("gate failure invisible: %q", incident)
	}
}
