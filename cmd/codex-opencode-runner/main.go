// Package main is the entrypoint for the codex-opencode-runner CLI.
// Codex remains the orchestrator; this command wraps OpenCode's headless CLI.
package main

import (
	"context"
	"os"

	"github.com/pradhankukiran/codex-opencode-executor/internal/runner"
)

func main() {
	cfg, err := runner.ParseArgs(os.Args[1:], os.Stdin, os.Getenv)
	if err != nil {
		_ = runner.EncodeResult(os.Stdout, runner.Result{
			Status: runner.StatusFailed,
			Error:  err.Error(),
		})
		os.Exit(1)
	}

	_, exitCode := runner.Run(context.Background(), cfg, runner.RunOptions{})
	os.Exit(exitCode)
}
