package main

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
  allow <path>          let a secret-guarded file in the current repository sync
  run                   run the sync service in the foreground (used by launchd)

Every command accepts --config <path>.`

func main() {
	if err := runCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		os.Exit(1)
	}
}

func runCLI(args []string) error {
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", defaultConfigPath(), "config file")

	switch command {
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
	case "add":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() > 1 {
			return fmt.Errorf("usage: repo-sync add [--config path] [repo path]")
		}
		return runAdd(*configPath, flags.Arg(0), os.Stdout)
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
	runner := execCommandRunner{}
	if _, err := runGit(context.Background(), runner, root, "remote", "get-url", "origin"); err != nil {
		return fmt.Errorf("%s has no origin remote; repo-sync needs one to push to", root)
	}
	if err := verifyRepositories(context.Background(), runner, []repoConfig{{
		Name: filepath.Base(root), Path: root, Remote: "origin",
	}}, os.Stdin, out); err != nil {
		return err
	}
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
