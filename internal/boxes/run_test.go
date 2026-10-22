package boxes

import (
	"errors"
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
