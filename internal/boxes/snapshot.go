package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bytes032/land/internal/stage"
)

const (
	// snapshotLabel labels the repository's snapshots: land=<owner>.<name>.
	// shortcut: Hetzner refuses label values over 63 characters, so a longer <owner>.<name> gets no snapshot; hash
	// the name into the value once a repository's is that long.
	snapshotLabel = "land"
	// snapshotAge is how old the repository's newest snapshot gets before a spare whose warm-up passed replaces it.
	snapshotAge = 4 * time.Hour
)

// startable is the snapshot new boxes start from: the repository's newest one Hetzner has finished, never one still
// being created. With none, no box starts: one from a bare image would fail every check.
func (p *pool) startable(ctx context.Context) (image, error) {
	snapshots, err := p.hz.snapshots(ctx, p.repo)
	if err != nil {
		return image{}, err
	}
	for _, s := range snapshots {
		if s.Status == "available" {
			return s, nil
		}
	}
	return image{}, fmt.Errorf("%s has no snapshot to start a box from: `land boxes snapshot` builds one", p.repo)
}

// stale reports whether a spare whose warm-up passed at commit should replace the repository's snapshots, the newest
// first. Failed ones don't count; of the others, the newest decides: one still being made is the newest one coming,
// and one made is stale when it is of another commit and at least snapshotAge old. With none, any snapshot will do.
func stale(snapshots []image, commit string, now time.Time) bool {
	for _, s := range snapshots {
		switch s.Status {
		case "creating":
			return false
		case "available":
			return s.Labels["commit"] != commit && now.Sub(s.Created) >= snapshotAge
		}
	}
	return true
}

// image snapshots the server, labelled with the repository and commit, waits until Hetzner has made it, and only then
// deletes the repository's older snapshots, so that there is always one to start a box from.
func (p *pool) image(ctx context.Context, server int64, commit string) (image, error) {
	began := p.now()
	var created struct {
		Image image `json:"image"`
	}
	body := map[string]any{"type": "snapshot", "description": fmt.Sprintf("land box of %s at %.12s", p.repo, commit),
		"labels": map[string]string{snapshotLabel: p.repo, "commit": commit}}
	if err := p.hz.do(ctx, http.MethodPost, fmt.Sprintf("/servers/%d/actions/create_image", server), body, &created); err != nil {
		return image{}, err
	}
	made := created.Image
	for made.Status == "creating" {
		if p.now().Sub(began) > time.Hour {
			return image{}, fmt.Errorf("snapshot %d is still being made after an hour", made.ID)
		}
		if err := p.sleep(ctx, 5*time.Second); err != nil {
			return image{}, err
		}
		var err error
		if made, err = p.hz.image(ctx, made.ID); err != nil {
			return image{}, err
		}
	}
	if made.Status != "available" {
		return image{}, fmt.Errorf("hetzner failed snapshot %d of %s: it is %s", made.ID, p.repo, made.Status)
	}
	fmt.Fprintf(p.stderr, "land: snapshot %d of %s at %.12s took %s\n", made.ID, p.repo, commit, p.now().Sub(began).Round(time.Second))
	snapshots, err := p.hz.snapshots(ctx, p.repo)
	if err != nil {
		return made, err
	}
	var errs []error
	for _, old := range snapshots {
		// One still being created is another's newer snapshot.
		if old.ID != made.ID && old.Status == "available" && old.Created.Before(made.Created) {
			fmt.Fprintf(p.stderr, "land: deleting the older snapshot %d\n", old.ID)
			errs = append(errs, p.hz.do(ctx, http.MethodDelete, fmt.Sprintf("/images/%d", old.ID), nil, nil))
		}
	}
	return made, errors.Join(errs...)
}

// prepareImage readies a box built for a snapshot: no self-delete, nothing a new box shouldn't inherit (the builder's
// lease expiry and authorized keys), and cloud-init to run again on each new box's first boot, with its own key and
// SSH host keys.
func prepareImage() string {
	return stripSelfDelete() + `
sudo rm -f /etc/crabbox/lease-expiry /root/.ssh/authorized_keys
rm -f ~/.ssh/authorized_keys
sudo cloud-init clean --logs
sync`
}

// snapshot is `land boxes snapshot`: it builds the repository's snapshot at the trunk's green commit, or its tip when
// none is green, or with head at the worktree's HEAD commit, whatever the worktree holds, with that commit's own
// configuration, in a temporary checkout of it in the folder of the box it builds on, which goes after with the box,
// whatever happens.
func (p *pool) snapshot(ctx context.Context, head bool) error {
	at := p.green
	if head {
		at = p.head
	}
	commit, err := at(ctx)
	if err != nil {
		return err
	}
	id, unlock, err := p.newBox()
	if err != nil {
		return err
	}
	defer unlock() // until then, no other box command touches the box
	c := claim{Box: id}
	defer func() {
		if err := p.drop(ctx, c); err != nil {
			fmt.Fprintf(p.stderr, "land: deleting %s, the snapshot's box: %v\n", cmp.Or(c.Lease, "land-"+id), err)
		}
	}()
	return p.checkout(ctx, id, commit, func(q *pool) error {
		made, err := p.create(ctx, id, q.cfg.Box.Type, func(context.Context) (string, error) { return "", nil }, true)
		if err != nil {
			return err
		}
		c = made
		return q.buildSnapshot(ctx, c, commit)
	})
}

// buildSnapshot builds the repository's snapshot of commit, the pool's checkout, on c, a new box from crabbox's plain
// Ubuntu: it runs setup, then prepare and warm in the box script, readies the box (prepareImage) and images it.
func (p *pool) buildSnapshot(ctx context.Context, c claim, commit string) error {
	b := &box{p, c}
	steps := []struct {
		name, prepare, command string
	}{
		{"setup", "", p.cfg.Stages.Setup},
		{"prepare and warm", p.cfg.Stages.Prepare, p.cfg.Stages.Warm},
	}
	for _, step := range steps {
		began := p.now()
		argv := stage.Argv(step.command, nil)
		if step.command == "" {
			argv = []string{"true"}
		}
		if err := b.run(ctx, step.prepare, nil, argv...); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		fmt.Fprintf(p.stderr, "land: %s took %s\n", step.name, p.now().Sub(began).Round(time.Second))
	}
	ssh, err := p.ssh(ctx, c)
	if err != nil {
		return err
	}
	if _, err := p.onBox(ctx, ssh, prepareImage(), ""); err != nil {
		return err
	}
	_, err = p.image(ctx, c.Server, commit)
	return err
}
