package landing

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/verdict"
)

// unchecked is how a landing skips the check at the push; its zero value checks. With on alone, the failure is the
// trunk's, and the landing inherits the first red commit's failure; with issue, '#N', the failure is the flaky test of
// that open issue, and the landing is marked success. Neither is ever verified: Verdict reads the Unchecked trailer.
type unchecked struct {
	on    bool
	issue string
}

// issueRef is --unchecked's optional argument, which parse checks.
var issueRef = regexp.MustCompile(`^#[1-9][0-9]*$`)

// uncheckedMark is an unchecked landing's Unchecked trailer and the mark it gets, onto being the fetched trunk: the
// first red commit and failure, or the open issue and success. It refuses when there is nothing to name.
func (l *lander) uncheckedMark(ctx context.Context, onto string, u unchecked) (string, forge.Mark, error) {
	if u.issue != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(u.issue, "#"))
		if err != nil {
			return "", forge.Mark{}, err
		}
		open, err := l.forge.IssueOpen(ctx, n)
		switch {
		case err != nil:
			return "", forge.Mark{}, fmt.Errorf("reading issue %s failed, so nothing was pushed: %w", u.issue, err)
		case !open:
			return "", forge.Mark{}, fmt.Errorf("%s isn't an open issue, so nothing was pushed: name the flaky test's open issue, or open one", u.issue)
		}
		return u.issue, forge.Mark{State: forge.Success, Description: "Unchecked: the failure is the flaky test of " + u.issue}, nil
	}
	red, err := l.firstRed(ctx, onto)
	if err != nil {
		return "", forge.Mark{}, err
	}
	return red, forge.Mark{State: forge.Failure, Description: "Unchecked: " + l.cfg.Trunk + " is red since " + short(red) + ", whose agent fixes it"}, nil
}

// firstRed is the commit where the trunk turned red: the oldest first-parent commit after the green commit, among the
// trunk's 100 newest, whose mark is failure. It refuses when none is, or when no commit there is green, since then it
// can't tell which commit turned the trunk red.
func (l *lander) firstRed(ctx context.Context, onto string) (string, error) {
	green, ok, err := verdict.GreenCommit(ctx, l.forge, l.root, onto)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding %s's green commit failed, so nothing was pushed: %w", l.cfg.Trunk, err)
	case !ok:
		return "", fmt.Errorf("none of %s's newest commits is green, so land can't tell which commit turned it red, and "+
			"nothing was pushed: for a flaky test, land with --unchecked '#N', its open issue", l.cfg.Trunk)
	}
	out, err := l.git(ctx, "rev-list", "--first-parent", "--reverse", onto, "^"+green, "--")
	if err != nil {
		return "", err
	}
	for _, sha := range strings.Fields(out) {
		v, err := verdict.Of(ctx, l.forge, l.root, sha)
		if err != nil {
			return "", err
		}
		if v.Mark != nil && v.Mark.State == forge.Failure {
			return sha, nil
		}
	}
	return "", fmt.Errorf("no commit on %s since its green commit %s is marked failure, so there is no failure to inherit, and "+
		"nothing was pushed: for a flaky test, land with --unchecked '#N', its open issue", l.cfg.Trunk, short(green))
}
