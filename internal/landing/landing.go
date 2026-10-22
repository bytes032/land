// Package landing is the landing flow: land, recheck and unchecked landings. Tickets #4, #6, #7, #8, #9.
package landing

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/check"
	"github.com/bytes032/land/internal/classify"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/docs"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/provenance"
	"github.com/bytes032/land/internal/review"
)

// Main is `land --message FILE [--unchecked ['#N']]`.
func Main(ctx context.Context, args []string) error {
	file, u, err := parse(args)
	if err != nil {
		return err
	}
	l, err := open(ctx)
	if err != nil {
		return err
	}
	return l.land(ctx, file, u)
}

// parse reads land's arguments: the message file, and how the landing skips the check, if it does.
func parse(args []string) (string, unchecked, error) {
	flags := flag.NewFlagSet("land", flag.ContinueOnError)
	message := flags.String("message", "", "the commit message `FILE`, to which land adds its trailers")
	skip := flags.Bool("unchecked", false, "land without the check at the push: alone, inheriting the failure of the "+
		"trunk's first red commit, marked failure; with '#N', for the flaky test of open issue N, marked success")
	if err := flags.Parse(args); err != nil {
		return "", unchecked{}, err
	}
	u := unchecked{on: *skip}
	if u.on && flags.NArg() > 0 {
		u.issue = flags.Arg(0)
		if !issueRef.MatchString(u.issue) {
			return "", unchecked{}, fmt.Errorf("--unchecked takes no argument, or the flaky test's open issue as '#N', not %q", u.issue)
		}
		if err := flags.Parse(flags.Args()[1:]); err != nil { // the flags after '#N'
			return "", unchecked{}, err
		}
	}
	if *message == "" || flags.NArg() > 0 {
		return "", unchecked{}, errors.New("usage: land --message FILE [--unchecked ['#N']]")
	}
	return *message, u, nil
}

// open is the lander of the worktree land runs in, wired to GitHub, the review ledger and the worktree's box.
func open(ctx context.Context) (*lander, error) {
	root, err := proc.Output(ctx, proc.Spec{Argv: []string{"git", "rev-parse", "--show-toplevel"}})
	if err != nil {
		return nil, fmt.Errorf("land runs in a git worktree: %w", err)
	}
	root = strings.TrimSpace(root)
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	f, err := forge.FromEnv(ctx, root)
	if err != nil {
		return nil, err
	}
	return &lander{
		root: root, cfg: cfg, forge: f, out: os.Stdout,
		reviewed: func(ctx context.Context, base string) error {
			return review.Finished(ctx, root, base, classify.ReviewExclusions(cfg.Stages))
		},
		claim:   boxes.Claim,
		release: boxes.Release,
	}, nil
}

// lander lands the work of the worktree at root.
type lander struct {
	root string
	// cfg is the worktree's .land.yaml until squash checks the landing commit out, then that commit's own (reload).
	cfg   *config.Config
	forge forge.Forge
	// reviewed fails unless a finished review of the change from base to HEAD is recorded.
	reviewed func(ctx context.Context, base string) error
	// claim returns the box of the worktree at root, as boxes.Claim does.
	claim func(ctx context.Context, root string, cfg *config.Config, typ string) (boxes.Box, error)
	// release gives up the box of the worktree at root, as boxes.Release does, for a worktree land added itself.
	release func(ctx context.Context, root string, cfg *config.Config) error
	out     io.Writer
	// squashed is the commit squash last wrote, and amended regenerate's amend of it, "" when it made none: a landing
	// that stops with nothing pushed puts HEAD back at squashed (unamend).
	squashed, amended string
}

// land lands the worktree's work as one commit with the message in file, checked on the box at its exact tree,
// pushed to the trunk with no force, and marked success; a CI run of the commit that completed already is rerun. A
// lost race pushes the rebased commit at once, marked pending, and checks it after the push (push, checkPushed). An
// unchecked landing skips the check and gets the mark u names, with its Unchecked trailer.
func (l *lander) land(ctx context.Context, file string, u unchecked) error {
	id, err := provenance.FromEnv()
	if err != nil {
		return err
	}
	// The message is read now to refuse a bad one early, and written once the trunk names an unchecked landing's reason.
	if _, err := l.message(file, id, ""); err != nil {
		return err
	}
	if err := l.clean(ctx); err != nil {
		return err
	}
	if err := docs.Current(l.root); err != nil {
		return err
	}
	onto, err := l.fetch(ctx)
	if err != nil {
		return err
	}
	var reason string
	var mark forge.Mark
	if u.on {
		if reason, mark, err = l.uncheckedMark(ctx, onto, u); err != nil {
			return err
		}
	}
	message, err := l.message(file, id, reason)
	if err != nil {
		return err
	}
	head, err := l.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	base, err := l.git(ctx, "merge-base", head, onto)
	if err != nil {
		return err
	}
	if base == head {
		return fmt.Errorf("nothing to land: HEAD %s is on %s already", short(head), l.cfg.Trunk)
	}
	if err := l.reviewed(ctx, base); err != nil {
		return fmt.Errorf("the change has no finished review (land review), so nothing was pushed: %w", err)
	}
	landing, conflicted, err := l.squash(ctx, head, base, onto, message)
	if err != nil {
		return err
	}
	if err := l.reload(); err != nil {
		return err
	}
	if landing, err = l.regenerate(ctx, onto, landing, message, conflicted); err != nil {
		return err
	}
	var verdict check.Outcome
	if !u.on {
		if verdict, err = l.check(ctx, landing); err != nil {
			return l.unamend(ctx, err)
		}
	}
	// An unchecked landing that loses the race pushes again at once, with no check, before or after the push.
	if landing, verdict, err = l.push(ctx, landing, onto, message, verdict, !u.on); err != nil {
		return err
	}
	l.say("pushed %s to %s; Agent %s, Agent-Session %s", short(landing), l.cfg.Trunk, id.Agent, id.Session)
	switch {
	case u.on: // the mark u names
	case verdict.State == "":
		return l.checkPushed(ctx, landing)
	default:
		mark = forge.Mark{State: forge.Success, Description: verdict.Detail}
	}
	if err := l.forge.SetMark(ctx, landing, mark); err != nil {
		return fmt.Errorf("%s is on %s, but setting its mark failed: %w; set it with land recheck %s", short(landing), l.cfg.Trunk, err, landing)
	}
	l.say("marked %s %s", short(landing), mark.State)
	return l.rerun(ctx, landing)
}

// message is the landing commit's message: file's, which must hold a line matching each of message.require's patterns,
// with id's trailer block added, Unchecked: unchecked first when unchecked isn't "".
func (l *lander) message(file string, id provenance.Identity, unchecked string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, pattern := range l.cfg.Message.Require {
		re, err := regexp.Compile("(?m)" + pattern)
		if err != nil {
			return "", fmt.Errorf("%s: message.require: %w", config.File, err)
		}
		if !re.MatchString(text) {
			return "", fmt.Errorf("%s has no line matching %q, which %s requires", file, pattern, config.File)
		}
	}
	message, err := id.Message(text, unchecked)
	if err != nil {
		return "", fmt.Errorf("%s: %w", file, err)
	}
	return message, nil
}

// clean refuses uncommitted changes and untracked files: land lands commits, and the box checks the worktree.
func (l *lander) clean(ctx context.Context) error {
	dirty, err := l.git(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("commit or drop your uncommitted changes and untracked files first:\n%s", dirty)
	}
	return nil
}

// fetch fetches the trunk into refs/worktree/<trunk>, a ref of the worktree's own that no other worktree's fetch
// moves, refusing a trunk that moved backward, and returns its commit.
func (l *lander) fetch(ctx context.Context) (string, error) {
	ref := "refs/worktree/" + l.cfg.Trunk
	// An empty --refmap leaves the remote-tracking branch alone.
	if _, err := l.git(ctx, "fetch", "--quiet", "--refmap=", "origin", "refs/heads/"+l.cfg.Trunk+":"+ref); err != nil {
		return "", err
	}
	return l.git(ctx, "rev-parse", "--verify", ref+"^{commit}")
}

// squash writes the work from base to head as one commit with message on onto, the trunk, and checks it out. The
// rebase is a merge that writes no worktree file, so a conflict stops land with HEAD, the index and the worktree as
// they were; only the merged commit is checked out. When every conflicted path is a derived file, the commit takes the
// trunk's side of each instead, and squash returns them for regeneration.
func (l *lander) squash(ctx context.Context, head, base, onto, message string) (string, []string, error) {
	tree := head + "^{tree}"
	var conflicted []string
	if base != onto {
		out, err := l.gitInput(ctx, "", "merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", "--merge-base="+base, onto, head)
		var exit proc.Exit
		if errors.As(err, &exit) && exit.Code == 1 { // 1: the merge conflicts
			conflicted = strings.Split(strings.Trim(out, "\x00"), "\x00")[1:]
			var stops []string
			for _, path := range conflicted {
				if classify.OnConflict(l.cfg.Stages, path) == classify.Stop {
					stops = append(stops, path)
				}
			}
			if len(stops) > 0 {
				return "", nil, fmt.Errorf("rebasing onto %s %s conflicts in %s, so nothing was pushed: merge %s into your branch, "+
					"resolve them, then review and land again", l.cfg.Trunk, short(onto), strings.Join(stops, ", "), short(onto))
			}
			err = nil
		}
		if err != nil {
			return "", nil, err
		}
		tree, _, _ = strings.Cut(out, "\x00")
		if len(conflicted) > 0 {
			if tree, err = l.takeTrunk(ctx, tree, onto, conflicted); err != nil {
				return "", nil, err
			}
			l.say("took %s's side of the conflicted derived files %s, to regenerate them", l.cfg.Trunk, strings.Join(conflicted, ", "))
		}
	}
	landing, err := l.gitInput(ctx, message, "commit-tree", tree, "-p", onto)
	if err != nil {
		return "", nil, err
	}
	// A two-tree read-tree checks the commit out over head, refusing to overwrite a change of the worktree's own.
	if _, err := l.git(ctx, "read-tree", "-m", "-u", head, landing); err != nil {
		return "", nil, err
	}
	if _, err := l.git(ctx, "update-ref", "-m", "land: squash onto "+l.cfg.Trunk, "HEAD", landing, head); err != nil {
		return "", nil, err
	}
	l.squashed, l.amended = landing, ""
	l.say("squashed the work since %s into %s on %s %s", short(base), short(landing), l.cfg.Trunk, short(onto))
	return landing, conflicted, nil
}

// reload reads the landing commit's own .land.yaml, which squash checked out, so that its regenerations, the prepare
// and environment of the box that checks it, and its checks are the ones the commit pushed asks for, as land recheck
// would have them. The landing keeps the trunk it fetched and rebased onto.
func (l *lander) reload() error {
	cfg, err := config.Load(l.root)
	if err != nil {
		return fmt.Errorf("the landing commit's configuration: %w; nothing was pushed", err)
	}
	cfg.Trunk = l.cfg.Trunk
	l.cfg = cfg
	return nil
}

// unamend returns stop, why the landing stopped with nothing pushed, after putting HEAD and the worktree back at the
// commit squash wrote when regenerate amended it: the next landing's change is the reviewed one again, its review key
// unchanged, and it regenerates afresh. A HEAD that moved since the amend is left alone.
func (l *lander) unamend(ctx context.Context, stop error) error {
	if l.amended == "" {
		return stop
	}
	ctx = context.WithoutCancel(ctx) // an interrupted landing is put back too
	head, err := l.git(ctx, "rev-parse", "HEAD")
	if err != nil || head != l.amended {
		return errors.Join(stop, err)
	}
	// A two-tree read-tree checks the squashed commit out over the amended one, as squash does over the work.
	if _, err := l.git(ctx, "read-tree", "-m", "-u", l.amended, l.squashed); err != nil {
		return errors.Join(stop, err)
	}
	if _, err := l.git(ctx, "update-ref", "-m", "land: undo the regeneration", "HEAD", l.squashed, l.amended); err != nil {
		return errors.Join(stop, err)
	}
	l.say("put HEAD back at %s, as squashed, before the regeneration", short(l.squashed))
	l.amended = ""
	return stop
}

// takeTrunk is tree, a merge's, with each of paths as onto, the trunk, holds it, or gone where onto has none. It
// builds the tree in an index of its own, so the worktree's index is untouched.
func (l *lander) takeTrunk(ctx context.Context, tree, onto string, paths []string) (string, error) {
	dir, err := os.MkdirTemp("", "land-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := l.gitEnv(ctx, index, "", "read-tree", tree); err != nil {
		return "", err
	}
	if _, err := l.gitEnv(ctx, index, "", append([]string{"update-index", "--force-remove", "--"}, paths...)...); err != nil {
		return "", err
	}
	theirs, err := l.git(ctx, append([]string{"ls-tree", "-r", "-z", onto, "--"}, paths...)...)
	if err != nil {
		return "", err
	}
	if _, err := l.gitEnv(ctx, index, theirs, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	return l.gitEnv(ctx, index, "", "write-tree")
}

// regenerate reruns, in one command on the worktree's box, the regenerate entries of the lockfiles landing changes
// against onto and of the derived files it changes or that conflicted, lockfiles first; fetches what they wrote back;
// and amends it into landing, returning the commit to check. With no entry to rerun, it returns landing.
func (l *lander) regenerate(ctx context.Context, onto, landing, message string, conflicted []string) (string, error) {
	changed, err := l.changed(ctx, onto, landing)
	if err != nil {
		return "", err
	}
	entries := classify.Regenerations(l.cfg.Stages, changed, conflicted)
	if len(entries) == 0 {
		return landing, nil
	}
	argv := []string{"bash", "-c", regenerateScript, "land"}
	var fetch []string
	for _, e := range entries {
		argv = append(argv, e.Path, e.Run)
		fetch = append(fetch, strings.TrimSuffix(e.Path, "/"))
	}
	box, err := l.claim(ctx, l.root, l.cfg, "")
	if err != nil {
		return "", fmt.Errorf("claiming the worktree's box failed, so nothing was pushed: %w", err)
	}
	l.say("regenerating %s on the box", strings.Join(fetch, ", "))
	if err := box.Run(ctx, fetch, argv...); err != nil {
		if putErr := l.putBack(ctx, fetch); putErr != nil {
			return "", errors.Join(err, putErr)
		}
		return "", fmt.Errorf("regenerating %s on the box failed, so nothing was pushed: %w; fix it, then land again",
			strings.Join(fetch, ", "), err)
	}
	// The worktree was clean at landing, so everything it now changes came back from the box.
	if _, err := l.git(ctx, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := l.git(ctx, "write-tree")
	if err != nil {
		return "", err
	}
	if old, err := l.git(ctx, "rev-parse", landing+"^{tree}"); err != nil || old == tree {
		l.say("the regenerated files match %s", short(landing))
		return landing, err
	}
	amended, err := l.gitInput(ctx, message, "commit-tree", tree, "-p", onto)
	if err != nil {
		return "", err
	}
	if _, err := l.git(ctx, "update-ref", "-m", "land: regenerate", "HEAD", amended, landing); err != nil {
		return "", err
	}
	l.amended = amended
	l.say("amended the regenerated files into %s", short(amended))
	return amended, nil
}

// putBack puts paths, files or directories, back as the index holds them, removing whatever else is there.
func (l *lander) putBack(ctx context.Context, paths []string) error {
	for _, path := range paths {
		if err := os.RemoveAll(filepath.Join(l.root, path)); err != nil {
			return err
		}
	}
	files, err := l.git(ctx, append([]string{"--literal-pathspecs", "ls-files", "-z", "--"}, paths...)...)
	if err != nil || files == "" {
		return err
	}
	_, err = l.gitInput(ctx, files, "checkout-index", "-f", "-z", "--stdin")
	return err
}

// regenerateScript runs on the box with each regenerate entry's path and command as its arguments, in order, each
// command under bash -eo pipefail -c, stopping at the first that fails.
const regenerateScript = `set -u
while [ $# -ge 2 ]; do
	code=0
	bash -eo pipefail -c "$2" land < /dev/null || code=$?
	if [ "$code" != 0 ]; then
		echo "land: regenerating $1 failed with exit $code" >&2
		exit "$code"
	fi
	echo "land: regenerated $1" >&2
	shift 2
done
`

// changed are the paths that differ between the commits from and to.
func (l *lander) changed(ctx context.Context, from, to string) ([]string, error) {
	out, err := l.git(ctx, "diff-tree", "-r", "-z", "--name-only", "--no-renames", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.Trim(out, "\x00"), "\x00"), nil
}

// check runs the check at the push of landing on the worktree's box, at its exact tree, and returns its outcome. For a
// failure, or a box that stopped before a verdict, it returns an error too, saying nothing was pushed, which is what a
// landing that hasn't pushed returns. An error with no outcome is a local one, or ctx ending the check: no verdict.
func (l *lander) check(ctx context.Context, landing string) (check.Outcome, error) {
	tree, err := l.git(ctx, "rev-parse", landing+"^{tree}")
	if err != nil {
		return check.Outcome{}, err
	}
	box, err := l.claim(ctx, l.root, l.cfg, "")
	if err != nil {
		return check.Stopped("claim", err.Error()), fmt.Errorf("claiming the worktree's box failed, so nothing was pushed: %w", err)
	}
	l.say("checking %s on the box", short(landing))
	// A root commit, which recheck can be asked about, changes everything against the empty tree.
	parent := landing + "^"
	if _, err := l.git(ctx, "rev-parse", "--verify", "--quiet", parent); err != nil {
		if parent, err = l.git(ctx, "hash-object", "-t", "tree", "/dev/null"); err != nil {
			return check.Outcome{}, err
		}
	}
	changed, err := l.changed(ctx, parent, landing)
	if err != nil {
		return check.Outcome{}, err
	}
	verdict, err := check.Run(ctx, box, l.root, tree, classify.Checks(l.cfg.Stages, changed))
	if err != nil {
		return check.Outcome{}, err
	}
	switch verdict.State {
	case forge.Success:
		return verdict, nil
	case forge.Failure:
		var names []string
		for _, c := range verdict.Failed {
			names = append(names, c.Name)
			l.say("check %s failed: `%s` exited %d; the end of its log, %s:\n%s", c.Name, c.Command, c.Exit, c.Log, c.Tail)
		}
		return verdict, fmt.Errorf("the check at the push failed in %s, so nothing was pushed: fix it, then land again",
			strings.Join(names, ", "))
	default:
		return verdict, fmt.Errorf("the box stopped at %s before a verdict, so nothing was pushed: %s; land again",
			verdict.Stage, verdict.Detail)
	}
}

// git runs git at the worktree's root and returns its output, trimmed.
func (l *lander) git(ctx context.Context, args ...string) (string, error) {
	return l.gitInput(ctx, "", args...)
}

// gitInput runs git at the worktree's root with stdin as its input and returns its output, trimmed, even when it
// fails; its error carries git's stderr.
func (l *lander) gitInput(ctx context.Context, stdin string, args ...string) (string, error) {
	return l.gitEnv(ctx, nil, stdin, args...)
}

// gitEnv is gitInput with env, KEY=value, added to land's environment.
func (l *lander) gitEnv(ctx context.Context, env []string, stdin string, args ...string) (string, error) {
	var stderr strings.Builder
	if env != nil {
		env = append(os.Environ(), env...)
	}
	out, err := proc.Output(ctx, proc.Spec{Argv: append([]string{"git"}, args...), Dir: l.root, Env: env, Stdin: strings.NewReader(stdin), Stderr: &stderr})
	if err != nil {
		err = fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out), err
}

// say prints one line of the landing's progress.
func (l *lander) say(format string, args ...any) {
	_, _ = fmt.Fprintf(l.out, "land: "+format+"\n", args...)
}

func short(sha string) string { return sha[:min(12, len(sha))] }
