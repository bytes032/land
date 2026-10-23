package boxes

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bytes032/land/internal/proc"
)

// run runs argv on b, the box script for real around it, and fails the test unless it exits 0.
func run(t *testing.T, b *box, argv ...string) {
	t.Helper()
	if err := b.Run(t.Context(), nil, argv...); err != nil {
		t.Fatalf("%q: %v\n%s", argv, err, b.p.stderr)
	}
}

// standInCalls are the stand-ins' calls, one line each, that start with prefix.
func standInCalls(t *testing.T, prefix string) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("CALLS"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var calls []string
	for line := range strings.Lines(string(data)) {
		if strings.HasPrefix(line, prefix) {
			calls = append(calls, strings.TrimSuffix(line, "\n"))
		}
	}
	return calls
}

// The script makes the build root a git repository that ignores crabbox's and land's own directories, turns apt's
// daily timers off only while they are on, and runs the command there in box.env. crabbox gets the script on stdin,
// to upload, so that it prints only the command, never the script, on every box command.
func TestTheBoxScriptReadiesTheBuildRoot(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	write(t, os.Getenv("TIMERS"), "on\n")
	run(t, b, "sh", "-c", `printf '%s' "$GREETING" >greeting`)
	run(t, b, "true")
	for _, call := range f.calls(t, "run") {
		if strings.Contains(call, "cd -P .") || !strings.Contains(call, "--script-stdin") {
			t.Errorf("crabbox ran %q; want the box script on stdin, out of its arguments", call)
		}
	}
	if data, err := os.ReadFile(filepath.Join(p.build, "greeting")); err != nil || string(data) != "it's here" {
		t.Fatalf("the command found GREETING=%q, %v", data, err)
	}
	if git(t, p.build, "rev-parse", "--show-toplevel") != p.build || git(t, p.build, "check-ignore", ".crabbox/runs/x", ".land/result.json") != ".crabbox/runs/x\n.land/result.json" {
		t.Fatal("the build root isn't a git repository that ignores .crabbox/ and .land/")
	}
	if off := standInCalls(t, "sudo systemctl disable"); !slices.Equal(off, []string{"sudo systemctl disable --now apt-daily.timer apt-daily-upgrade.timer"}) {
		t.Fatalf("over two commands, sudo ran %q; want the timers turned off once", off)
	}
	if locks := standInCalls(t, "flock"); len(locks) != 4 {
		t.Fatalf("flock ran %q; want the lock taken before and after each command", locks)
	}
}

// The script runs only in the build root: a lease whose workspace isn't linked there stops before the command, at
// the script's stage.
func TestTheBoxScriptStopsOutsideTheBuildRoot(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	workspace := filepath.Join(f.dir, "work", b.c.Lease, "api")
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	var stage *StageError
	if err := b.Run(t.Context(), nil, "touch", "ran"); !errors.As(err, &stage) || stage.Stage != "script" {
		t.Fatalf("a command outside the build root gave %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "ran")); err == nil {
		t.Fatal("the command ran outside the build root")
	}
}

// A file the sync's manifest doesn't list, such as one the box's image held after the worktree deleted it, is gone
// before the command runs; listed and ignored files stay, and crabbox's own state, its manifests among it, which a
// snapshot carries too; a manifest without .land.yaml removes nothing; and nothing is removed through a link, such as
// a directory the sync replaced with a link that the box's index still names files under.
func TestTheBoxScriptRemovesWhatTheSyncDidNotList(t *testing.T) {
	newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	root := p.build
	write(t, filepath.Join(root, ".gitignore"), "/built/\n")
	write(t, filepath.Join(root, "data/file"), "in the tree")
	run(t, b, "true") // the box's index names data/file
	outside := realDir(t)
	write(t, filepath.Join(outside, "file"), "outside the tree")
	if err := os.RemoveAll(filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, ".git/crabbox/sync-manifest")
	for _, c := range []struct {
		name   string
		listed []string
		stale  bool // whether stale.txt stays
	}{
		{"the sync's", []string{".gitignore", ".land.yaml", "kept.txt", "data"}, false},
		{"one without .land.yaml", []string{".gitignore", "kept.txt", "data"}, true},
	} {
		for _, name := range []string{".land.yaml", "kept.txt", "stale.txt", "built/out", ".crabbox/sync-manifest"} {
			write(t, filepath.Join(root, name), name)
		}
		write(t, manifest, strings.Join(c.listed, "\x00")+"\x00")
		run(t, b, "true")
		for name, want := range map[string]bool{"kept.txt": true, "built/out": true, "stale.txt": c.stale, ".land.yaml": true,
			".crabbox/sync-manifest": true, ".git/crabbox/sync-manifest": true} {
			if _, err := os.Stat(filepath.Join(root, name)); (err == nil) != want {
				t.Errorf("%s: %s exists %t, want %t", c.name, name, err == nil, want)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); err != nil {
		t.Fatalf("the file the link points at: %v", err)
	}
}

// Each file gets back the time the box last gave it, whatever time crabbox's copy brought, and a file whose content
// changed the current time: an older time would pass a build of other content off as fresh.
func TestTheBoxScriptKeepsTheTimesTheBoxGave(t *testing.T) {
	newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	kept, changed := filepath.Join(p.build, "kept.go"), filepath.Join(p.build, "changed.go")
	write(t, kept, "kept")
	write(t, changed, "old")
	mtime := func(path string) time.Time {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	run(t, b, "true")
	built := mtime(kept)
	// crabbox's copy: kept.go as it was, changed.go with new content, both older than anything the box built.
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	write(t, changed, "new")
	for _, path := range []string{kept, changed} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	began := time.Now()
	run(t, b, "true")
	if got := mtime(kept); got.Sub(built).Abs() > time.Microsecond {
		t.Fatalf("kept.go's time is %s, want back the box's %s", got, built)
	}
	if got := mtime(changed); got.Before(began.Add(-time.Second)) {
		t.Fatalf("changed.go's time is %s, before the command that found its new content (%s)", got, began)
	}
}

// A failed prepare stops the box command before the command runs, naming its stage, and nothing is fetched; a box
// that dies or drops the connection, whether before the script or in prepare, is the sync's error, never prepare's;
// a command that fails is its own exit status, and what it left is fetched over the worktree's copy.
func TestRunNamesTheStageItStoppedAt(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	result, stale := filepath.Join(p.root, ".land/result.json"), filepath.Join(p.build, ".land/result.json")
	write(t, stale, "an earlier command's")
	p.cfg.Stages.Prepare = "echo preparing; exit 3"
	var stage *StageError
	if err := b.Run(t.Context(), []string{".land"}, "touch", "ran"); !errors.As(err, &stage) || stage.Stage != "prepare" || stage.Log != "preparing" {
		t.Fatalf("a failed prepare gave %v (%+v)", err, stage)
	}
	if _, err := os.Stat(filepath.Join(p.build, "ran")); err == nil {
		t.Fatal("the command ran after a failed prepare")
	}
	if _, err := os.Stat(result); err == nil {
		t.Fatal("fetched a result after a failed prepare")
	}
	p.cfg.Stages.Prepare = "kill -9 $PPID" // the box script dies in prepare without stopping itself
	if err := b.Run(t.Context(), nil, "true"); !errors.As(err, &stage) || stage.Stage != "sync" {
		t.Fatalf("a box script killed in prepare gave %v (%+v)", err, stage)
	}
	p.cfg.Stages.Prepare = "echo prepared"
	f.write(t, "unreachable", "")
	if err := b.Run(t.Context(), nil, "true"); !errors.As(err, &stage) || stage.Stage != "sync" || !strings.HasSuffix(stage.Log, "Connection refused") {
		t.Fatalf("an unreachable box gave %v (%+v)", err, stage)
	}
	if err := os.Remove(filepath.Join(f.dir, "unreachable")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.root, ".land/stale.json"), "the worktree's")
	var exit proc.Exit
	err := b.Run(t.Context(), []string{".land"}, "sh", "-c", "echo result >.land/result.json; exit 4")
	if errors.As(err, &stage) || !errors.As(err, &exit) || exit.Code != 4 {
		t.Fatalf("a command exiting 4 gave %v", err)
	}
	if data, err := os.ReadFile(result); err != nil || string(data) != "result\n" {
		t.Fatalf("the fetched result holds %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(p.root, ".land/stale.json")); err == nil {
		t.Fatal("the fetch kept the worktree's .land/stale.json, which the box doesn't have")
	}
}

// A commit that turns a directory into a file leaves the box's record naming the directory's old files; the script
// gives back times only to files that exist, so that command and every later one still run.
func TestTheBoxScriptSurvivesADirectoryBecomingAFile(t *testing.T) {
	newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	write(t, filepath.Join(p.build, "sub", "a"), "a")
	write(t, filepath.Join(p.build, "sub", "b"), "b")
	run(t, b, "true")
	if err := os.RemoveAll(filepath.Join(p.build, "sub")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.build, "sub"), "now a file")
	run(t, b, "true")
	run(t, b, "true")
}

// Giving times back only keeps builds incremental: when it fails, here on a file touch can't touch, the box command
// says so and still runs. A step that guards the tree the command runs on, here the prune, still stops it.
func TestTheBoxScriptRunsWhenItCannotGiveTimesBack(t *testing.T) {
	newFake(t)
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cpx42")
	write(t, filepath.Join(p.build, ".land.yaml"), "")
	write(t, filepath.Join(p.build, "a"), "old")
	run(t, b, "true")
	write(t, filepath.Join(p.build, "a"), "new")
	failing := realDir(t)
	t.Setenv("PATH", failing+string(os.PathListSeparator)+os.Getenv("PATH"))
	refuse := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(failing, name), []byte("#!/bin/sh\necho \""+name+": a: Permission denied\" >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in
			t.Fatal(err)
		}
	}
	refuse("touch")
	if err := b.Run(t.Context(), nil, "sh", "-c", "echo ran >ran"); err != nil {
		t.Fatalf("a box command whose times couldn't be given back gave %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(p.build, "ran")); err != nil {
		t.Fatalf("the command didn't run: %v", err)
	}
	if want := "land: could not restore file times: touch: a: Permission denied; the build may redo work\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("the box command said %q; want %q", stderr, want)
	}
	refuse("rm")
	write(t, filepath.Join(p.build, ".git/crabbox/sync-manifest"), ".land.yaml\x00a\x00") // ran is stale
	var stage *StageError
	if err := b.Run(t.Context(), nil, "sh", "-c", "echo ran >again"); !errors.As(err, &stage) || stage.Stage != "script" {
		t.Fatalf("a box command whose prune failed gave %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.build, "again")); err == nil {
		t.Fatal("the command ran after the prune failed")
	}
}

// The box's repository has a commit of the tree the command runs on, so a command that reads HEAD finds one and git
// status finds nothing to commit; the same tree keeps its commit, and a changed one gets a commit of its own.
func TestTheBoxScriptCommitsTheTreeAsHEAD(t *testing.T) {
	newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	seen := filepath.Join(t.TempDir(), "seen")
	sees := func() (head string) {
		t.Helper()
		run(t, b, "sh", "-c", `{ git rev-parse HEAD 'HEAD^{tree}' && git write-tree && git status --porcelain; } >"$1"`, "sh", seen)
		data, err := os.ReadFile(seen)
		if lines := strings.Fields(string(data)); err != nil || len(lines) != 3 || lines[1] != lines[2] {
			t.Fatalf("the command saw HEAD, its tree, the tree and git status as %q, %v; want HEAD a commit of the tree, and nothing to commit", data, err)
		}
		return strings.Fields(string(data))[0]
	}
	write(t, filepath.Join(p.build, "a"), "a")
	first := sees()
	if again := sees(); again != first {
		t.Fatalf("the same tree moved HEAD from %s to %s", first, again)
	}
	write(t, filepath.Join(p.build, "a"), "changed")
	if changed := sees(); changed == first {
		t.Fatal("a changed tree kept HEAD")
	}
}

// Over rounds of random changes to the synced tree, each command runs without a warning, and leaves the box's files
// the synced tree but what the sync didn't list, HEAD a commit of the tree it recorded with nothing to commit, a time
// recorded for each file but a link, and a fresh time on each file whose content changed, though the copy gave it an
// old one. Files, links and directories keep taking each other's places, as when a directory becomes a file (#15).
func TestTheBoxScriptFollowsRandomTreeChanges(t *testing.T) {
	newFake(t)
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cpx42")
	root := p.build
	write(t, filepath.Join(root, ".land.yaml"), "")
	run(t, b, "true")
	// Every time the box recorded is before recorded.
	prev, recorded := files(t, root), time.Now()
	rng := rand.New(rand.NewPCG(15, 17)) //nolint:gosec // a fixed seed, so a failure repeats
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for round := range 20 {
		var did []string
		for range 1 + rng.IntN(5) {
			did = append(did, changeTree(t, rng, root, ""))
		}
		var stale []string
		for range rng.IntN(2) {
			stale = append(stale, strings.TrimPrefix(changeTree(t, rng, root, "stale"), "file "))
		}
		want := files(t, root)
		var listed, timed []string
		for _, rel := range slices.Sorted(maps.Keys(want)) {
			switch {
			case slices.Contains(stale, rel):
				delete(want, rel)
			case strings.HasPrefix(want[rel], "file "):
				listed, timed = append(listed, rel), append(timed, rel)
				if err := os.Chtimes(filepath.Join(root, rel), old, old); err != nil { // crabbox's copy gives the Mac's time
					t.Fatal(err)
				}
			case strings.HasPrefix(want[rel], "link "):
				listed = append(listed, rel)
			}
		}
		write(t, filepath.Join(root, ".git/crabbox/sync-manifest"), strings.Join(listed, "\x00")+"\x00")
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("round %d, after %q: %s", round, did, fmt.Sprintf(format, args...))
		}
		stderr.Reset()
		run(t, b, "sh", "-c", `mkdir -p .land && echo "$1" >.land/ran`, "sh", fmt.Sprint(round))
		if ran, err := os.ReadFile(filepath.Join(root, ".land/ran")); err != nil || string(ran) != fmt.Sprintln(round) {
			fail("the command didn't run: %q, %v", ran, err)
		}
		if strings.Contains(stderr.String(), "land: could not") {
			fail("the box command warned:\n%s", stderr)
		}
		if got := files(t, root); !maps.Equal(got, want) {
			fail("the box holds\n%q\nwant the synced tree\n%q", got, want)
		}
		times, err := os.ReadFile(filepath.Join(root, ".git/land-times"))
		if err != nil {
			t.Fatal(err)
		}
		record := strings.Split(strings.TrimSuffix(string(times), "\x00"), "\x00")
		if head := git(t, root, "rev-parse", "HEAD^{tree}"); head != record[0] {
			fail("HEAD's tree is %s, the recorded tree %s", head, record[0])
		}
		if status := git(t, root, "status", "--porcelain"); status != "" {
			fail("git status found\n%s", status)
		}
		var inRecord []string
		for i := 1; i < len(record); i += 2 {
			inRecord = append(inRecord, record[i])
		}
		if !slices.Equal(inRecord, timed) {
			fail("the record times %q; want each file but links, %q", inRecord, timed)
		}
		for _, rel := range timed {
			if strings.HasPrefix(prev[rel], "file ") && prev[rel][9:] == want[rel][9:] {
				continue
			}
			if info, err := os.Lstat(filepath.Join(root, rel)); err != nil {
				t.Fatal(err)
			} else if !info.ModTime().After(recorded) {
				fail("%s, whose content changed, has the time %v, which the box gave before %v", rel, info.ModTime(), recorded)
			}
		}
		prev, recorded = want, time.Now()
	}
}

// changeTree makes one random change at a random path of root, of up to three names, one with a space, so that files,
// links and directories keep taking each other's places, and says what it did. A file it makes holds content if
// content isn't "".
func changeTree(t *testing.T, rng *rand.Rand, root, content string) string {
	t.Helper()
	pick := func() string {
		names := []string{"a", "b c", "d"}
		rel := names[rng.IntN(3)]
		for range rng.IntN(3) {
			rel += "/" + names[rng.IntN(3)]
		}
		return rel
	}
	// reached is what rel is, if it is reached through directories only, never a link.
	reached := func(rel string) (fs.FileInfo, bool) {
		names := strings.Split(rel, "/")
		for i := range names {
			info, err := os.Lstat(filepath.Join(root, filepath.Join(names[:i+1]...)))
			if err != nil || i < len(names)-1 && !info.IsDir() {
				return nil, false
			}
			if i == len(names)-1 {
				return info, true
			}
		}
		return nil, false
	}
	// place clears rel for a new entry: what was there goes, and each name above it is a directory.
	place := func(rel string) string {
		t.Helper()
		names := strings.Split(rel, "/")
		for i := 1; i < len(names); i++ {
			if info, err := os.Lstat(filepath.Join(root, filepath.Join(names[:i]...))); err == nil && !info.IsDir() {
				if err := os.Remove(filepath.Join(root, filepath.Join(names[:i]...))); err != nil {
					t.Fatal(err)
				}
			}
		}
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(full); err != nil {
			t.Fatal(err)
		}
		return full
	}
	rel := pick()
	op := rng.IntN(6)
	if content != "" {
		op = 0
	}
	var err error
	switch op {
	case 0:
		if content == "" {
			content = fmt.Sprintf("v%d", rng.IntN(20)) // often the same size as before
		}
		full := place(rel)
		if err = os.WriteFile(full, []byte(content), 0o600); err == nil && rng.IntN(2) == 0 {
			err = os.Chmod(full, 0o700) //nolint:gosec // an executable file of the tree
		}
		rel = "file " + rel
	case 1:
		if _, ok := reached(rel); ok {
			err = os.RemoveAll(filepath.Join(root, rel))
		}
		rel = "rm " + rel
	case 2:
		from := pick()
		if _, ok := reached(from); ok && from != rel && !strings.HasPrefix(rel, from+"/") && !strings.HasPrefix(from, rel+"/") {
			err = os.Rename(filepath.Join(root, from), place(rel))
		}
		rel = "mv " + from + " " + rel
	case 3:
		to := pick()
		full := place(rel)
		target, _ := filepath.Rel(filepath.Dir(full), filepath.Join(root, to))
		err = os.Symlink(target, full)
		rel = "ln " + to + " " + rel
	case 4:
		err = os.Mkdir(place(rel), 0o750)
		rel = "mkdir " + rel
	case 5:
		if info, ok := reached(rel); ok && info.Mode().IsRegular() {
			err = os.Chmod(filepath.Join(root, rel), info.Mode()^0o100)
		}
		rel = "chmod " + rel
	}
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// files are root's paths but .git, .crabbox and .land, each with what it is: "dir", "link TARGET" or "file MODE
// CONTENT", MODE in three octal digits.
func files(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case rel == ".git" || rel == ".crabbox" || rel == ".land":
			return filepath.SkipDir
		case d.IsDir():
			got[rel] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			got[rel] = "link " + target
			return err
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			got[rel] = fmt.Sprintf("file %03o %s", info.Mode().Perm(), data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
