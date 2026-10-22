package boxes

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/proc"
)

const (
	// lifetime is a box's crabbox lease, and so its server's longest life; crabbox powers it off once it passes.
	lifetime = 24 * time.Hour
	// How often and how long a claim asks again for a new box while Hetzner refuses one at the project's limit.
	retryEvery = 30 * time.Second
	retryFor   = 3 * time.Hour
)

// leaseTTL is lifetime as crabbox takes a lease's length: 24h.
var leaseTTL = strconv.Itoa(int(lifetime.Hours())) + "h"

// land runs crabbox from minCrabbox up to, not including, maxCrabbox: another version may keep a box's files elsewhere
// than its folder. Widen it only after `go test -tags crabboxreal ./internal/boxes/` passes on the new version.
const minCrabbox, maxCrabbox = "0.78.1", "0.80.0"

// boxRoot holds every repository's build root on a box, /work/<owner>.<name>; tests move it to a directory of theirs.
var boxRoot = "/work"

// pool is the boxes of one repository, as one of its worktrees sees them.
type pool struct {
	root  string // the worktree's root; "" in release's background warm-up, which no worktree runs
	cfg   *config.Config
	repo  string // <owner>.<name>
	dir   string // the claims and the boxes' folders: boxes/ in the repository's state directory
	build string // the build root on every box, where crabbox's workspace for the lease links to
	hz    hetzner
	// stdout carries the box command's own output; stderr everything land and crabbox say.
	stdout, stderr io.Writer
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
	forge          func(context.Context, string) (forge.Forge, error) // which names the green commit
	// background starts `land boxes ARGS...` in dir without waiting for it: release's warm-up.
	background func(dir string, args ...string) error
}

func newPool(ctx context.Context, root string, cfg *config.Config) (*pool, error) {
	repo, err := config.RepoName(ctx, root)
	if err != nil {
		return nil, err
	}
	return poolOf(ctx, repo, root, cfg)
}

// poolOf is the pool of the repository repo as the worktree at root sees it, once crabbox is a version land runs.
func poolOf(ctx context.Context, repo, root string, cfg *config.Config) (*pool, error) {
	if err := checkCrabbox(ctx); err != nil {
		return nil, err
	}
	state, err := config.StateDir(repo)
	if err != nil {
		return nil, err
	}
	return &pool{
		root: root, cfg: cfg, repo: repo, dir: filepath.Join(state, "boxes"), build: path.Join(boxRoot, repo),
		stdout: os.Stdout, stderr: os.Stderr, now: time.Now, sleep: sleep, forge: forge.FromEnv, background: background,
	}, nil
}

// checkCrabbox refuses a crabbox outside [minCrabbox, maxCrabbox).
func checkCrabbox(ctx context.Context) error {
	out, err := proc.Output(ctx, proc.Spec{Argv: []string{"crabbox", "--version"}, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	v := strings.TrimSpace(out)
	if n := version(v); n == nil || slices.Compare(n, version(minCrabbox)) < 0 || slices.Compare(n, version(maxCrabbox)) >= 0 {
		return fmt.Errorf("crabbox %s is installed, and land runs crabbox %s up to, not including, %s", v, minCrabbox, maxCrabbox)
	}
	return nil
}

// version is the numbers of v, a major.minor.patch version, or nil.
func version(v string) []int {
	var numbers []int
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		numbers = append(numbers, n)
	}
	if len(numbers) != 3 {
		return nil
	}
	return numbers
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// claim is one of the repository's boxes: the worktree that holds it, or none, a spare.
type claim struct {
	Box      string `json:"box"` // the box's ID, which names its folder and, land-<Box>, its slug
	Worktree string `json:"worktree,omitempty"`
	// Lease is crabbox's lease ID, cbx_<12 hex>, or "" while the box is being created: pending, which no claim takes.
	Lease  string `json:"lease"`
	Slug   string `json:"slug"`
	Type   string `json:"type"`
	Server int64  `json:"server"` // the Hetzner server's ID
	// Snapshot is a box being built into a snapshot, which no claim takes, and which goes with its build.
	Snapshot bool `json:"snapshot,omitempty"`
	// A spare's: when its worktree released it, the commit its warm-up passed at, and whether it is being imaged,
	// which no claim takes it during.
	Released time.Time `json:"released,omitzero"`
	Warm     string    `json:"warm,omitempty"`
	Imaging  bool      `json:"imaging,omitempty"`
	// Deadline is when the box deletes itself: a spare's, or, on a box a worktree holds, one the box may still have,
	// which a takeover or a release didn't finish clearing or setting, and the next claim clears (disarm).
	Deadline time.Time `json:"deadline,omitzero"`
}

type state struct {
	Claims []claim `json:"claims"`
}

// withState runs f on the claims under their lock and saves what f leaves, even when f fails: its changes mirror what
// it already did to the boxes. The lock dies with the process, so it is never left stale.
func (p *pool) withState(f func(*state) error) error {
	unlock, err := lock(filepath.Join(p.dir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	file := filepath.Join(p.dir, "claims.json")
	var s state
	switch data, err := os.ReadFile(file); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		for _, c := range s.Claims {
			if !boxID.MatchString(c.Box) { // a box's ID names a folder that its end deletes
				return fmt.Errorf("%s: %s names no box folder, as an older land's claims don't: delete its server, then the file",
					file, cmp.Or(c.Lease, "a claim"))
			}
		}
	}
	ferr := f(&s)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errors.Join(ferr, err)
	}
	// Rename replaces the file whole, so a crash never leaves half the claims.
	if err := os.WriteFile(file+".new", append(data, '\n'), 0o600); err != nil {
		return errors.Join(ferr, err)
	}
	return errors.Join(ferr, os.Rename(file+".new", file))
}

// lock takes the lock file path, waiting for its holder, until unlock or the process's end.
func lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

// free reports whether no process holds the lock file path, a missing one included. Closing the file releases the lock
// free takes to tell.
func free(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil //nolint:gosec // a file descriptor fits an int
}

// boxID is a box's ID, 12 random hex digits.
var boxID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// folder is the box's folder, which holds all the box makes on the Mac, and goes with it (end): crabbox's state,
// state/, and temporary files, tmp/; the lock its creator holds, lock; its warm-up's log, warm.log; and a checkout of
// the repository, checkout/, that a release's warm-up or a snapshot's build makes.
func (p *pool) folder(box string) string { return filepath.Join(p.dir, box) }

// newBox makes a new box's folder and takes its lock, which keeps every box command off the box until unlock: under
// the claims' lock, so that none finds the folder without it.
func (p *pool) newBox() (box string, unlock func(), err error) {
	err = p.withState(func(*state) error {
		var id [6]byte
		_, _ = rand.Read(id[:]) // never fails
		box = hex.EncodeToString(id[:])
		for _, dir := range []string{"", "state", "tmp"} {
			if err := os.Mkdir(filepath.Join(p.folder(box), dir), 0o700); err != nil {
				return err
			}
		}
		u, err := lock(filepath.Join(p.folder(box), "lock"))
		unlock = u
		return err
	})
	return box, unlock, err
}

// locked runs f on the claims under their lock, once it has ended the boxes that should be gone: a box whose server is
// gone, a spare that deleted itself among them; one crabbox powered off at the end of its lease; one whose worktree is
// gone; one being created, or built into a snapshot, whose land command is gone, its folder's lock free; and a folder
// no claim names, whose lock is free. It removes the claim locks of gone worktrees too. f gets the status of each
// crabbox server in the project, by lease.
func (p *pool) locked(ctx context.Context, f func(s *state, live map[string]string) error) error {
	return p.withState(func(s *state) error {
		live := map[string]string{}
		if len(s.Claims) > 0 {
			var err error
			if live, err = p.servers(ctx); err != nil {
				return err
			}
		}
		kept := s.Claims[:0]
		for _, c := range s.Claims {
			status, up := live[c.Lease]
			why := ""
			switch {
			case c.Lease == "" || c.Snapshot:
				if !free(filepath.Join(p.folder(c.Box), "lock")) {
					kept = append(kept, c)
					continue
				}
				why = "the land command making it is gone"
			case !up:
				why = "its server is gone"
			case status == "off": // Hetzner bills an off server all the same
				why = "crabbox powered it off at the end of its lease"
			case c.Worktree != "" && gone(c.Worktree):
				why = "its worktree " + c.Worktree + " is gone"
			default:
				kept = append(kept, c)
				continue
			}
			if !p.ended(ctx, c, why) {
				kept = append(kept, c)
			}
		}
		s.Claims = kept
		entries, err := os.ReadDir(p.dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name, file := e.Name(), filepath.Join(p.dir, e.Name())
			switch {
			case e.IsDir() && boxID.MatchString(name):
				if !slices.ContainsFunc(s.Claims, func(c claim) bool { return c.Box == name }) && free(filepath.Join(file, "lock")) {
					p.ended(ctx, claim{Box: name, Slug: "land-" + name}, "no claim names it")
				}
			case strings.HasSuffix(name, ".claiming"):
				if worktree, err := os.ReadFile(file); err == nil && gone(string(worktree)) && free(file) {
					_ = os.Remove(file) // or the next box command's
				}
			}
		}
		return f(s, live)
	})
}

// ended ends the box c, as locked does, saying why, and reports whether it did.
func (p *pool) ended(ctx context.Context, c claim, why string) bool {
	if err := p.end(ctx, c); err != nil {
		fmt.Fprintf(p.stderr, "land: deleting %s (%s): %v; the next box command tries again\n", cmp.Or(c.Lease, c.Slug), why, err)
		return false
	}
	fmt.Fprintf(p.stderr, "land: deleted %s: %s\n", cmp.Or(c.Lease, c.Slug), why)
	return true
}

func gone(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, fs.ErrNotExist)
}

// held is the box the worktree holds, or nil; never a pending one.
func (s *state) held(worktree string) *claim {
	if i := slices.IndexFunc(s.Claims, func(c claim) bool { return worktree != "" && c.Worktree == worktree && c.Lease != "" }); i >= 0 {
		return &s.Claims[i]
	}
	return nil
}

// byLease is the box with lease, or nil.
func (s *state) byLease(lease string) *claim {
	if i := slices.IndexFunc(s.Claims, func(c claim) bool { return c.Lease == lease }); i >= 0 {
		return &s.Claims[i]
	}
	return nil
}

// spare is the spare of typ a claim takes, warm before not, then the one with the most time left; never one being
// imaged, nor one about to delete itself, nor a pending box or a snapshot's.
func (s *state) spare(typ string, now time.Time) *claim {
	var best *claim
	for i := range s.Claims {
		c := &s.Claims[i]
		if c.Worktree != "" || c.Lease == "" || c.Snapshot || c.Type != typ || c.Imaging || !c.Deadline.After(now.Add(spareMargin)) {
			continue
		}
		if best == nil || c.Warm != "" && best.Warm == "" ||
			(c.Warm != "") == (best.Warm != "") && c.Deadline.After(best.Deadline) {
			best = c
		}
	}
	return best
}

// claim returns the worktree's box: the one it holds, else a spare, else a new box from the repository's newest
// snapshot, the last two of typ, or box.type when typ is "". A worktree holding a box of another type than a typ it
// asks for is refused, so that it never holds two boxes. A box it holds whose deadline may still be armed, a spare it
// just took or a box a takeover or release left so, is disarmed first, or deleted, so that no box deletes itself
// under its worktree.
func (p *pool) claim(ctx context.Context, typ string) (*box, error) {
	want := cmp.Or(typ, p.cfg.Box.Type)
	if want == "" {
		return nil, errors.New("no box type: set box.type in .land.yaml") // else crabbox picks its own, up to a ccx63
	}
	if err := p.ignoreCaptures(ctx); err != nil {
		return nil, err
	}
	// One claim of the worktree at a time, so that two box commands started at once never get two boxes. The lock
	// names its worktree, so that locked removes it once the worktree is gone.
	claiming := filepath.Join(p.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(p.root)))[:16]+".claiming")
	unlock, err := lock(claiming)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := os.WriteFile(claiming, []byte(p.root), 0o600); err != nil {
		return nil, err
	}
	for {
		var held claim
		if err := p.locked(ctx, func(s *state, _ map[string]string) error {
			h := s.held(p.root)
			if h == nil {
				if h = s.spare(want, p.now()); h != nil {
					// The worktree's from now on, its deadline recorded until the box no longer has it.
					*h = claim{Box: h.Box, Worktree: p.root, Lease: h.Lease, Slug: h.Slug, Type: h.Type, Server: h.Server, Deadline: h.Deadline}
				}
			}
			if h != nil {
				held = *h
			}
			return nil
		}); err != nil {
			return nil, err
		}
		switch {
		case held.Lease == "":
			return p.claimNew(ctx, want)
		case typ != "" && held.Type != typ:
			return nil, fmt.Errorf("this worktree holds %s, a %s: `land boxes release` it before asking for a %s", held.Lease, held.Type, typ)
		case held.Deadline.IsZero():
			return &box{p, held}, nil
		}
		err := p.disarm(ctx, held)
		if err == nil {
			fmt.Fprintf(p.stderr, "land: took over %s (%s), no longer due to delete itself\n", held.Lease, held.Slug)
			held.Deadline = time.Time{}
			return &box{p, held}, nil
		}
		fmt.Fprintf(p.stderr, "land: taking over %s: %v; deleting it\n", held.Lease, err)
		if err := p.drop(ctx, held); err != nil {
			return nil, err
		}
	}
}

// claimNew gives the worktree a new box of typ, from the repository's newest snapshot.
func (p *pool) claimNew(ctx context.Context, typ string) (*box, error) {
	id, unlock, err := p.newBox()
	if err != nil {
		return nil, err
	}
	defer unlock() // once create returns, the box's record is complete, or gone with the box
	c, err := p.create(ctx, id, typ, p.snapshotImage, false)
	if err != nil {
		return nil, err
	}
	return &box{p, c}, nil
}

// snapshotImage is the image new boxes start from, the repository's newest snapshot of the moment (startable).
func (p *pool) snapshotImage(ctx context.Context) (string, error) {
	snapshot, err := p.startable(ctx)
	return strconv.FormatInt(snapshot.ID, 10), err
}

// drop ends the box c and forgets it.
func (p *pool) drop(ctx context.Context, c claim) error {
	if err := p.end(context.WithoutCancel(ctx), c); err != nil {
		return err
	}
	return p.withState(func(s *state) error {
		s.Claims = slices.DeleteFunc(s.Claims, func(h claim) bool { return h.Box == c.Box })
		return nil
	})
}

// end deletes the box c and all it made, whatever is gone already: its server, by its ID or, for a box land has no
// server of, each crabbox server labelled with its slug; the SSH keys crabbox registered with Hetzner for it, by the
// fingerprints of the public keys in its folder; and its folder, and with it a checkout git then forgets.
func (p *pool) end(ctx context.Context, c claim) error {
	if !boxID.MatchString(c.Box) {
		return fmt.Errorf("%q is no box's ID", c.Box) // never let a name reach RemoveAll
	}
	servers := []int64{c.Server}
	if c.Server == 0 {
		var err error
		if servers, err = p.hz.slugged(ctx, "land-"+c.Box); err != nil {
			return err
		}
	}
	for _, id := range servers {
		if err := p.hz.do(ctx, http.MethodDelete, fmt.Sprintf("/servers/%d", id), nil, nil); err != nil && !notFound(err) {
			return err
		}
	}
	folder := p.folder(c.Box)
	if err := p.hz.deleteKeys(ctx, folder); err != nil {
		return err
	}
	_, err := os.Stat(filepath.Join(folder, "checkout"))
	checkout := err == nil
	if err := os.RemoveAll(folder); err != nil || !checkout {
		return err
	}
	_, err = p.git(ctx, "worktree", "prune") // git forgets a worktree once its directory is gone
	return err
}

// ignoreCaptures makes the worktree's repository ignore .crabbox/, where crabbox saves a bundle of each box command
// that fails, so that a failed command never leaves the worktree unclean.
func (p *pool) ignoreCaptures(ctx context.Context) error {
	exclude, err := p.git(ctx, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(p.root, exclude)
	}
	data, err := os.ReadFile(exclude)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case slices.Contains(strings.Split(string(data), "\n"), "/.crabbox/"):
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		return err
	}
	return os.WriteFile(exclude, append(data, "/.crabbox/\n"...), 0o644) //nolint:gosec // git's own file mode
}

// create creates the box box, whose new folder's lock the caller holds, of typ, from the image image names, a
// snapshot's ID, or "" for crabbox's plain Ubuntu: for the worktree, or, with snapshot, to build a snapshot on. The
// box's record comes first, pending, so that a box command finds whatever crabbox makes even if this one dies; it is
// complete once create returns, or gone, with all the box made. While Hetzner refuses the box at the project's limit,
// create asks again every retryEvery for up to retryFor, saying once what Hetzner said and who holds the repository's
// boxes, and naming the image again each time: a spare's refresh may have replaced the snapshot meanwhile.
func (p *pool) create(ctx context.Context, box, typ string, image func(context.Context) (string, error), snapshot bool) (c claim, err error) {
	c = claim{Box: box, Slug: "land-" + box, Type: typ, Snapshot: snapshot}
	if !snapshot {
		c.Worktree = p.root
	}
	if err := p.withState(func(s *state) error { s.Claims = append(s.Claims, c); return nil }); err != nil {
		return claim{}, err
	}
	defer func() {
		if err != nil {
			c, err = claim{}, errors.Join(err, p.drop(ctx, c))
		}
	}()
	start := p.now()
	for attempt := 0; ; attempt++ {
		img, err := image(ctx)
		if err != nil {
			return c, err
		}
		made, refusal, err := p.warmup(ctx, c, img)
		switch {
		case err != nil:
			return made, err
		case refusal == "":
			return made, p.withState(func(s *state) error {
				if i := slices.IndexFunc(s.Claims, func(h claim) bool { return h.Box == box }); i >= 0 {
					s.Claims[i] = made
				}
				return nil
			})
		}
		if attempt == 0 {
			var holders []string
			if err := p.withState(func(s *state) error {
				for _, h := range s.Claims {
					if h.Lease != "" {
						holders = append(holders, fmt.Sprintf("%s (%s, a %s) held by %s", h.Lease, h.Slug, h.Type, cmp.Or(h.Worktree, "none, a spare")))
					}
				}
				return nil
			}); err != nil {
				return c, err
			}
			fmt.Fprintf(p.stderr, "land: Hetzner refused a new %s box: %s\nland: this repository's boxes: %s\n"+
				"land: asking again every %s for up to %s\n", typ, refusal, cmp.Or(strings.Join(holders, "; "), "none"), retryEvery, retryFor)
		}
		if p.now().Sub(start) >= retryFor {
			return c, fmt.Errorf("hetzner refused a new %s box for %s: %s", typ, retryFor, refusal)
		}
		if err := p.sleep(ctx, retryEvery); err != nil {
			return c, err
		}
	}
}

var (
	leasedLine = regexp.MustCompile(`(?m)^leased (cbx_[0-9a-f]{12}) slug=(\S+) .*\bserver=(\d+)\b`)
	readyLine  = regexp.MustCompile(`(?m)^ready .*\bworkroot=(/\S+)`)
	// limitLine is crabbox's report of Hetzner refusing a server at one of the project's limits.
	limitLine = regexp.MustCompile(`.*\b(resource_limit_exceeded|dedicated_core_limit)\b.*`)
)

// warmup creates the pending box c from image with crabbox and readies it (setup), returning it with its lease, slug
// and server, those crabbox printed even when it fails. Hetzner refusing it at the project's limit is no error: warmup
// returns what Hetzner said.
func (p *pool) warmup(ctx context.Context, c claim, image string) (claim, string, error) {
	fmt.Fprintf(p.stderr, "land: creating a %s box\n", c.Type)
	var stdout, stderr bytes.Buffer
	err := proc.Run(ctx, p.crabboxSpec(c.Box, []string{"CRABBOX_HETZNER_IMAGE=" + image}, nil, &stdout, &stderr,
		[]string{"warmup", "--slug", c.Slug, "--type", c.Type, "--ttl", leaseTTL, "--idle-timeout", leaseTTL}))
	leased, ready := leasedLine.FindStringSubmatch(stdout.String()), readyLine.FindStringSubmatch(stdout.String())
	if leased != nil {
		server, _ := strconv.ParseInt(leased[3], 10, 64) // \d+ that crabbox printed
		c.Lease, c.Slug, c.Server = leased[1], leased[2], server
	}
	switch {
	case err != nil && limitLine.MatchString(stderr.String()):
		return c, strings.TrimSpace(limitLine.FindString(stderr.String())), nil
	case err == nil && (leased == nil || ready == nil):
		err = errors.New("crabbox warmup printed no leased or no ready line")
	}
	if err != nil {
		_, _ = p.stderr.Write(append(stdout.Bytes(), stderr.Bytes()...))
		return c, "", err
	}
	// crabbox's workspace for the lease is <workroot>/<lease>/<name>, name the origin's, the part of repo after its
	// owner, which on GitHub has no dot.
	_, name, _ := strings.Cut(p.repo, ".")
	if err := p.setup(ctx, c, path.Join(ready[1], c.Lease, name)); err != nil {
		return c, "", fmt.Errorf("readying %s: %w", c.Lease, err)
	}
	fmt.Fprintf(p.stderr, "land: created %s (%s)\n", c.Lease, c.Slug)
	return c, "", nil
}

// setup readies a new box: crabbox's workspace for its lease becomes a link to the build root, so that every box
// builds at the same path whatever its lease; the box drops the SSH keys of other crabbox leases, which a box from a
// snapshot of a spare inherits; and it gets its self-delete.
func (p *pool) setup(ctx context.Context, c claim, workspace string) error {
	ssh, err := p.ssh(ctx, c)
	if err != nil {
		return err
	}
	install, token, err := p.selfDelete(ctx, c.Server)
	if err != nil {
		return err
	}
	script := `set -eu
sudo install -d -o "$(id -un)" -g "$(id -gn)" ` + quote(p.build) + `
mkdir -p ` + quote(path.Dir(workspace)) + `
ln -s ` + quote(p.build) + ` ` + quote(workspace) + `
for keys in ~/.ssh/authorized_keys /root/.ssh/authorized_keys; do
  if sudo test -f "$keys"; then sudo sed -i -e '/ crabbox ` + c.Lease + `$/b' -e '/ crabbox cbx_[0-9a-f]\{12\}$/d' "$keys"; fi
done
` + install
	_, err = p.onBox(ctx, ssh, script, token)
	return err
}

// ssh is the SSH command crabbox prints for the box c, run in the pool's worktree, whose checkout crabbox's claim of
// the lease names.
func (p *pool) ssh(ctx context.Context, c claim) (string, error) {
	var printed bytes.Buffer
	if err := p.crabbox(ctx, c.Box, &printed, p.stderr, "ssh", "--id", c.Lease); err != nil {
		return "", err
	}
	ssh := strings.TrimSpace(printed.String())
	ssh = ssh[strings.LastIndexByte(ssh, '\n')+1:] // the command is crabbox's last line
	if ssh == "" {
		return "", errors.New("crabbox ssh printed no command") // bash would run the script here, on the Mac
	}
	return ssh, nil
}

// onBox runs script on the box over ssh, an SSH command crabbox printed, with stdin, and returns what it printed.
func (p *pool) onBox(ctx context.Context, ssh, script, stdin string) (string, error) {
	return proc.Output(ctx, proc.Spec{
		// bash reads crabbox's quoting of the command's words.
		Argv:  []string{"bash", "-c", ssh + ` "$1"`, "land", script},
		Stdin: strings.NewReader(stdin), Stderr: p.stderr,
	})
}

// release releases the worktree's box: it stays as a spare, warming at the green commit, until 5 minutes before its
// next paid hour; a box that can't is deleted.
func (p *pool) release(ctx context.Context) error {
	var held claim
	if err := p.locked(ctx, func(s *state, _ map[string]string) error {
		if h := s.held(p.root); h != nil {
			held = *h
		}
		return nil
	}); err != nil {
		return err
	}
	if held.Lease == "" {
		fmt.Fprintln(p.stdout, "this worktree holds no box")
		return nil
	}
	d, err := p.spare(ctx, held)
	if err != nil {
		fmt.Fprintf(p.stderr, "land: %s stays no spare: %v\n", held.Lease, err)
		if err := p.drop(ctx, held); err != nil {
			return err
		}
		fmt.Fprintf(p.stdout, "released %s (%s), which is deleted\n", held.Lease, held.Slug)
		return nil
	}
	fmt.Fprintf(p.stdout, "released %s (%s), a spare until %s\n", held.Lease, held.Slug, d.Local().Format(time.TimeOnly))
	return nil
}

// status prints each of the repository's boxes: its lease, slug, type, server status and who holds it; and for the
// worktree's box, crabbox's own tunnel and cp commands, ready to paste, with the box's state.
func (p *pool) status(ctx context.Context) error {
	return p.locked(ctx, func(s *state, live map[string]string) error {
		held := s.held(p.root)
		if held == nil {
			fmt.Fprintln(p.stdout, "this worktree holds no box; `land boxes run -- CMD` claims one")
		}
		for _, c := range s.Claims {
			holder := c.Worktree
			switch {
			case c.Lease == "":
				holder = "being created"
			case c.Snapshot:
				holder = "being built into a snapshot"
			case holder == p.root:
				holder = "this worktree"
			case c.Imaging:
				holder = "a spare, being imaged"
			case holder == "":
				holder = "a spare until " + c.Deadline.Local().Format(time.TimeOnly)
				if c.Warm != "" {
					holder += ", warm at " + c.Warm[:min(len(c.Warm), 12)]
				}
			}
			fmt.Fprintf(p.stdout, "%s\t%s\t%s\t%s\t%s\n", c.Lease, c.Slug, c.Type, live[c.Lease], holder)
		}
		if held != nil {
			// Without the box's state crabbox knows no lease of it, and without land's ceiling it cuts the lease to 4 h.
			env := "XDG_STATE_HOME=" + quote(filepath.Join(p.folder(held.Box), "state")) + " CRABBOX_MAX_TTL=" + leaseTTL
			fmt.Fprintf(p.stdout, "%s crabbox tunnel --provider hetzner --id %s --local-port PORT PORT\n", env, held.Lease)
			fmt.Fprintf(p.stdout, "%s crabbox cp --provider hetzner --id %s SANDBOX:%s/PATH PATH\n", env, held.Lease, p.build)
		}
		return nil
	})
}

// servers is the status of each crabbox server in the Hetzner project, by lease.
func (p *pool) servers(ctx context.Context) (map[string]string, error) {
	servers, err := p.hz.servers(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]string{}
	for _, s := range servers {
		if lease := s.Labels["lease"]; lease != "" {
			live[lease] = s.Status
		}
	}
	return live, nil
}

func (p *pool) crabbox(ctx context.Context, box string, stdout, stderr io.Writer, args ...string) error {
	return proc.Run(ctx, p.crabboxSpec(box, nil, nil, stdout, stderr, args))
}

// crabboxIn is crabbox with stdin: the box script `run --script-stdin` uploads.
func (p *pool) crabboxIn(ctx context.Context, box string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return proc.Run(ctx, p.crabboxSpec(box, nil, stdin, stdout, stderr, args))
}

// crabboxSpec runs crabbox for the box box in the pool's worktree, with env added: with the box's folder for its state
// and temporary files, so that all crabbox keeps of the box, its claim, key and known hosts among it, goes with the
// folder; on Hetzner, in direct mode, with leases of up to lifetime (crabbox clamps a lease to its ceiling on every
// call, the default 4 h included); and without seeding the box's git repository from the origin: the box's repository
// is the box script's own.
func (p *pool) crabboxSpec(box string, env []string, stdin io.Reader, stdout, stderr io.Writer, args []string) proc.Spec {
	return proc.Spec{
		Argv: append([]string{"crabbox"}, args...),
		Dir:  p.root,
		Env: slices.Concat(os.Environ(), []string{"CRABBOX_PROVIDER=hetzner", "CRABBOX_MAX_TTL=" + leaseTTL,
			"CRABBOX_SYNC_GIT_SEED=false", "XDG_STATE_HOME=" + filepath.Join(p.folder(box), "state"),
			"TMPDIR=" + filepath.Join(p.folder(box), "tmp")}, env),
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Grace: time.Minute, // interrupted, crabbox deletes a box it was creating, which takes a few API calls
	}
}

func (p *pool) git(ctx context.Context, args ...string) (string, error) {
	out, err := proc.Output(ctx, proc.Spec{Argv: append([]string{"git"}, args...), Dir: p.root, Stderr: p.stderr})
	return strings.TrimSpace(out), err
}

// quote is word in single quotes for a POSIX shell.
func quote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}
