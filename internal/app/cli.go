package app

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const usage = `usage: repo-sync <command> [options]

  setup                 find repositories, choose which to sync, install the service
  add [path]            start syncing a repository (defaults to the current one)
  remove [path]         stop syncing a repository; keep its files and Git history
  allow <path>          let a secret-guarded file in the current repository sync
  status                show service readiness and repository health
  version               show the installed version
  update                check and install the latest Homebrew release now
  updates on|off         enable automatic updates or keep notifications only
  uninstall             remove the service, settings, logs, and program
  run                   run the sync service in the foreground (used by launchd)

Every command accepts --config <path>.`

// Run executes a repo-sync command.
func Run(args []string) error {
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command == "update-command-supervisor" {
		return runUpdateCommandSupervisor(args)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", defaultConfigPath(), "config file")

	switch command {
	case "version":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync version")
		}
		fmt.Println("repo-sync " + appVersion())
		return nil
	case "updates":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 1 || (flags.Arg(0) != "on" && flags.Arg(0) != "off") {
			return fmt.Errorf("usage: repo-sync updates on|off")
		}
		return setAutomaticUpdates(flags.Arg(0) == "on", os.Stdout)
	case "update":
		scheduled := flags.Bool("scheduled", false, "run a due background check")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync update [--config path]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runUpdate(ctx, *configPath, *scheduled, os.Stdout)
	case "install-updater":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync install-updater [--config path]")
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		return installUpdater(context.Background(), *configPath, binary, false, defaultService())
	case "run":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync run [--config path]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx, *configPath)
	case "setup":
		noLaunch := flags.Bool("no-launch", false, "write files but do not load the LaunchAgent")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync setup [--config path] [--no-launch]")
		}
		binary, err := executablePath()
		if err != nil {
			return err
		}
		return runSetup(context.Background(), setupOptions{
			configPath: *configPath, binary: binary, noLaunch: *noLaunch, in: os.Stdin, out: os.Stdout,
		})
	case "status":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync status [--config path]")
		}
		return runStatus(context.Background(), *configPath, defaultService(), os.Stdout)
	case "uninstall":
		yes := flags.Bool("yes", false, "remove without a confirmation prompt")
		keepBinary := flags.Bool("keep-binary", false, "remove service and data, but keep the installed program")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: repo-sync uninstall [--yes] [--keep-binary] [--config path]")
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		return runUninstall(context.Background(), uninstallOptions{configPath: *configPath, binary: binary, yes: *yes, keepBinary: *keepBinary, in: os.Stdin, out: os.Stdout, service: defaultService()})
	case "add":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() > 1 {
			return fmt.Errorf("usage: repo-sync add [--config path] [repo path]")
		}
		return runAdd(*configPath, flags.Arg(0), os.Stdout)
	case "remove":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() > 1 {
			return fmt.Errorf("usage: repo-sync remove [--config path] [repo path]")
		}
		return runRemove(context.Background(), *configPath, flags.Arg(0), defaultService(), os.Stdout)
	case "allow":
		repoFlag := flags.String("repo", "", "repository path (defaults to the current repository)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: repo-sync allow [--config path] [--repo path] <file>")
		}
		return runAllow(*configPath, *repoFlag, flags.Arg(0), os.Stdout)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

// repoRoot resolves the top-level directory of the Git repository containing
// path (the working directory when empty).
func repoRoot(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	output, err := runGit(context.Background(), execCommandRunner{}, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a Git repository", abs)
	}
	root := strings.TrimSpace(output)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, nil
}

func runAdd(configPath, path string, out *os.File) error {
	root, err := repoRoot(path)
	if err != nil {
		return err
	}
	runner := backgroundRunner()
	if _, err := runGit(context.Background(), runner, root, "remote", "get-url", "origin"); err != nil {
		return fmt.Errorf("%s has no origin remote; repo-sync needs one to push to", root)
	}
	if err := verifyRepositories(context.Background(), runner, []repoConfig{{
		Name: filepath.Base(root), Path: root, Remote: "origin",
	}}, os.Stdin, out); err != nil {
		return err
	}
	unlock, err := acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	store := &configStore{path: configPath}
	var added repoConfig
	err = store.update(func(cfg *config) error {
		added, err = addRepository(cfg, root)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Syncing %s as %q\n", root, added.Name)
	applyConfigChange(out)
	return nil
}

func runAllow(configPath, repoPath, file string, out *os.File) error {
	root, err := repoRoot(repoPath)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not inside %s", abs, root)
	}
	unlock, err := acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	store := &configStore{path: configPath}
	if err := store.update(func(cfg *config) error { return allowPath(cfg, root, rel) }); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s will now sync in %s\n", filepath.ToSlash(rel), root)
	applyConfigChange(out)
	return nil
}

func applyConfigChange(out *os.File) {
	if err := restartDaemon(); err != nil {
		fmt.Fprintln(out, "The service is not running; run `repo-sync setup` to install it.")
		return
	}
	fmt.Fprintln(out, "Service restarted.")
}
