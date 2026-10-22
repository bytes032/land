// Package boxes is the box pool: one box per worktree on crabbox in direct mode against Hetzner, the core's box script
// around every command run there, spares, snapshots and the sweep.
//
// A released box stays as a spare, warming at the green commit, until 5 minutes before its next paid hour, when it
// deletes itself; a claim takes a spare before it creates a box from the repository's newest snapshot. land calls
// Hetzner's API itself to list and delete boxes, for snapshots, spares' deadlines and the sweep.
//
// Every box command needs HCLOUD_TOKEN, the API token of the Hetzner project the boxes live in, in the environment:
// crabbox reads it there, land calls the API with it, and each box keeps it, in memory, to delete itself. land never
// prints it, nor puts it in an argument list. The claims live under the user's config directory (config.StateDir),
// behind an operating-system lock, so that every worktree of a repository on the machine shares them. So does each
// box's folder, which holds everything the box makes on the Mac, crabbox's state for it included, from before
// anything of it exists on Hetzner, and goes when the box does, with its server and its SSH key on Hetzner.
package boxes

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/proc"
)

// Box runs commands on a worktree's box.
type Box interface {
	// Run syncs the worktree, uncommitted changes included, to the box, runs argv at the box's build root inside the
	// core's box script (which runs the prepare stage first), then copies each fetch path, a repository-relative file
	// or directory, back over the worktree's. A failure of the box, the sync or prepare is a *StageError, and then
	// nothing is fetched: a path left on the box by an earlier command could pass for this one's.
	Run(ctx context.Context, fetch []string, argv ...string) error
}

// StageError is a box command that stopped before the command itself ran: Stage is "script" or "prepare" when the
// box script stopped itself there, and "sync" when crabbox, the box or the connection failed, in the sync or after.
type StageError struct {
	Stage string
	Err   error // crabbox's failure, its exit status in the chain
	// Log is the end, up to 40 lines and 4000 bytes, of what the stage wrote: crabbox for the sync, the box script for
	// itself, the repository's command for prepare.
	Log string
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// Claim returns the box the worktree at root holds, claiming one when it holds none. typ "" asks for no type: any box
// the worktree holds, else a box of cfg.Box.Type; another refuses a box the worktree holds of another type.
func Claim(ctx context.Context, root string, cfg *config.Config, typ string) (Box, error) {
	p, err := newPool(ctx, root, cfg)
	if err != nil {
		return nil, err
	}
	return p.claim(ctx, typ)
}

// Release releases the box the worktree at root holds, if any.
func Release(ctx context.Context, root string, cfg *config.Config) error {
	p, err := newPool(ctx, root, cfg)
	if err != nil {
		return err
	}
	return p.release(ctx)
}

const usage = "usage: land boxes claim [--type TYPE] | run [--type TYPE] -- CMD [ARGS...] | release | status | snapshot | " +
	"sweep (with HCLOUD_TOKEN, the Hetzner project's API token, in the environment)"

// Main is `land boxes …`: claim claims the worktree's box, run runs CMD on it at the build root and exits with CMD's
// exit code, release releases it as a spare, and status names each of the repository's boxes and its lease, and prints
// crabbox's own `tunnel` and `cp` commands for the worktree's box. --type asks for a server type other than box.type,
// only of a worktree with no box or a box of that type. snapshot builds the repository's snapshot, and sweep, the
// hourly backstop, deletes the project's boxes that should be gone.
func Main(ctx context.Context, args []string) error {
	if len(args) == 0 || len(args) > 1 && args[0] != "claim" && args[0] != "run" && args[0] != "warm" {
		return errors.New(usage)
	}
	switch args[0] {
	case "sweep": // in CI, for the whole project: no worktree
		return sweep(ctx, hetzner{}, time.Now(), os.Stdout)
	case "warm":
		return warmMain(ctx, args[1:])
	}
	out, err := proc.Output(ctx, proc.Spec{Argv: []string{"git", "rev-parse", "--show-toplevel"}, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	root := strings.TrimSpace(out)
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	p, err := newPool(ctx, root, cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "claim", "run":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		typ := fs.String("type", "", "the server type of a new box (default box.type)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if (args[0] == "run") != (fs.NArg() > 0) {
			return errors.New(usage)
		}
		b, err := p.claim(ctx, *typ)
		if err != nil || args[0] == "claim" {
			if err == nil {
				fmt.Printf("%s (%s, a %s)\n", b.c.Lease, b.c.Slug, b.c.Type)
			}
			return err
		}
		return exitStatus(b.Run(ctx, nil, fs.Args()...))
	case "release":
		return p.release(ctx)
	case "status":
		return p.status(ctx)
	case "snapshot":
		return p.snapshot(ctx)
	default:
		return errors.New(usage)
	}
}

// exitStatus is err with the command's own exit code for `land boxes run` to exit with, when the command ran.
func exitStatus(err error) error {
	var stage *StageError
	var exit proc.Exit
	if !errors.As(err, &stage) && errors.As(err, &exit) {
		return exitError{exit}
	}
	return err
}

type exitError struct{ proc.Exit }

func (e exitError) ExitCode() int { return max(e.Code, 1) }
