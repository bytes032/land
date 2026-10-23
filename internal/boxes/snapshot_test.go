package boxes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A new box starts from the repository's newest snapshot Hetzner has finished, never one still being created, nor
// another repository's; with none, a claim stops and names `land boxes snapshot`.
func TestBoxesStartOnlyFromTheRepositorysAvailableSnapshot(t *testing.T) {
	now := time.Now()
	snapshot := func(id int64, status, repo string, age time.Duration) image {
		return image{ID: id, Status: status, Created: now.Add(-age), Labels: map[string]string{snapshotLabel: repo}}
	}
	for _, c := range []struct {
		name   string
		images []image
		want   string // the image the box starts from, or "" for none
	}{
		{"an older available one", []image{snapshot(2, "creating", "acme.api", 0), snapshot(1, "available", "acme.api", time.Hour),
			snapshot(3, "available", "acme.other", 0)}, "1"},
		{"one being created", []image{snapshot(2, "creating", "acme.api", 0)}, ""},
		{"none", nil, ""},
	} {
		f := newFake(t)
		f.api.images = c.images
		p, _, _ := testPool(t)
		b, err := p.claim(t.Context(), "cpx42")
		switch {
		case c.want == "" && (err == nil || !strings.Contains(err.Error(), "`land boxes snapshot` builds one")):
			t.Errorf("%s: claimed %v, %v; want the claim stopped, naming land boxes snapshot", c.name, b, err)
		case c.want == "" && len(f.calls(t, "warmup")) > 0:
			t.Errorf("%s: created a box from %q", c.name, f.lines(t, "images", ""))
		case c.want != "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && !slices.Equal(f.lines(t, "images", ""), []string{c.want}):
			t.Errorf("%s: the box started from %q, want %s", c.name, f.lines(t, "images", ""), c.want)
		}
	}
}

// A claim waiting at Hetzner's limit asks again with the repository's snapshot of the moment, not the one it saw first,
// which a spare's refresh may have deleted meanwhile.
func TestAClaimAtTheLimitAsksForTheSnapshotOfTheMoment(t *testing.T) {
	f := newFake(t)
	f.write(t, "refuse", "1\n")
	p, _, _ := testPool(t)
	p.sleep = func(context.Context, time.Duration) error {
		f.api.replace(image{ID: 2, Status: "available", Created: time.Now(), Labels: map[string]string{snapshotLabel: "acme.api", "commit": "new"}})
		return nil
	}
	claimed(t, p, "cpx42")
	if images := f.lines(t, "images", ""); !slices.Equal(images, []string{"2"}) {
		t.Fatalf("the box started from %q, want the snapshot of the moment, 2", images)
	}
}

// `land boxes snapshot` builds the snapshot at the trunk's green commit, whatever the worktree holds, committed or not,
// with that commit's own configuration, on a box from plain Ubuntu: setup, then prepare and warm. The image carries no
// token nor timer, and the older snapshot goes only once the new one is available. The box is deleted after.
func TestASnapshotIsBuiltAtTheGreenCommitAndReplacesTheOldOnlyOnceAvailable(t *testing.T) {
	f := newFake(t)
	p, green := greenWorld(t, testConfig+`  setup: echo set up >>stages
  prepare: echo prepared >>stages
  warm: echo "warmed ${GREETING}" >>stages
`, testConfig+"  setup: echo the branch setup >>stages\n")
	write(t, filepath.Join(p.root, "work.txt"), "uncommitted")
	p.sleep = func(context.Context, time.Duration) error { return nil }
	f.api.creating = 2
	var violations []string
	f.api.imaged = func() {
		for _, path := range []string{p.code.run, filepath.Join(os.Getenv("UNITS"), "land-self-delete.timer")} {
			if _, err := os.Stat(path); err == nil {
				violations = append(violations, path)
			}
		}
	}
	if err := p.snapshot(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if images := f.lines(t, "images", ""); !slices.Equal(images, []string{"plain Ubuntu"}) {
		t.Fatalf("the snapshot's box started from %q, want plain Ubuntu", images)
	}
	if heads := f.lines(t, "heads", ""); len(heads) == 0 || slices.ContainsFunc(heads, func(h string) bool { return h != green }) {
		t.Fatalf("crabbox ran from checkouts at %q; want each at the green commit %s", heads, green)
	}
	if data, err := os.ReadFile(filepath.Join(p.code.build, "stages")); err != nil || string(data) != "set up\nprepared\nwarmed it's here\n" {
		t.Fatalf("the box ran %q, %v; want the green commit's setup, then prepare and warm", data, err)
	}
	if len(violations) > 0 {
		t.Fatalf("the snapshot carried %q", violations)
	}
	changes, images := f.api.changed()
	made := slices.IndexFunc(changes, func(c string) bool { return strings.HasPrefix(c, "available /images/") })
	deleted := slices.Index(changes, "DELETE /images/1")
	if made < 0 || deleted < made {
		t.Fatalf("changes %q: want the old snapshot deleted only once the new one is available", changes)
	}
	if len(images) != 1 || images[0].Labels[snapshotLabel] != "acme.api" || images[0].Labels["commit"] != green {
		t.Fatalf("snapshots %+v; want the new one alone, of acme.api at %s", images, green)
	}
	// The box goes through Hetzner's API, server and key, and its folder with it, the checkout in it included, which git
	// forgets.
	if len(f.servers(t)) != 0 || len(f.keys(t)) != 0 || len(p.folders(t)) != 0 ||
		!slices.ContainsFunc(changes, func(c string) bool { return strings.HasPrefix(c, "DELETE /servers/") }) {
		t.Fatalf("servers %v, keys %v, folders %q, changes %q; want the snapshot's box, key and folder deleted", f.servers(t), f.keys(t), p.folders(t), changes)
	}
	if worktrees := git(t, p.root, "worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("git still knows of the snapshot's checkout:\n%s", worktrees)
	}
	if data, err := os.ReadFile(filepath.Join(p.root, "work.txt")); err != nil || string(data) != "uncommitted" {
		t.Fatalf("the worktree's work: %q, %v", data, err)
	}
}

// `land boxes snapshot --head` builds the snapshot at the worktree's HEAD commit, with its own configuration, when the
// trunk has none yet, as before a repository's adoption of land lands, and labels it with that commit; without --head,
// the trunk's green commit, with no configuration, builds nothing. A HEAD with no committed .land.yaml is refused.
func TestASnapshotAtHeadIsBuiltAtHeadsCommitWithItsConfiguration(t *testing.T) {
	f := newFake(t)
	p, green := greenWorld(t, "", testConfig+"  setup: echo the branch setup >>stages\n")
	p.sleep = func(context.Context, time.Duration) error { return nil }
	if err := p.snapshot(t.Context(), false); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%.12s", green)) {
		t.Fatalf("without --head: %v; want the green commit's missing configuration", err)
	}
	head := git(t, p.root, "rev-parse", "HEAD")
	if err := p.snapshot(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if heads := f.lines(t, "heads", ""); len(heads) == 0 || slices.ContainsFunc(heads, func(h string) bool { return h != head }) {
		t.Fatalf("crabbox ran from checkouts at %q; want each at HEAD %s", heads, head)
	}
	if data, err := os.ReadFile(filepath.Join(p.code.build, "stages")); err != nil || string(data) != "the branch setup\n" {
		t.Fatalf("the box ran %q, %v; want HEAD's setup", data, err)
	}
	if _, images := f.api.changed(); len(images) != 1 || images[0].Labels["commit"] != head {
		t.Fatalf("snapshots %+v; want one at HEAD %s", images, head)
	}
	git(t, p.root, "rm", "-q", "--cached", ".land.yaml")
	git(t, p.root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "no configuration")
	if err := p.snapshot(t.Context(), true); err == nil || !strings.Contains(err.Error(), "has no committed .land.yaml") {
		t.Fatalf("at a HEAD with only an uncommitted .land.yaml: %v; want it refused", err)
	}
}

// A snapshot Hetzner failed to make, unavailable, fails its imaging as soon as it stops being created, never waited on,
// and deletes no older snapshot.
func TestAFailedSnapshotFailsAtOnce(t *testing.T) {
	f := newFake(t)
	f.write(t, "servers", `{"id":42,"name":"crabbox-box","status":"running","labels":{"crabbox":"true"}}`+"\n")
	f.api.creating, f.api.failing = 1, true
	p, _, _ := testPool(t)
	now := time.Now()
	var waited time.Duration
	p.now = func() time.Time { return now }
	p.sleep = func(_ context.Context, d time.Duration) error {
		now, waited = now.Add(d), waited+d
		return nil
	}
	_, err := p.image(t.Context(), 42, "abc")
	if err == nil || !strings.Contains(err.Error(), "unavailable") || waited > time.Minute {
		t.Fatalf("imaging a snapshot Hetzner failed gave %v after waiting %s", err, waited)
	}
	if changes, _ := f.api.changed(); slices.Contains(changes, "DELETE /images/1") {
		t.Fatalf("a failed snapshot deleted the older one: %q", changes)
	}
}

// A spare whose warm-up passed replaces the repository's snapshot when the newest one that hasn't failed, the newest
// a box can boot, is old and of another commit; never while a newer one is being made.
func TestAStaleSnapshotIsJudgedByTheNewestThatDidNotFail(t *testing.T) {
	now := time.Now()
	snapshot := func(status, commit string, age time.Duration) image {
		return image{Status: status, Created: now.Add(-age), Labels: map[string]string{snapshotLabel: "acme.api", "commit": commit}}
	}
	for _, c := range []struct {
		name      string
		snapshots []image // newest first
		want      bool
	}{
		{"none", nil, true},
		{"an old one", []image{snapshot("available", "old", 5*time.Hour)}, true},
		{"a young one", []image{snapshot("available", "old", time.Hour)}, false},
		{"one of the spare's commit", []image{snapshot("available", "new", 5*time.Hour)}, false},
		{"a newer one being made", []image{snapshot("creating", "", 0), snapshot("available", "old", 5*time.Hour)}, false},
		{"a newer failed one", []image{snapshot("unavailable", "", time.Hour), snapshot("available", "old", 5*time.Hour)}, true},
		{"a newer failed one over a young one", []image{snapshot("unavailable", "", 0), snapshot("available", "old", time.Hour)}, false},
		{"failed ones only", []image{snapshot("unavailable", "", time.Hour)}, true},
	} {
		if got := stale(c.snapshots, "new", now); got != c.want {
			t.Errorf("%s: stale = %t, want %t", c.name, got, c.want)
		}
	}
}

// The sweep, which needs no worktree, deletes land's crabbox servers, those with crabbox's labels and a slug of land's,
// past their deadline label, older than 25 h or powered off, with crabbox's key for each; never another server,
// whatever its labels say, another tool's crabbox box among them.
func TestTheSweepDeletesOnlyLandsBoxesThatShouldBeGone(t *testing.T) {
	f := newFake(t)
	now := time.Now()
	crabbox := func(n int, slug string) map[string]string {
		lease := fmt.Sprintf("cbx_%012x", n)
		return map[string]string{"crabbox": "true", "created_by": "crabbox", "lease": lease, "slug": slug,
			"provider_key": "crabbox-" + strings.ReplaceAll(lease, "_", "-")}
	}
	with := func(labels map[string]string, key, value string) map[string]string {
		labels[key] = value
		return labels
	}
	past, future := fmt.Sprint(now.Add(-time.Minute).Unix()), fmt.Sprint(now.Add(time.Hour).Unix())
	servers := []server{
		{ID: 1, Status: "running", Created: now, Labels: with(crabbox(1, "land-000000000001"), deadlineLabel, past)},
		{ID: 2, Status: "running", Created: now, Labels: with(crabbox(2, "land-000000000002"), deadlineLabel, future)},
		{ID: 3, Status: "running", Created: now.Add(-26 * time.Hour), Labels: crabbox(3, "land-000000000003")},
		{ID: 4, Status: "off", Created: now, Labels: crabbox(4, "land-000000000004")},
		{ID: 5, Status: "off", Created: now.Add(-26 * time.Hour), Labels: map[string]string{deadlineLabel: past, "slug": "land-000000000005"}},
		{ID: 6, Status: "off", Created: now.Add(-26 * time.Hour), Labels: map[string]string{"crabbox": "true", "lease": "cbx_000000000006"}},
		{ID: 7, Status: "off", Created: now.Add(-26 * time.Hour), Labels: with(crabbox(7, "blue-lobster-0007"), deadlineLabel, past)},
	}
	if err := f.api.save(servers); err != nil {
		t.Fatal(err)
	}
	var keys strings.Builder
	for _, s := range servers {
		if name := s.Labels["provider_key"]; name != "" {
			fmt.Fprintf(&keys, `{"id":%d,"name":%q}`+"\n", 100+s.ID, name)
		}
	}
	f.write(t, "keys", keys.String())
	t.Chdir(t.TempDir()) // no worktree
	if err := Main(t.Context(), []string{"sweep"}); err != nil {
		t.Fatal(err)
	}
	changes, _ := f.api.changed()
	want := []string{"DELETE /servers/1", "DELETE /ssh_keys/101", "DELETE /servers/3", "DELETE /ssh_keys/103", "DELETE /servers/4", "DELETE /ssh_keys/104"}
	if !slices.Equal(changes, want) {
		t.Fatalf("the sweep made %q, want %q", changes, want)
	}
}
