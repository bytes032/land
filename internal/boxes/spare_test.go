package boxes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
)

// A spare's deadline is 5 minutes before the next hour Hetzner bills the server for, counted from its creation, not
// by the clock, and which may already have passed.
func TestTheDeadlineIsFiveMinutesBeforeTheNextPaidHour(t *testing.T) {
	created := time.Date(2026, 10, 21, 10, 20, 0, 0, time.UTC)
	for released, want := range map[string]string{
		"10:20:30": "11:15",
		"10:40:00": "11:15",
		"11:14:59": "11:15",
		"11:17:00": "11:15", // the next hour starts in 3 minutes: no spare
		"11:20:00": "12:15",
		"13:50:00": "14:15",
	} {
		now, err := time.Parse(time.DateTime, "2026-10-21 "+released)
		if err != nil {
			t.Fatal(err)
		}
		if got := deadline(created, now).Format("15:04"); got != want {
			t.Errorf("released at %s: deadline %s, want %s", released, got, want)
		}
	}
}

// at sets the fake's clock: the servers crabbox creates are created at created, and p's now is now.
func at(t *testing.T, p *pool, created, now time.Time) {
	t.Helper()
	t.Setenv("FAKE_CREATED", created.UTC().Format(time.RFC3339))
	p.now = func() time.Time { return now }
}

// offTheHour is a creation time 20 minutes past a clock hour, an hour or two ago, where a paid hour and a clock hour
// part.
func offTheHour() time.Time { return time.Now().Truncate(time.Hour).Add(-100 * time.Minute) }

// armed is whether the box is armed as a spare: the flag its self-delete and rearm read, and its server's label.
func armed(t *testing.T, f *fake, lease string) (file, label bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(runDir, "spare"))
	_, label = f.servers(t)[lease].Labels[deadlineLabel]
	return err == nil, label
}

// record is the repository's record of the box with lease.
func (p *pool) record(t *testing.T, lease string) claim {
	t.Helper()
	var c claim
	if err := p.withState(func(s *state) error {
		if h := s.byLease(lease); h != nil {
			c = *h
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// A released box stays as a spare until 5 minutes before its next paid hour, a deadline it holds and its server's
// label names; a claim takes it over, its deadline cleared on the box, the server and the record, its warm-up stopped.
// The box got its self-delete at creation: the token in a root-only file, and a timer.
func TestASpareKeepsItsDeadlineUntilAClaimClearsIt(t *testing.T) {
	f := newFake(t)
	created := offTheHour()
	p, stdout, _ := testPool(t)
	at(t, p, created, created.Add(80*time.Minute))
	b := claimed(t, p, "cpx42")
	if info, err := os.Stat(filepath.Join(runDir, "auth")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the box's token: %v, %v; want a file only its owner reads", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(runDir, "auth")); err != nil || string(data) != "Authorization: Bearer "+testToken+"\n" {
		t.Fatalf("the box's token file holds %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("UNITS"), "land-self-delete.timer")); err != nil {
		t.Fatalf("no self-delete timer: %v", err)
	}
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	unix := strconv.FormatInt(created.Add(115*time.Minute).Unix(), 10)
	if data, err := os.ReadFile(filepath.Join(runDir, "deadline")); err != nil || strings.TrimSpace(string(data)) != unix {
		t.Fatalf("the box's deadline is %q, %v; want %s, 5 minutes before its second paid hour ends", data, err, unix)
	}
	if label := f.servers(t)[b.c.Lease].Labels[deadlineLabel]; label != unix {
		t.Fatalf("the server's deadline label is %q, want %s", label, unix)
	}
	if held := p.claims(t); held["spare"] != b.c.Lease || !strings.Contains(stdout.String(), "a spare until") {
		t.Fatalf("after release: claims %v, %q", held, stdout)
	}
	other, _, _ := testPool(t)
	at(t, other, created, created.Add(90*time.Minute))
	if theirs := claimed(t, other, "cpx42"); theirs.c.Lease != b.c.Lease {
		t.Fatalf("a claim got %s, not the spare %s", theirs.c.Lease, b.c.Lease)
	}
	if warmups := f.calls(t, "warmup"); len(warmups) != 1 {
		t.Fatalf("crabbox warmups %q; want the one box", warmups)
	}
	if file, label := armed(t, f, b.c.Lease); file || label {
		t.Fatalf("the taken spare still has its deadline: on the box %t, as a label %t", file, label)
	}
	if c := other.record(t, b.c.Lease); c.Worktree != other.root || !c.Deadline.IsZero() {
		t.Fatalf("the taken spare's record: %+v", c)
	}
	if stops := standInCalls(t, "sudo systemctl stop land-warm"); len(stops) != 1 {
		t.Fatalf("the warm-up was stopped %d times, want once", len(stops))
	}
	if held := other.claims(t); len(held) != 1 || held[other.root] != b.c.Lease {
		t.Fatalf("after the claim: %v", held)
	}
}

// A claim whose takeover of a spare fails, and whose deletion of it fails too, as one outage of the network or Hetzner's
// API does both, leaves the spare recorded as the worktree's with its deadline: the next claim clears that deadline
// before it hands the box out, never a box that deletes itself under the worktree.
func TestAClaimDisarmsASpareItsTakeoverLeftArmed(t *testing.T) {
	f := newFake(t)
	created := offTheHour()
	p, _, _ := testPool(t)
	at(t, p, created, created.Add(80*time.Minute))
	b := claimed(t, p, "cpx42")
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	other, _, _ := testPool(t)
	at(t, other, created, created.Add(90*time.Minute))
	f.write(t, "unreachable", "")
	f.api.failDeletes(true)
	if _, err := other.claim(t.Context(), "cpx42"); err == nil {
		t.Fatal("a claim whose takeover and deletion failed succeeded")
	}
	f.api.failDeletes(false)
	if err := os.Remove(filepath.Join(f.dir, "unreachable")); err != nil {
		t.Fatal(err)
	}
	if theirs := claimed(t, other, "cpx42"); theirs.c.Lease != b.c.Lease {
		t.Fatalf("the next claim got %s, not the box it held, %s", theirs.c.Lease, b.c.Lease)
	}
	if file, label := armed(t, f, b.c.Lease); file || label {
		t.Fatalf("the worktree holds %s with its deadline: on the box %t, as a label %t", b.c.Lease, file, label)
	}
	if c := other.record(t, b.c.Lease); !c.Deadline.IsZero() {
		t.Fatalf("the record still holds the deadline: %+v", c)
	}
}

// A release that armed the box's deadline and then failed, and whose deletion of the box failed too, leaves the box
// recorded as the worktree's with its deadline: the next claim clears it before it hands the box out.
func TestAClaimDisarmsABoxAFailedReleaseLeftArmed(t *testing.T) {
	f := newFake(t)
	p, _ := greenWorld(t, testConfig+"  warm: echo warmed >warmed\n", "")
	at(t, p, offTheHour(), offTheHour().Add(30*time.Minute))
	b := claimed(t, p, "")
	f.write(t, "sshfail", "") // after the warm-up started
	f.api.failDeletes(true)
	if err := p.release(t.Context()); err == nil {
		t.Fatal("a release whose box can't be reached nor deleted succeeded")
	}
	f.api.failDeletes(false)
	if err := os.Remove(filepath.Join(f.dir, "sshfail")); err != nil {
		t.Fatal(err)
	}
	if again := claimed(t, p, ""); again.c.Lease != b.c.Lease {
		t.Fatalf("the next claim got %s, not the box the worktree held, %s", again.c.Lease, b.c.Lease)
	}
	if file, label := armed(t, f, b.c.Lease); file || label {
		t.Fatalf("the worktree holds %s with its deadline: on the box %t, as a label %t", b.c.Lease, file, label)
	}
}

// A box released less than the margin before its deadline, near the end of its paid hour, is no spare: it is deleted.
func TestABoxNearItsPaidHoursEndIsNoSpare(t *testing.T) {
	f := newFake(t)
	created := offTheHour()
	p, stdout, _ := testPool(t)
	at(t, p, created, created.Add(54*time.Minute))
	b := claimed(t, p, "cpx42")
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, up := f.servers(t)[b.c.Lease]; up || !strings.Contains(stdout.String(), "deleted") {
		t.Fatalf("after release: the server up %t, %q", up, stdout)
	}
	if held, folders := p.claims(t), p.folders(t); len(held) != 0 || len(folders) != 0 {
		t.Fatalf("claims %v, folders %q", held, folders)
	}
}

// A spare leaves whoever claims it at least an hour of its 24 h crabbox lease, at whose end crabbox powers it off: a
// box whose deadline comes later in its life is deleted at its release instead.
func TestABoxNearItsLeasesEndIsNoSpare(t *testing.T) {
	for _, c := range []struct {
		name  string
		age   time.Duration
		spare bool
	}{
		{"released in its 23rd hour", 22*time.Hour + 10*time.Minute, true}, // its deadline leaves 1 h 5 m of lease
		{"released in its 24th hour", 23*time.Hour + 10*time.Minute, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			created := time.Now().Truncate(time.Minute).Add(-c.age)
			p, _, _ := testPool(t)
			at(t, p, created, created.Add(c.age))
			b := claimed(t, p, "cpx42")
			if err := p.release(t.Context()); err != nil {
				t.Fatal(err)
			}
			held := p.claims(t)
			_, up := f.servers(t)[b.c.Lease]
			if spare := held["spare"] == b.c.Lease && up; spare != c.spare {
				t.Fatalf("after release: claims %v, the server up %t; want a spare %t", held, up, c.spare)
			}
		})
	}
}

// A claim takes a spare of its type, warm before not, then the one with the most time left; never one being imaged,
// nor one at its deadline's margin.
func TestAClaimTakesTheRightSpare(t *testing.T) {
	now := time.Now()
	spare := func(lease, typ, warm string, left time.Duration, imaging bool) claim {
		return claim{Lease: lease, Type: typ, Warm: warm, Deadline: now.Add(left), Imaging: imaging}
	}
	for _, c := range []struct {
		spares []claim
		want   string
	}{
		{[]claim{spare("a", "cpx42", "", 10*time.Minute, false), spare("b", "cpx42", "", 30*time.Minute, false)}, "b"},
		{[]claim{spare("a", "cpx42", "abc", 10*time.Minute, false), spare("b", "cpx42", "", 30*time.Minute, false)}, "a"},
		{[]claim{spare("a", "cpx42", "abc", 30*time.Minute, true), spare("b", "cpx42", "", time.Minute, false)}, ""},
		{[]claim{spare("a", "ccx63", "", 30*time.Minute, false), {Worktree: "/wt", Lease: "b", Type: "cpx42"}}, ""},
	} {
		s := state{Claims: c.spares}
		got := ""
		if taken := s.spare("cpx42", now); taken != nil {
			got = taken.Lease
		}
		if got != c.want {
			t.Errorf("of %+v, a claim takes %q, want %q", c.spares, got, c.want)
		}
	}
}

// The self-delete deletes the box's SSH key and then its server, which the metadata service names, once its deadline
// has passed, and does nothing before, nor with no deadline: a box no worktree released.
func TestTheSelfDeleteDeletesTheBoxOnlyAfterItsDeadline(t *testing.T) {
	f := newFake(t)
	f.write(t, "servers", `{"id":42,"name":"crabbox-box","status":"running","labels":{"crabbox":"true"}}`+"\n")
	f.write(t, "keys", `{"id":7,"name":"crabbox-cbx-00000000002a"}`+"\n")
	f.api.instance = 42
	write(t, filepath.Join(runDir, "auth"), "Authorization: Bearer "+testToken+"\n")
	write(t, filepath.Join(runDir, "ssh-keys"), "7\n")
	script := filepath.Join(t.TempDir(), "self-delete")
	write(t, script, selfDeleteScript())
	now := time.Now().Unix()
	for _, c := range []struct {
		name, deadline string
		want           []string
	}{
		{"no deadline", "", nil},
		{"a deadline to come", strconv.FormatInt(now+60, 10), nil},
		{"a deadline passed", strconv.FormatInt(now-1, 10), []string{"DELETE /ssh_keys/7", "DELETE /servers/42"}},
	} {
		if c.deadline != "" {
			write(t, filepath.Join(runDir, "deadline"), c.deadline+"\n")
		}
		if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		if changes, _ := f.api.changed(); !slices.Equal(changes, c.want) {
			t.Fatalf("%s: the self-delete called %q, want %q", c.name, changes, c.want)
		}
	}
	if servers := f.servers(t); len(servers) != 0 {
		t.Fatalf("servers left: %v", servers)
	}
}

// release warms a spare at the trunk's green commit, not its tip, nor the worktree's own commit, in a checkout of it
// crabbox copies to the box, with that commit's own configuration, its ${…} left for bash, not systemd, to expand; the
// warm-up's background half then marks the spare warm and, the repository's snapshot being stale, images it into a
// new snapshot, without its token or timer, which come back after, deadline included.
func TestASpareWarmsAtTheGreenCommitAndRefreshesAStaleSnapshot(t *testing.T) {
	f := newFake(t)
	f.api.images[0].Created = time.Now().Add(-5 * time.Hour)
	p, green := greenWorld(t, testConfig+`  warm: echo "warmed ${GREETING}" >warmed`+"\n",
		testConfig+"  warm: echo the branch warm-up >warmed\n")
	var started []string
	p.background = func(_ string, args ...string) error { started = args; return nil }
	at(t, p, time.Now().Add(-10*time.Minute), time.Now())
	b := claimed(t, p, "cpx42")
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if heads := f.lines(t, "heads", green); len(heads) != 1 {
		t.Fatalf("crabbox ran from checkouts at %q; want one at the green commit %s", f.lines(t, "heads", ""), green)
	}
	if data, err := os.ReadFile(filepath.Join(p.build, "warmed")); err != nil || string(data) != "warmed it's here\n" {
		t.Fatalf("the warm-up left %q, %v; want the green commit's, with box.env's GREETING", data, err)
	}
	if len(started) != 7 || started[0] != "warm" || started[1] != "acme.api" || started[2] != b.c.Lease || started[4] != green || started[5] != "true" {
		t.Fatalf("release started %q", started)
	}
	var violations []string
	f.api.imaged = func() {
		for _, path := range []string{runDir, filepath.Join(os.Getenv("UNITS"), "land-self-delete.timer")} {
			if _, err := os.Stat(path); err == nil {
				violations = append(violations, path)
			}
		}
	}
	if err := warmMain(t.Context(), started[1:]); err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("the snapshot carried %q", violations)
	}
	if _, images := f.api.changed(); len(images) != 1 || images[0].Labels["commit"] != green || images[0].Status != "available" {
		t.Fatalf("snapshots after the refresh: %+v; want the spare's, at %s, alone", images, green)
	}
	spare := p.record(t, b.c.Lease)
	if spare.Warm != green || spare.Imaging || spare.Worktree != "" {
		t.Fatalf("the spare after its warm-up: %+v", spare)
	}
	unix := strconv.FormatInt(spare.Deadline.Unix(), 10)
	for path, want := range map[string]string{
		filepath.Join(runDir, "auth"):     "Authorization: Bearer " + testToken + "\n",
		filepath.Join(runDir, "deadline"): unix + "\n",
	} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("after the refresh, %s holds %q, %v; want %q", path, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("UNITS"), "land-self-delete.timer")); err != nil {
		t.Errorf("after the refresh, no self-delete timer: %v", err)
	}
}

// A box that lost its self-delete, as a reboot empties /run, is no spare: its release, warm-up or not, deletes it, as
// nothing would delete it at its deadline.
func TestABoxWithoutItsSelfDeleteIsNoSpare(t *testing.T) {
	f := newFake(t)
	p, _ := greenWorld(t, testConfig+"  warm: echo warmed >warmed\n", "")
	p.background = func(string, ...string) error { return nil }
	at(t, p, offTheHour(), offTheHour().Add(30*time.Minute))
	b := claimed(t, p, "")
	if err := os.RemoveAll(runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(os.Getenv("UNITS"), "land-self-delete.timer")); err != nil {
		t.Fatal(err)
	}
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, up := f.servers(t)[b.c.Lease]; up || len(p.claims(t)) != 0 || len(p.folders(t)) != 0 {
		t.Fatalf("after release: the server up %t, claims %v, folders %q; want the box deleted", up, p.claims(t), p.folders(t))
	}
}

// greenWorld is a worktree of acme/api cloned from a remote whose trunk's green commit, marked success, holds the
// configuration trunk, none when it is empty, and whose tip follows it; with branch, the worktree holds a commit of its
// own after the tip, holding that configuration. Its pool's forge holds the mark.
func greenWorld(t *testing.T, trunk, branch string) (p *pool, green string) {
	t.Helper()
	dir := realDir(t)
	remote := filepath.Join(dir, "acme", "api.git")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	root := realDir(t)
	git(t, root, "clone", "-q", remote, ".")
	commit := func(message, landYAML string) string {
		write(t, filepath.Join(root, "stage"), message+"\n")
		if landYAML != "" {
			write(t, filepath.Join(root, config.File), landYAML)
		}
		git(t, root, "add", "-A")
		git(t, root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", message)
		return git(t, root, "rev-parse", "HEAD")
	}
	green = commit("green", trunk)
	commit("tip", trunk)
	git(t, root, "push", "-q", "origin", "HEAD:main")
	if branch != "" {
		commit("the branch", branch)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ = poolAt(t, root, cfg)
	marks := &forge.Memory{}
	if err := marks.SetMark(t.Context(), green, forge.Mark{State: forge.Success}); err != nil {
		t.Fatal(err)
	}
	p.forge = func(context.Context, string) (forge.Forge, error) { return marks, nil }
	return p, green
}

// readDeadline is the box's deadline in Unix seconds.
func readDeadline(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, "deadline"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A held box's deadline is 5 minutes before the first of its paid hours to end at least 15 minutes from now, counted
// from its creation, so that one nobody uses ends before Hetzner bills another hour for it; a spare's stays as set.
func TestAHeldBoxEndsBeforeTheNextPaidHourItHasntUsed(t *testing.T) {
	newFake(t)
	script := filepath.Join(t.TempDir(), "rearm")
	write(t, script, rearmScript())
	now := time.Now().Unix()
	for _, c := range []struct {
		name      string
		age, want int64 // seconds since creation; the deadline in seconds after creation
		spare     bool
	}{
		{"10 minutes old", 600, 3300, false},
		{"45 minutes old: its first deadline is under 15 minutes away", 2700, 6900, false},
		{"58 minutes old: past its first deadline", 3480, 6900, false},
		{"2 hours 10 minutes old", 7800, 10500, false},
		{"a spare", 600, 42, true},
	} {
		created := now - c.age
		write(t, filepath.Join(runDir, "created"), strconv.FormatInt(created, 10)+"\n")
		write(t, filepath.Join(runDir, "deadline"), strconv.FormatInt(created+42, 10)+"\n")
		_ = os.Remove(filepath.Join(runDir, "spare"))
		if c.spare {
			write(t, filepath.Join(runDir, "spare"), "")
		}
		if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		if got := readDeadline(t) - created; got != c.want {
			t.Errorf("%s: the deadline is %d s after creation, want %d", c.name, got, c.want)
		}
	}
}

// A held box someone is connected to at its deadline (a box command running, a tunnel open) isn't deleted: its
// deadline moves to the end of its next paid hour. A spare is deleted at its deadline whoever is connected.
func TestABoxInUseOutlivesItsDeadlineUnlessItIsASpare(t *testing.T) {
	f := newFake(t)
	f.write(t, "servers", `{"id":42,"name":"crabbox-box","status":"running","labels":{"crabbox":"true"}}`+"\n")
	f.write(t, "keys", `{"id":7,"name":"crabbox-cbx-00000000002a"}`+"\n")
	f.api.instance = 42
	write(t, filepath.Join(runDir, "auth"), "Authorization: Bearer "+testToken+"\n")
	write(t, filepath.Join(runDir, "ssh-keys"), "7\n")
	bin := t.TempDir()
	write(t, filepath.Join(bin, "ss"), "#!/bin/sh\n"+`echo 'ESTAB 0 0 10.0.0.2:2222 10.0.0.9:51234 users:(("sshd",pid=812,fd=4))'`+"\n")
	if err := os.Chmod(filepath.Join(bin, "ss"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := filepath.Join(t.TempDir(), "self-delete")
	write(t, script, selfDeleteScript())
	passed := time.Now().Unix() - 1
	write(t, filepath.Join(runDir, "deadline"), strconv.FormatInt(passed, 10)+"\n")
	if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if changes, _ := f.api.changed(); len(changes) != 0 || readDeadline(t) != passed+3600 {
		t.Fatalf("held and in use: the self-delete called %q and left the deadline %d s after the old one, want nothing called and 3600",
			changes, readDeadline(t)-passed)
	}
	write(t, filepath.Join(runDir, "deadline"), strconv.FormatInt(passed, 10)+"\n")
	write(t, filepath.Join(runDir, "spare"), "")
	if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if changes, _ := f.api.changed(); !slices.Equal(changes, []string{"DELETE /ssh_keys/7", "DELETE /servers/42"}) {
		t.Fatalf("a spare in use: the self-delete called %q, want the key and the server deleted", changes)
	}
}

// Every box command moves its held box's deadline on, so a box in use never runs into a deadline set long before.
func TestABoxCommandMovesItsHeldBoxsDeadlineOn(t *testing.T) {
	newFake(t)
	created := time.Now().Add(-50 * time.Minute).Truncate(time.Second)
	p, _, _ := testPool(t)
	at(t, p, created, time.Now())
	b := claimed(t, p, "")
	write(t, filepath.Join(runDir, "deadline"), strconv.FormatInt(time.Now().Unix()-1, 10)+"\n") // due now
	run(t, b, "true")
	if got := readDeadline(t) - created.Unix(); got != 2*3600-300 {
		t.Fatalf("after a box command 50 minutes into its first paid hour, the deadline is %d s after creation, want %d", got, 2*3600-300)
	}
}
