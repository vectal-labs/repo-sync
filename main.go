package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"

	"github.com/vectal-labs/repo-sync/internal/app"
)

//go:embed .agents/skills/repo-sync/SKILL.md .agents/skills/repo-sync/references
var bundledSkill embed.FS

func main() {
	skill, err := fs.Sub(bundledSkill, ".agents/skills/repo-sync")
	if err != nil {
		panic(err)
	}
	if err := app.Run(os.Args[1:], skill); err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		os.Exit(1)
	}
}
