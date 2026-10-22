// Command land lands an agent's work on a repository's trunk: one commit, checked on the worktree's box at the exact
// tree it pushes, pushed with no lock, and marked with a commit status that CI copies. See docs/DESIGN.md and GLOSSARY.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bytes032/land/internal/autoreview"
	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/docs"
	"github.com/bytes032/land/internal/guard"
	"github.com/bytes032/land/internal/landing"
	"github.com/bytes032/land/internal/review"
	"github.com/bytes032/land/internal/verdict"
)

// version is the module version; `go install …@vX` builds set it through the module's build info.
var version = "dev"

// commands are land's subcommands. `land` with no subcommand (or with flags first) lands the worktree's work.
var commands = map[string]func(ctx context.Context, args []string) error{
	"recheck":    landing.RecheckMain,
	"green":      verdict.GreenMain,
	"gate":       verdict.GateMain,
	"pre-push":   guard.PrePushMain,
	"review":     review.Main,
	"autoreview": autoreview.Main,
	"boxes":      boxes.Main,
	"docs":       docs.Main,
	"version":    printVersion,
	"--version":  printVersion,
}

func printVersion(context.Context, []string) error {
	fmt.Println(moduleVersion())
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := dispatch(ctx, os.Args[1:])
	stop()
	if err != nil {
		var exit interface{ ExitCode() int }
		fmt.Fprintln(os.Stderr, "land:", err)
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) > 0 {
		if run, ok := commands[args[0]]; ok {
			return run(ctx, args[1:])
		}
	}
	return landing.Main(ctx, args)
}
