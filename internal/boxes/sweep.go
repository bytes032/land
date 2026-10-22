package boxes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sweepAge is the age past which the sweep deletes any crabbox box: an hour past the longest lease.
const sweepAge = 25 * time.Hour

var leaseID = regexp.MustCompile(`^cbx_[0-9a-f]{12}$`)

// crabboxOwned reports whether labels are crabbox's own on a server it created.
func crabboxOwned(labels map[string]string) bool {
	return labels["crabbox"] == "true" && labels["created_by"] == "crabbox" && leaseID.MatchString(labels["lease"])
}

// sweep is `land boxes sweep`, the hourly backstop CI runs: it deletes each of land's crabbox servers that is past its
// deadline label, older than sweepAge, or powered off, as crabbox does a box at the end of its lease, with crabbox's SSH
// key for it. Every repository's boxes share the project, so it needs no worktree; it never touches a server without
// crabbox's labels and a slug of land's, land-…: another tool's box.
func sweep(ctx context.Context, hz hetzner, now time.Time, out io.Writer) error {
	servers, err := hz.servers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range servers {
		if !crabboxOwned(s.Labels) || !strings.HasPrefix(s.Labels["slug"], "land-") {
			continue
		}
		deadline, err := strconv.ParseInt(s.Labels[deadlineLabel], 10, 64)
		why := ""
		switch {
		case err == nil && !now.Before(time.Unix(deadline, 0)):
			why = "past its deadline"
		case now.Sub(s.Created) > sweepAge:
			why = "older than " + sweepAge.String()
		case s.Status == "off":
			why = "powered off"
		default:
			continue
		}
		fmt.Fprintf(out, "deleting %s (server %d, lease %s): %s\n", s.Name, s.ID, s.Labels["lease"], why)
		if err := hz.remove(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}
