package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/verdict"
)

const (
	// spareMargin is how close to its deadline a spare is no longer handed out, so it can't delete itself under a claim.
	spareMargin = 2 * time.Minute
	// leaseLeft is the least of its crabbox lease a spare leaves whoever claims it: crabbox powers a box off at the end
	// of its lease, lifetime after its creation, whoever holds it.
	leaseLeft = time.Hour
	// deadlineLabel is the server label holding a spare's deadline, in Unix seconds, which the sweep reads.
	deadlineLabel = "land-deadline"
)

// selfDeleteOf is what makes the self-delete of the box whose server is id.
func (p *pool) selfDeleteOf(ctx context.Context, id int64) (selfDelete, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return selfDelete{}, errors.New("HCLOUD_TOKEN, the Hetzner project's API token, isn't set")
	}
	s, err := p.hz.server(ctx, id)
	if err != nil {
		return selfDelete{}, err
	}
	keys, err := p.hz.keys(ctx, s)
	return selfDelete{token: token, created: s.Created, keys: keys}, err
}

// disarm readies the box c, which the worktree holds and which may still be a spare, for the worktree: it disarms the
// box before crabbox copies the worktree there, then removes its deadline label and, only then, the deadline from its
// record.
func (p *pool) disarm(ctx context.Context, c claim) error {
	if err := p.crabbox(ctx, c.Box, p.stderr, p.stderr, "run", "--id", c.Lease, "--reclaim", "--no-sync", "--", "bash", "-c", p.code.disarm()); err != nil {
		return err
	}
	if err := p.hz.label(ctx, c.Server, deadlineLabel, ""); err != nil {
		return err
	}
	return p.withState(func(s *state) error {
		if h := s.byLease(c.Lease); h != nil && h.Worktree == p.root {
			h.Deadline = time.Time{}
		}
		return nil
	})
}

// spare makes the box c, which the worktree releases, a spare: it sets its deadline, 5 minutes before its next paid
// hour, on the box and as its server's label, and, with a warm stage, starts its warm-up at the green commit, whose
// end a background `land boxes warm` waits for. It returns the deadline. A box with less time left, or one leaving less
// than leaseLeft of its lease after it, is no spare. The record holds the deadline before the box does, so that a
// release that stops halfway leaves the box to the next claim to clear (disarm).
func (p *pool) spare(ctx context.Context, c claim) (time.Time, error) {
	s, err := p.hz.server(ctx, c.Server)
	if err != nil {
		return time.Time{}, err
	}
	released := p.now()
	d := deadline(s.Created, released)
	switch end := s.Created.Add(lifetime); {
	case !d.After(released.Add(spareMargin)):
		return time.Time{}, fmt.Errorf("its paid hour ends at %s", d.Add(paidLead).Local().Format(time.TimeOnly))
	case end.Sub(d) < leaseLeft:
		return time.Time{}, fmt.Errorf("its lease ends at %s", end.Local().Format(time.TimeOnly))
	}
	if err := p.withState(func(s *state) error {
		if h := s.byLease(c.Lease); h != nil && h.Worktree == p.root {
			h.Deadline = d
		}
		return nil
	}); err != nil {
		return time.Time{}, err
	}
	unix := strconv.FormatInt(d.Unix(), 10)
	commit := ""
	if p.cfg.Stages.Warm != "" {
		if commit, err = p.green(ctx); err != nil {
			fmt.Fprintf(p.stderr, "land: %s won't warm: the green commit: %v\n", c.Lease, err)
		}
	}
	ssh, refresh := "", false
	if commit != "" {
		if err := p.checkout(ctx, c.Box, commit, func(q *pool) error {
			// A spare of the configured type has the snapshot's disk, so its snapshot starts every new box.
			refresh = c.Type == q.cfg.Box.Type
			var err error
			ssh, err = q.startWarm(ctx, c, commit, unix)
			return err
		}); err != nil {
			return time.Time{}, err
		}
	} else if err := p.crabbox(ctx, c.Box, p.stderr, p.stderr, "run", "--id", c.Lease, "--reclaim", "--no-sync", "--",
		"bash", "-c", p.code.arm(d)); err != nil {
		return time.Time{}, err
	}
	if err := p.hz.label(ctx, c.Server, deadlineLabel, unix); err != nil {
		return time.Time{}, err
	}
	if err := p.withState(func(s *state) error {
		if h := s.byLease(c.Lease); h != nil && h.Worktree == p.root {
			*h = claim{Box: h.Box, Lease: h.Lease, Slug: h.Slug, Type: h.Type, Server: h.Server, Released: released, Deadline: d}
		}
		return nil
	}); err != nil {
		return time.Time{}, err
	}
	if ssh != "" {
		if err := p.background(p.folder(c.Box), "warm", p.repo, c.Lease, released.Format(time.RFC3339Nano), commit,
			strconv.FormatBool(refresh), ssh); err != nil {
			fmt.Fprintf(p.stderr, "land: %s won't refresh the snapshot: %v\n", c.Lease, err)
		}
	}
	return d, nil
}

// green is the commit spares warm and snapshots are built at: the trunk's green commit, or its tip when none is green.
func (p *pool) green(ctx context.Context) (string, error) {
	tip, err := p.cfg.FetchTrunk(ctx, p.root)
	if err != nil {
		return "", err
	}
	f, err := p.forge(ctx, p.root)
	if err != nil {
		return "", err
	}
	sha, _, err := verdict.GreenCommit(ctx, f, p.root, tip)
	return sha, err
}

// head is the commit `land boxes snapshot --head` builds at: the worktree's HEAD, which must hold a .land.yaml.
func (p *pool) head(ctx context.Context) (string, error) {
	commit, err := p.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if listed, err := p.git(ctx, "ls-tree", "--name-only", commit, "--", config.File); err != nil || listed == "" {
		return "", cmp.Or(err, fmt.Errorf("HEAD, %.12s, has no committed %s to build the snapshot with", commit, config.File))
	}
	return commit, nil
}

// checkout runs f on the pool as a temporary checkout of commit sees it, with that commit's own configuration: a
// worktree of the repository in the box's folder, named as crabbox names the repository's workspace, which it removes
// after; one an earlier checkout left there goes first.
func (p *pool) checkout(ctx context.Context, box, commit string, f func(q *pool) error) error {
	_, name, _ := strings.Cut(p.repo, ".")
	dir := filepath.Join(p.folder(box), "checkout", name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// --force takes the place of a worktree git still records there.
	if _, err := p.git(ctx, "worktree", "add", "--quiet", "--force", "--detach", dir, commit); err != nil {
		return err
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		if _, err := os.Stat(dir); err != nil { // ended with the box's folder, which held it: git only needs to forget it
			_, _ = p.git(ctx, "worktree", "prune")
			return
		}
		_, _ = p.git(ctx, "worktree", "remove", "--force", dir)
	}()
	cfg, err := config.Load(dir)
	if err != nil {
		return fmt.Errorf("the configuration at %.12s: %w", commit, err)
	}
	q := *p
	q.root, q.cfg = dir, cfg
	return f(&q)
}

// startWarm copies the pool's checkout of commit to the box c and starts its warm-up there (warmStart), with the
// commit's own prepare, warm and box.env, after setting the box's deadline, unix. It returns the box's SSH command,
// which outlives the checkout, crabbox's claim of the lease names.
func (p *pool) startWarm(ctx context.Context, c claim, commit, unix string) (string, error) {
	fmt.Fprintf(p.stderr, "land: warming %s at %.12s\n", c.Lease, commit)
	start := p.code.warmStart(p.cfg.Box.Env, p.cfg.Stages.Prepare, p.cfg.Stages.Warm)
	if err := p.crabboxIn(ctx, c.Box, strings.NewReader(start), p.stderr, p.stderr,
		"run", "--id", c.Lease, "--reclaim", "--script-stdin", "--", unix); err != nil {
		return "", err
	}
	return p.ssh(ctx, c)
}

// background starts `land boxes ARGS...` in dir, a box's folder, in a session of its own, its output in dir's
// warm.log, and returns without waiting for it.
func background(dir string, args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(dir, "warm.log")) // each warm-up's log replaces the last one's
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }() // the child holds its own descriptor
	_, err = proc.Start(context.Background(), proc.Spec{Argv: append([]string{self, "boxes"}, args...), Dir: dir, Stdout: log, Stderr: log})
	return err
}

const warmUsage = "usage: land boxes warm REPO LEASE RELEASED COMMIT REFRESH SSH (release starts it)"

// warmMain is `land boxes warm REPO LEASE RELEASED COMMIT REFRESH SSH`, the background half of a release, on boxes
// whose box code box gives.
func warmMain(ctx context.Context, args []string, box func(repo string) boxCode) error {
	if len(args) != 6 {
		return errors.New(warmUsage)
	}
	released, err := time.Parse(time.RFC3339Nano, args[2])
	if err != nil {
		return err
	}
	refresh, err := strconv.ParseBool(args[4])
	if err != nil {
		return err
	}
	p, err := poolOf(ctx, args[0], "", nil, box)
	if err != nil {
		return err
	}
	return p.warm(ctx, args[1], released, args[3], refresh, args[5])
}

// warm waits, over ssh, for the warm-up of the spare with lease, released at released, to commit. When it passed, the
// spare is warm, which a claim takes first, and when refresh is true, no other box is being imaged and the repository's
// snapshot is stale, the spare is imaged into its new snapshot (refresh). It touches no spare a claim took meanwhile.
func (p *pool) warm(ctx context.Context, lease string, released time.Time, commit string, refresh bool, ssh string) error {
	code, err := p.onBox(ctx, ssh, p.code.warmWait(), "")
	if err != nil {
		return fmt.Errorf("waiting for the warm-up of %s: %w", lease, err)
	}
	if code = strings.TrimSpace(code); code != "0" {
		fmt.Fprintf(p.stderr, "land: the warm-up of %s ended: %s\n", lease, cmp.Or(code, "stopped"))
		return nil
	}
	var c claim
	if err := p.withState(func(s *state) error {
		h := s.byLease(lease)
		if h == nil || h.Worktree != "" || !h.Released.Equal(released) {
			return nil // a claim took it, or it is gone
		}
		h.Warm = commit
		// An image that outlasted the deadline would keep the box into its next paid hour.
		if !refresh || h.Deadline.Before(p.now().Add(15*time.Minute)) || slices.ContainsFunc(s.Claims, func(c claim) bool { return c.Imaging }) {
			return nil
		}
		snapshots, err := p.hz.snapshots(ctx, p.repo)
		if err != nil || !stale(snapshots, commit, p.now()) {
			return err
		}
		h.Imaging = true
		c = *h
		return nil
	}); err != nil || !c.Imaging {
		fmt.Fprintf(p.stderr, "land: the warm-up of %s passed at %.12s\n", lease, commit)
		return err
	}
	err = p.refresh(ctx, c, commit, ssh)
	return errors.Join(err, p.withState(func(s *state) error {
		if h := s.byLease(lease); h != nil {
			h.Imaging = false
		}
		return nil
	}))
}

// refresh images the warm spare c, which no claim takes meanwhile, into the repository's snapshot of commit: without
// its self-delete, which comes back after with its deadline, whether or not the snapshot was made.
func (p *pool) refresh(ctx context.Context, c claim, commit, ssh string) error {
	fmt.Fprintf(p.stderr, "land: refreshing the snapshot from %s\n", c.Lease)
	_, err := p.onBox(ctx, ssh, p.code.strip(), "")
	if err == nil {
		_, err = p.image(ctx, c.Server, commit)
	}
	s, restoreErr := p.selfDeleteOf(context.WithoutCancel(ctx), c.Server)
	if restoreErr == nil {
		script, stdin := p.code.restore(s, c.Deadline)
		_, restoreErr = p.onBox(context.WithoutCancel(ctx), ssh, script, stdin)
	}
	return errors.Join(err, restoreErr)
}
