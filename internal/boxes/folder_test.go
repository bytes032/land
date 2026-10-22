package boxes

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every crabbox call for a box runs with the box's folder for crabbox's state and temporary files, and the folder goes
// with the box, all crabbox kept there with it, whichever way the box ends: a spare that deleted itself, a box whose
// worktree is gone, one crabbox powered off, one built into a snapshot. A spare's warm-up starts in its folder.
func TestABoxIsAFolder(t *testing.T) {
	f := newFake(t)
	spare, _ := greenWorld(t, testConfig+"  warm: echo warmed >warmed\n", "")
	var warmedIn string
	spare.background = func(dir string, _ ...string) error { warmedIn = dir; return nil }
	at(t, spare, offTheHour(), offTheHour().Add(30*time.Minute))
	removed, _, _ := testPool(t)
	off, _, _ := testPool(t)
	folders, leases := map[string]string{}, map[*pool]string{}
	for _, p := range []*pool{spare, removed, off} {
		b := claimed(t, p, "")
		if err := b.Run(t.Context(), []string{".land"}, "sh", "-c", "mkdir -p .land && echo done >.land/result"); err != nil {
			t.Fatalf("%v\n%s", err, p.stderr)
		}
		leases[p] = b.c.Lease
		maps.Copy(folders, p.boxes(t))
	}
	if err := spare.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if warmedIn != folders[leases[spare]] {
		t.Fatalf("the spare's warm-up started in %s, not its folder %s", warmedIn, folders[leases[spare]])
	}
	f.setServer(t, leases[spare], "") // its self-delete, at its deadline, which leaves its key to land
	if err := os.RemoveAll(removed.root); err != nil {
		t.Fatal(err)
	}
	f.setServer(t, leases[off], "off")
	if err := spare.snapshot(t.Context()); err != nil {
		t.Fatalf("%v\n%s", err, spare.stderr)
	}
	p, _, stderr := testPool(t)
	if err := p.status(t.Context()); err != nil {
		t.Fatal(err)
	}
	slugs := map[string]string{}
	for _, line := range f.lines(t, "leases", "") {
		lease, slug, _ := strings.Cut(line, " ")
		slugs[lease] = slug
	}
	for _, line := range f.lines(t, "envs", "") {
		fields := strings.Split(line, "\t")
		args, state, tmp := strings.Fields(fields[0]), fields[1], fields[2]
		slug := ""
		if i := slices.Index(args, "--slug"); i >= 0 && args[0] == "warmup" {
			slug = args[i+1]
		} else if i := slices.Index(args, "--id"); i >= 0 {
			slug = slugs[args[i+1]]
		}
		folder := filepath.Join(p.dir, strings.TrimPrefix(slug, "land-"))
		if !strings.HasPrefix(slug, "land-") || state != filepath.Join(folder, "state") || tmp != filepath.Join(folder, "tmp") {
			t.Errorf("crabbox %s ran with XDG_STATE_HOME %s and TMPDIR %s; want its box's folder, %s", fields[0], state, tmp, folder)
		}
	}
	for lease, folder := range folders {
		if filepath.Base(folder) != strings.TrimPrefix(slugs[lease], "land-") {
			t.Errorf("%s, %s, has the folder %s", lease, slugs[lease], folder)
		}
	}
	if len(slugs) != 4 || len(f.servers(t)) != 0 || len(f.keys(t)) != 0 || len(p.folders(t)) != 0 || len(p.claims(t)) != 0 {
		t.Fatalf("boxes %v left servers %v, keys %v, folders %q and claims %v; want none\n%s",
			slugs, f.servers(t), f.keys(t), p.folders(t), p.claims(t), stderr)
	}
}

// A box stays its worktree's however many crabbox servers the project holds: land reads every page of Hetzner's
// listing, where crabbox's own stops at the first 100 servers.
func TestABoxPastTheFirstHundredServersIsKept(t *testing.T) {
	f := newFake(t)
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cpx42")
	ours, err := f.api.load()
	if err != nil {
		t.Fatal(err)
	}
	var servers []server
	for n := range 120 {
		servers = append(servers, server{ID: int64(1_000_000 + n), Name: "crabbox-other-" + strconv.Itoa(n), Status: "running",
			Created: time.Now(), Labels: map[string]string{"crabbox": "true", "created_by": "crabbox",
				"lease": fmt.Sprintf("cbx_%012x", 0xf00000+n), "slug": "other-" + strconv.Itoa(n)}})
	}
	if err := f.api.save(slices.Insert(servers, 110, ours...)); err != nil { // ours the 111th
		t.Fatal(err)
	}
	if again := claimed(t, p, "cpx42"); again.c.Lease != b.c.Lease {
		t.Fatalf("the worktree's box %s, the 111th server, was forgotten: the claim got %s\n%s", b.c.Lease, again.c.Lease, stderr)
	}
	if _, up := f.servers(t)[b.c.Lease]; !up || len(f.calls(t, "warmup")) != 1 {
		t.Fatalf("the box is up %t after warmups %q; want it up, and no other box\n%s", up, f.calls(t, "warmup"), stderr)
	}
}

// A box whose land command died while creating it, its folder's lock free, goes with all it made, found as land finds
// what crabbox made before it printed the lease: the server by its slug's label, the SSH key by the fingerprint of the
// public key in the box's folder, then the folder and the claim. So does a folder no claim names, whose lock is free. A
// box whose land command still holds its folder's lock is being created: left alone, and shown so.
func TestABoxWhoseLandDiedGoesWithAllItMade(t *testing.T) {
	f := newFake(t)
	p, stdout, stderr := testPool(t)
	ids := map[string]string{"died": "0123456789ab", "creating": "ba9876543210", "unnamed": "00000000beef"}
	keyOf := map[string]string{}
	for name, id := range ids {
		folder := filepath.Join(p.dir, id)
		write(t, filepath.Join(folder, "lock"), "")
		for _, dir := range []string{"state", "tmp"} {
			if err := os.Mkdir(filepath.Join(folder, dir), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		// What crabbox made of the box before land died.
		warmup := exec.Command("crabbox", "warmup", "--slug", "land-"+id, "--type", "cpx42", "--ttl", "24h", "--idle-timeout", "24h")
		warmup.Env = append(os.Environ(), "CRABBOX_PROVIDER=hetzner", "CRABBOX_MAX_TTL=24h", "CRABBOX_SYNC_GIT_SEED=false",
			"XDG_STATE_HOME="+filepath.Join(folder, "state"), "TMPDIR="+filepath.Join(folder, "tmp"))
		if out, err := warmup.CombinedOutput(); err != nil {
			t.Fatalf("crabbox warmup: %v\n%s", err, out)
		}
		for _, s := range f.servers(t) {
			if s.Labels["slug"] == "land-"+id {
				keyOf[name] = s.Labels["provider_key"]
			}
		}
	}
	claim := `{"box": %q, "worktree": %q, "lease": "", "slug": "land-%[1]s", "type": "cpx42", "server": 0}`
	write(t, filepath.Join(p.dir, "claims.json"), `{"claims": [`+fmt.Sprintf(claim, ids["died"], p.root)+", "+
		fmt.Sprintf(claim, ids["creating"], p.root)+"]}\n")
	unlock, err := lock(filepath.Join(p.dir, ids["creating"], "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := p.status(t.Context()); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	slugs, keys := map[string]bool{}, f.keys(t)
	for _, s := range f.servers(t) {
		slugs[s.Labels["slug"]] = true
	}
	for name, id := range ids {
		_, key := keys[keyOf[name]]
		_, err := os.Stat(filepath.Join(p.dir, id))
		if left := name == "creating"; slugs["land-"+id] != left || key != left || (err == nil) != left {
			t.Errorf("%s: server %t, key %t, folder %t; want each %t\n%s", name, slugs["land-"+id], key, err == nil, left, stderr)
		}
	}
	if held := p.claims(t); len(held) != 1 || !strings.Contains(stdout.String(), "\tland-"+ids["creating"]+"\tcpx42\t\tbeing created\n") {
		t.Fatalf("claims %v, status %q; want the box being created alone, shown so", held, stdout)
	}
}

// A claim's lock names its worktree, and goes once the worktree is gone.
func TestTheClaimLockOfAGoneWorktreeGoes(t *testing.T) {
	newFake(t)
	p, _, _ := testPool(t)
	claimed(t, p, "cpx42")
	file := filepath.Join(p.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(p.root)))[:16]+".claiming")
	if data, err := os.ReadFile(file); err != nil || string(data) != p.root {
		t.Fatalf("the claim's lock %s holds %q, %v; want its worktree, %s", file, data, err, p.root)
	}
	if err := os.RemoveAll(p.root); err != nil {
		t.Fatal(err)
	}
	other, _, stderr := testPool(t)
	if err := other.status(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the claim lock of a gone worktree is still there: %v\n%s", err, stderr)
	}
}

// land runs only the crabbox versions it knows, from 0.78.1 up to, not including, 0.80.0, and names them and the one
// installed when it refuses another.
func TestLandRunsOnlyTheCrabboxVersionsItKnows(t *testing.T) {
	f := newFake(t)
	root, cfg := worktree(t)
	for _, c := range []struct {
		version string
		runs    bool
	}{{"0.77.0", false}, {"0.78.1", true}, {"0.79.1", true}, {"0.80.0", false}} {
		f.write(t, "version", c.version+"\n")
		_, err := newPool(t.Context(), root, cfg)
		switch {
		case c.runs && err != nil:
			t.Errorf("crabbox %s: %v", c.version, err)
		case !c.runs && (err == nil || !strings.Contains(err.Error(), "crabbox "+c.version+" is installed") ||
			!strings.Contains(err.Error(), "0.78.1") || !strings.Contains(err.Error(), "0.80.0")):
			t.Errorf("crabbox %s: %v; want it refused, naming it, 0.78.1 and 0.80.0", c.version, err)
		}
	}
}
