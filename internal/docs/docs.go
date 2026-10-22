// Package docs ships LANDING.md, the rules for landing with this version of land, written for any repository, and
// `land docs`, which writes it into a repository. A landing refuses a repository whose copy isn't this version's.
package docs

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytes032/land/internal/proc"
)

// File is the rules' name at the repository root.
const File = "LANDING.md"

//go:embed LANDING.md
var landing []byte

// Main is `land docs`: it writes LANDING.md at the root of the worktree land runs in.
func Main(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: land docs")
	}
	root, err := proc.Output(ctx, proc.Spec{Argv: []string{"git", "rev-parse", "--show-toplevel"}})
	if err != nil {
		return fmt.Errorf("land docs runs in a git worktree: %w", err)
	}
	root = strings.TrimSpace(root)
	if err := Write(root); err != nil {
		return err
	}
	fmt.Printf("wrote %s; commit it with the .land.yaml that pins this version\n", filepath.Join(root, File))
	return nil
}

// Write writes this version's LANDING.md at root.
func Write(root string) error {
	return os.WriteFile(filepath.Join(root, File), landing, 0o644)
}

// Current fails unless root holds this version's LANDING.md, byte for byte.
func Current(root string) error {
	data, err := os.ReadFile(filepath.Join(root, File))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(data, landing) {
		return fmt.Errorf("%s is missing or isn't this land version's, so nothing was pushed: run land docs, commit it, then land again", File)
	}
	return nil
}
