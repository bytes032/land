// Package guard is `land pre-push`, the repository's pre-push hook: it refuses deleting the trunk or moving it
// backward.
package guard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/proc"
)

// PrePushMain is `land pre-push REMOTE URL`, reading git's pre-push lines on stdin. Git runs hooks at the worktree's
// root, where .land.yaml names the trunk.
func PrePushMain(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: land pre-push REMOTE URL (git's pre-push hook)")
	}
	cfg, err := config.Load(".")
	if err != nil {
		return err
	}
	return prePush(ctx, cfg.Trunk, os.Stdin)
}

// prePush refuses a push that deletes the trunk on the remote or moves it to a commit that doesn't contain the
// remote's trunk, which only a force-push can. stdin holds git's lines,
// `<local ref> <local oid> <remote ref> <remote oid>`.
func prePush(ctx context.Context, trunk string, stdin io.Reader) error {
	ref := "refs/heads/" + trunk
	lines := bufio.NewScanner(stdin)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) != 4 {
			return fmt.Errorf("pre-push: unexpected line %q", lines.Text())
		}
		local, remoteRef, remote := fields[1], fields[2], fields[3]
		switch {
		case remoteRef != ref || zero(remote): // another branch, or creating the trunk
		case zero(local):
			return fmt.Errorf("pre-push: refusing to delete the trunk, %s", trunk)
		default:
			// Exit 1 is "not an ancestor"; any other failure (a remote commit not fetched) refuses all the same.
			if err := proc.Run(ctx, proc.Spec{Argv: []string{"git", "merge-base", "--is-ancestor", remote, local}}); err != nil {
				return fmt.Errorf("pre-push: refusing to move %s from %s to %s, which doesn't contain it: the trunk "+
					"only moves forward; fetch, rebase onto the remote's %s and push again", trunk, short(remote),
					short(local), trunk)
			}
		}
	}
	return lines.Err()
}

// zero reports whether oid is git's null object ID, which names no commit.
func zero(oid string) bool { return strings.Trim(oid, "0") == "" }

func short(oid string) string { return oid[:min(len(oid), 12)] }
