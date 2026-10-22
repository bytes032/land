package landing

import (
	"context"
	"errors"
	"fmt"

	"github.com/bytes032/land/internal/check"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
)

// push pushes landing, a commit on onto, to the trunk, fast-forward only and with no lock. Each time the trunk moved
// meanwhile, the push is refused, and push fetches the trunk, rebases landing onto it with squash, regenerates as the
// first time, and pushes again: once derived files conflicted, only after checking the regenerated commit, when
// checks and the push comes after the check (push: after-check); at once otherwise, the check left for after the push.
// Any other conflict stops it with nothing pushed. A push git reports failed though the trunk holds landing, as when
// the connection dropped once the remote had updated it, reached the trunk. It returns the commit pushed and verdict,
// the outcome of its check before the push, whose State is "" when it had none. A stop puts an amended commit back as
// squashed (unamend).
func (l *lander) push(ctx context.Context, landing, onto, message string, verdict check.Outcome, checks bool) (string, check.Outcome, error) {
	for {
		_, refused := l.git(ctx, "push", "--quiet", "origin", landing+":refs/heads/"+l.cfg.Trunk)
		if refused == nil {
			return landing, verdict, nil
		}
		moved, err := l.fetch(ctx)
		if err != nil {
			return "", check.Outcome{}, l.unamend(ctx, errors.Join(refused, err))
		}
		if _, err := l.git(ctx, "merge-base", "--is-ancestor", landing, moved); err == nil {
			l.say("git reported the push of %s failed, but %s holds it: it reached the trunk (%v)", short(landing), l.cfg.Trunk, refused)
			return landing, verdict, nil
		}
		if moved == onto {
			return "", check.Outcome{}, l.unamend(ctx, fmt.Errorf("the push of %s to %s failed, though %s didn't move, so nothing was pushed: %w",
				short(landing), l.cfg.Trunk, l.cfg.Trunk, refused))
		}
		l.say("%s moved to %s during the landing: rebasing onto it", l.cfg.Trunk, short(moved))
		rebased, conflicted, err := l.squash(ctx, landing, onto, moved, message)
		if err != nil {
			return "", check.Outcome{}, l.unamend(ctx, err)
		}
		if err := l.reload(); err != nil {
			return "", check.Outcome{}, err
		}
		if landing, err = l.regenerate(ctx, moved, rebased, message, conflicted); err != nil {
			return "", check.Outcome{}, err
		}
		onto, verdict = moved, check.Outcome{}
		if len(conflicted) > 0 && checks && l.cfg.Push == config.PushAfterCheck {
			if verdict, err = l.check(ctx, landing); err != nil {
				return "", check.Outcome{}, l.unamend(ctx, err)
			}
		}
	}
}

// checkPushed marks landing, pushed with no check of its own yet, after a lost race or under push: first, pending, then
// checks it on the box and sets its final mark, rerunning its completed CI run, as checkAfter does.
func (l *lander) checkPushed(ctx context.Context, landing string) error {
	pending := &forge.Mark{State: forge.Pending, Description: "The check after the push is running on the pusher's box"}
	if err := l.forge.SetMark(ctx, landing, *pending); err != nil {
		l.say("marking %s pending failed, so it has no mark until its check ends: %v", short(landing), err)
		pending = nil
	}
	l.say("marked %s pending: checking it after the push", short(landing))
	return l.checkAfter(ctx, landing, pending)
}
