package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nicolasacchi/mbx/internal/commands"
)

var version = "dev"

func main() {
	commands.SetVersion(version)
	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	// errors.As unwraps %w chains, so a wrapped *APIError (e.g. the
	// "max retries exceeded: %w" path or a "fetch …: %w" command wrap) still
	// yields its canonical ExitCode instead of falling through to 1.
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}
