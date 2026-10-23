package boxes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytes032/land/internal/proc"
)

// box is a worktree's claimed box.
type box struct {
	p *pool
	c claim
}

func (b *box) Run(ctx context.Context, fetch []string, argv ...string) error {
	return b.run(ctx, b.p.cfg.Stages.Prepare, fetch, argv...)
}

// run is Run with prepare, the command the box script runs before argv, "" for none: a box readied by setup has none.
// crabbox uploads the box script from stdin, content-hashed under the workspace's ignored .crabbox/, and runs it with
// argv, so that it prints only argv, not the script, on every command.
func (b *box) run(ctx context.Context, prepare string, fetch []string, argv ...string) error {
	var reached stages
	err := b.p.crabboxIn(ctx, b.c.Box, strings.NewReader(b.p.code.script(b.p.cfg.Box.Env, prepare)), b.p.stdout,
		io.MultiWriter(b.p.stderr, &reached), append([]string{"run", "--id", b.c.Lease, "--reclaim", "--script-stdin", "--"}, argv...)...)
	if err != nil && reached.stage != "command" {
		// The script says when it stops itself; anything else that ends a box command before its command is
		// crabbox's, the box's or the connection's.
		at := "sync"
		if reached.stopped {
			at = reached.stage
		}
		return &StageError{Stage: at, Err: err, Log: reached.tail()}
	}
	errs := []error{err}
	for _, rel := range fetch {
		if ctx.Err() == nil {
			errs = append(errs, b.fetch(ctx, rel))
		}
	}
	return errors.Join(errs...)
}

// fetch copies rel from the box's build root over the worktree's copy, with tar over the SSH command crabbox prints for
// the box. The copy lands beside the worktree's, on its filesystem, and the worktree's moves aside only once the new
// one is whole, and back if the new one can't take its place.
// shortcut: a fetch killed midway leaves a .land-fetch-* directory beside rel, which the next command syncs to the box.
func (b *box) fetch(ctx context.Context, rel string) error {
	local := filepath.Join(b.p.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(local), ".land-fetch-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	ssh, err := b.p.ssh(ctx, b.c)
	if err != nil {
		return fmt.Errorf("fetching %s from the box: %w", rel, err)
	}
	pack, name := b.p.code.pack(rel)
	copied, old := filepath.Join(temp, "new"), filepath.Join(temp, "old")
	if err := os.Mkdir(copied, 0o700); err != nil {
		return err
	}
	// The box's tar packs rel as name, which the Mac's unpacks in new/. bash reads crabbox's quoting of the SSH
	// command's words, and pipefail fails the copy when the box's tar does.
	if err := proc.Run(ctx, proc.Spec{
		Argv:   []string{"bash", "-o", "pipefail", "-c", ssh + ` "$1" | tar -xf - -C "$2"`, "land", pack, copied},
		Stderr: b.p.stderr,
	}); err != nil {
		return fmt.Errorf("fetching %s from the box: %w", rel, err)
	}
	if err := os.Rename(local, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(filepath.Join(copied, name), local); err != nil {
		return errors.Join(err, os.Rename(old, local))
	}
	return nil
}
