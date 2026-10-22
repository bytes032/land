package landing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/check"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/verdict"
)

// RecheckMain is `land recheck SHA`: it finishes the mark of SHA, a commit on the trunk marked pending, error or
// nothing, as when the session or the box of its check died, by checking it again. It refuses success and failure,
// which only a fix forward changes.
func RecheckMain(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: land recheck SHA")
	}
	l, err := open(ctx)
	if err != nil {
		return err
	}
	return l.recheck(ctx, args[0])
}

// recheck checks rev, a commit on the trunk whose mark isn't final, with its own configuration, in a worktree land adds
// for it, on that worktree's box, and sets its final mark; the worktree land runs in is never touched. The added
// worktree and its box go once the recheck ends.
func (l *lander) recheck(ctx context.Context, rev string) error {
	onto, err := l.fetch(ctx)
	if err != nil {
		return err
	}
	sha, err := l.git(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return err
	}
	if _, err := l.git(ctx, "merge-base", "--is-ancestor", sha, onto); err != nil {
		return fmt.Errorf("%s isn't on %s, so land recheck doesn't check it", short(sha), l.cfg.Trunk)
	}
	v, err := verdict.Of(ctx, l.forge, l.root, sha)
	if err != nil {
		return err
	}
	if v.Mark != nil && (v.Mark.State == forge.Success || v.Mark.State == forge.Failure) {
		return fmt.Errorf("%s is marked %s already, which land recheck leaves as it is: a failure is fixed forward", short(sha), v.Mark.State)
	}
	dir, err := os.MkdirTemp("", "land-recheck-")
	if err != nil {
		return err
	}
	if _, err := l.git(ctx, "worktree", "add", "--quiet", "--detach", dir, sha); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	r := *l
	r.root = dir
	defer func() {
		if _, err := l.git(context.WithoutCancel(ctx), "worktree", "remove", "--force", dir); err != nil {
			l.say("removing the worktree %s failed: %v", dir, err)
		}
	}()
	if r.cfg, err = config.Load(dir); err != nil {
		return fmt.Errorf("%s's configuration: %w", short(sha), err)
	}
	defer func() {
		if err := r.release(context.WithoutCancel(ctx), dir, r.cfg); err != nil {
			l.say("releasing the box of %s failed: %v", dir, err)
		}
	}()
	l.say("rechecking %s, marked %s, in %s", short(sha), state(v.Mark), dir)
	return r.checkAfter(ctx, sha, v.Mark)
}

// checkAfter checks sha, a commit already on the trunk, held by the worktree at root, on its box, and sets the final
// mark in place of previous, sha's mark until now (nil for none): success; failure, with a commit comment listing each
// failed check's name, command, exit and log tail, which the mark links to; or error, naming where the box stopped,
// with no comment. A prepare error repeated at the commit, previous being one too, is failure instead, listing
// prepare: the commit broke the repository's setup, so it is fixed forward rather than rechecked forever. After a final
// mark, the commit's completed CI run is rerun. A check that ends with no outcome, as when ctx ends, leaves the mark.
func (l *lander) checkAfter(ctx context.Context, sha string, previous *forge.Mark) error {
	out, err := l.check(ctx, sha)
	if out.State == "" {
		return fmt.Errorf("the check of %s stopped before a verdict, so its mark is still %s: %w; finish it with land recheck %s",
			short(sha), state(previous), err, sha)
	}
	mark, failed := forge.Mark{State: out.State, Description: out.Detail}, out.Failed
	if out.Stage == "prepare" && previous != nil && previous.State == forge.Error && check.StageOf(previous.Description) == "prepare" {
		mark = forge.Mark{State: forge.Failure, Description: "prepare failed again at this commit: the commit broke the repository's setup"}
		failed = []check.CheckResult{l.prepare(out.Err)}
	}
	var posted error
	if mark.State == forge.Failure {
		if mark.TargetURL, posted = l.forge.Comment(ctx, sha, comment(failed)); posted != nil {
			posted = fmt.Errorf("posting what failed as a comment on %s failed, so its mark links to none: %w", short(sha), posted)
		}
	}
	if err := l.forge.SetMark(ctx, sha, mark); err != nil {
		return errors.Join(posted, fmt.Errorf("marking %s %s failed: %w; finish it with land recheck %s", short(sha), mark.State, err, sha))
	}
	l.say("marked %s %s: %s", short(sha), mark.State, mark.Description)
	if mark.State == forge.Error {
		return fmt.Errorf("the box stopped at %s before a verdict on %s, so it is marked error: %s; finish it with land recheck %s",
			out.Stage, short(sha), out.Detail, sha)
	}
	var red error
	if mark.State == forge.Failure {
		red = fmt.Errorf("the check of %s failed, so %s is red there: fix it forward; the mark links to what failed", short(sha), l.cfg.Trunk)
	}
	return errors.Join(red, posted, l.rerun(ctx, sha))
}

// prepare is the prepare stage as a failed check, from the error that stopped the box command: its command, its exit
// and the end of its log.
func (l *lander) prepare(err error) check.CheckResult {
	c := check.CheckResult{Name: "prepare", Command: l.cfg.Stages.Prepare, Exit: -1}
	var exit proc.Exit
	if errors.As(err, &exit) {
		c.Exit = exit.Code
	}
	var stopped *boxes.StageError
	if errors.As(err, &stopped) {
		c.Tail = stopped.Log
	}
	return c
}

// fence fences a command or a log in a comment; a log line of eight tildes would end it early.
const fence = "~~~~~~~~"

// comment is a failure's commit comment: each failed check's name, command, exit and the end of its log.
// shortcut: GitHub refuses a comment over 65536 characters, some 15 failed checks with full tails, and the mark then
// links to none; cut the tails to fit if a repository ever fails that many at once.
func comment(failed []check.CheckResult) string {
	var b strings.Builder
	b.WriteString("The check of this commit on its box failed, so the trunk is red here: fix it forward.\n")
	for _, c := range failed {
		fmt.Fprintf(&b, "\n**%s** exited %d:\n\n%ssh\n%s\n%s\n\nThe end of its log:\n\n%s\n%s\n%s\n", c.Name, c.Exit, fence, c.Command, fence, fence, c.Tail, fence)
	}
	return b.String()
}

// rerun reruns the commit's completed push runs of the CI workflow, once a final mark replaced a pending, missing or
// error one, so the runs copy it; a run still going copies it when its gate next reads it. forge.Runs lists that
// workflow's push runs alone, so no other workflow's run is ever touched.
func (l *lander) rerun(ctx context.Context, sha string) error {
	runs, err := l.forge.Runs(ctx, sha)
	if err != nil {
		return fmt.Errorf("reading %s's CI runs failed, so rerun its completed %s run by hand: %w", short(sha), forge.Workflow, err)
	}
	var errs []error
	for _, r := range runs {
		if r.Status != "completed" {
			continue
		}
		if err := l.forge.Rerun(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("rerunning %s's CI run %d failed, so rerun it by hand: %w", short(sha), r.ID, err))
			continue
		}
		l.say("reran %s's CI run %d, which copies its mark", short(sha), r.ID)
	}
	return errors.Join(errs...)
}

// state names a mark's state, or none.
func state(m *forge.Mark) string {
	if m == nil {
		return "nothing"
	}
	return string(m.State)
}
