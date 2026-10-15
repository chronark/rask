// Package main provides rask's executable entrypoint.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/chronark/rask/cmd/clean"
	testcmd "github.com/chronark/rask/cmd/test"
	"github.com/chronark/rask/cmd/why"
	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/setup"
)

// main assembles the root command and exits with its code.
func main() {
	testCommand := testcmd.Command()
	rootFlags := append([]cli.Flag{}, testCommand.Flags...)
	rootFlags = append(rootFlags, cli.Bool("version", "print version and exit"))
	app := &cli.Command{
		Name:        "rask",
		Usage:       "Run Go tests with content-addressed result caching",
		Description: "Testing takes package patterns plus -v/--verbose, --no-cache, --concurrency, --shard, --shard-timings, and --version.",
		Flags:       rootFlags,
		Action: func(ctx context.Context, command *cli.Command) error {
			if command.Bool("version") {
				fmt.Fprintf(command.Output, "rask %s\n", version)
				return nil
			}
			return testCommand.Action(ctx, command)
		},
		AcceptsArgs:        true,
		DefaultActionNames: []string{"test", "t"},
		UnknownFlagHint:    "rask supports only -v/--verbose, --no-cache, --concurrency, --shard, --shard-timings, and --version; use `go test` for other flags",
		Commands: []*cli.Command{
			clean.Command(),
			why.Command(),
		},
	}
	os.Exit(setup.Execute(app, os.Args))
}
