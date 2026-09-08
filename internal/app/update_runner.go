package app

import (
	"context"
	"os"
)

// Only Homebrew commands need a supervisor: they spawn installers and hooks
// which must retain update locks even if the original updater disappears.
type updateBrewRunner struct {
	base  execCommandRunner
	brew  string
	locks []*os.File
}

func (r *updateBrewRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	if r.brew != "" && name == r.brew {
		return runSupervisedUpdateCommand(ctx, r.base, r.locks, name, args...)
	}
	return r.base.run(ctx, dir, stdin, name, args...)
}
