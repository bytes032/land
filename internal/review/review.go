// Package review is the review ledger: a finished review recorded per change key, so a clean rebase keeps it. Ticket #12.
//
//	land review [ARGS…]   runs the review stage with the base, a fresh empty directory and ARGS, and records it
//	land review check     fails unless a finished review is recorded for the change
//
// Records live under config.StateDir(repo)/reviews/<key>/: the stage's output directory as report/, and a finished
// marker written last, so a record without it never counts.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bytes032/land/internal/classify"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/stage"
)

// Key is the change's review key: git patch-id --verbatim over git diff-tree -p -U0 --no-renames --full-index
// base..HEAD minus exclude (git pathspecs), or over the whole diff when exclude leaves nothing. diff-tree is plumbing,
// so diff.noprefix, diff.algorithm and other diff settings can't change the key; patch-id ignores hunk line numbers, so
// a clean rebase keeps it, while any byte the change adds or removes changes it. A binary file's diff is the object
// ids patch-id hashes, written in full, so neither core.abbrev nor the repository's growth changes them. An empty
// change has no key.
func Key(ctx context.Context, dir, base string, exclude []string) (string, error) {
	diff := []string{"diff-tree", "-p", "-U0", "--no-renames", "--full-index", base, "HEAD", "--"}
	pathspec := make([]string, len(exclude))
	for i, path := range exclude {
		pathspec[i] = ":(top,exclude,literal)" + path
	}
	patch, err := git(ctx, dir, nil, slices.Concat(diff, pathspec)...)
	if err == nil && patch == "" && len(exclude) > 0 {
		patch, err = git(ctx, dir, nil, diff...)
	}
	if err != nil {
		return "", err
	}
	if patch == "" {
		return "", fmt.Errorf("HEAD changes nothing since %s: an empty change has no review", base)
	}
	out, err := git(ctx, dir, strings.NewReader(patch), "patch-id", "--verbatim")
	id, _, _ := strings.Cut(out, " ")
	if err == nil && id == "" {
		err = errors.New("git patch-id printed no ID")
	}
	return id, err
}

// Finished fails unless a finished review is recorded for the change from base to HEAD.
func Finished(ctx context.Context, dir, base string, exclude []string) error {
	key, err := Key(ctx, dir, base, exclude)
	if err != nil {
		return err
	}
	record, err := recordDir(ctx, dir, key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(record, "finished")); err != nil {
		return fmt.Errorf("no finished review of the change from %s to HEAD (review key %s): run land review", base, key)
	}
	return nil
}

// Main is `land review [check] …`.
func Main(ctx context.Context, args []string) error {
	root, err := git(ctx, ".", nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	exclude := classify.ReviewExclusions(cfg.Stages)
	if len(args) > 0 && args[0] == "check" {
		if len(args) > 1 {
			return errors.New("usage: land review check")
		}
		return check(ctx, root, cfg.Trunk, exclude, os.Stdout)
	}
	return runReview(ctx, root, cfg, exclude, args, os.Stdout, os.Stderr)
}

// check is `land review check`.
func check(ctx context.Context, root, trunk string, exclude []string, out io.Writer) error {
	base, err := changeBase(ctx, root, trunk)
	if err != nil {
		return err
	}
	if err := Finished(ctx, root, base, exclude); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "review: a finished review of the change from %s to HEAD is recorded\n", base)
	return nil
}

// runReview is `land review [ARGS…]`: it runs the review stage over the committed change and records it as finished when
// the stage exits 0 and the change's key is still the one it reviewed.
func runReview(ctx context.Context, root string, cfg *config.Config, exclude, args []string, stdout, stderr io.Writer) error {
	if cfg.Stages.Review == "" {
		return fmt.Errorf("%s has no review stage", config.File)
	}
	// The key is HEAD's change; a reviewer could see edits the key doesn't hold.
	dirty, err := git(ctx, root, nil, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("commit your changes first: the review covers HEAD's change\n%s", dirty)
	}
	base, err := changeBase(ctx, root, cfg.Trunk)
	if err != nil {
		return err
	}
	key, err := Key(ctx, root, base, exclude)
	if err != nil {
		return err
	}
	record, err := recordDir(ctx, root, key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(record, "finished")); err == nil {
		_, _ = fmt.Fprintf(stdout, "review: the change from %s already has a finished review (review key %s): %s\n",
			base, key, filepath.Join(record, "report"))
		return nil
	}
	reviews := filepath.Dir(record)
	if err := os.MkdirAll(reviews, 0o700); err != nil {
		return err
	}
	// Under reviews/, so the rename into the record never crosses a filesystem.
	output, err := os.MkdirTemp(reviews, ".run-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(output) }()
	_, _ = fmt.Fprintf(stdout, "review: reviewing the change from %s (review key %s)\n", base, key)
	err = stage.Run(ctx, stage.Spec{
		Dir: root, Command: cfg.Stages.Review, Args: slices.Concat([]string{base, output}, args), Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		return fmt.Errorf("the review stage: %w; nothing recorded", err)
	}
	after, err := Key(ctx, root, base, exclude)
	if err != nil {
		return err
	}
	if after != key {
		return fmt.Errorf("HEAD's change moved during the review (review key %s, now %s); nothing recorded", key, after)
	}
	if err := os.RemoveAll(record); err != nil { // a record a crash left without its marker
		return err
	}
	if err := os.Mkdir(record, 0o700); err != nil {
		return err
	}
	if err := os.Rename(output, filepath.Join(record, "report")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(record, "finished"), nil, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "review: finished; recorded under review key %s: %s\n", key, filepath.Join(record, "report"))
	return nil
}

// changeBase fetches the trunk into refs/worktree/<trunk>, the worktree's own ref, so no other worktree's fetch moves
// it, and returns HEAD's merge-base with it. The empty --refmap leaves the shared remote-tracking ref alone.
func changeBase(ctx context.Context, root, trunk string) (string, error) {
	ref := "refs/worktree/" + trunk
	if _, err := git(ctx, root, nil, "fetch", "--quiet", "--refmap=", "origin", "refs/heads/"+trunk+":"+ref); err != nil {
		return "", err
	}
	base, err := git(ctx, root, nil, "merge-base", "HEAD", ref)
	return strings.TrimSpace(base), err
}

// recordDir is where the review of the change with key is recorded for dir's repository.
func recordDir(ctx context.Context, dir, key string) (string, error) {
	repo, err := config.RepoName(ctx, dir)
	if err != nil {
		return "", err
	}
	state, err := config.StateDir(repo)
	return filepath.Join(state, "reviews", key), err
}

// git runs git in dir with stdin and returns its stdout; its error carries git's stderr.
func git(ctx context.Context, dir string, stdin io.Reader, args ...string) (string, error) {
	var stderr strings.Builder
	out, err := proc.Output(ctx, proc.Spec{Argv: slices.Concat([]string{"git", "-C", dir}, args), Stdin: stdin, Stderr: &stderr})
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
