package main

import (
	"fmt"
	"os"

	"github.com/vectal-labs/repo-sync/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		os.Exit(1)
	}
}
