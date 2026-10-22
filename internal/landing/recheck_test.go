package landing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/proc"
)

// states are sha's marks' states, newest first.
func (w *world) states(t *testing.T, sha string) []forge.State {
	t.Helper()
	marks, err := w.forge.Marks(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	var states []forge.State
	for _, m := range marks {
		states = append(states, m.State)
	}
	return states
}

// newest is sha's newest mark.
func (w *world) newest(t *testing.T, sha string) forge.Mark {
	t.Helper()
	marks, err := w.forge.Marks(t.Context(), sha)
	if err != nil || len(marks) == 0 {
		t.Fatalf("%s has no mark: %v", sha, err)
	}
	return marks[0]
}

// A check after the push sets the commit's final mark: success when its checks pass; failure when one fails, with a
// commit comment naming the check, its command, its exit and its log's end, which the mark links to; error, naming
// where the box stopped, with no comment, when the box dies. After a final mark, the commit's completed CI run is
// rerun and the one still going is left to read the mark; after error, neither is.
func TestACheckAfterThePushSetsItsVerdict(t *testing.T) {
	for name, c := range map[string]struct {
		run   string
		stop  error
		state forge.State
		fails string // in the error, "" when recheck succeeds
	}{
		"passes":            {run: "true", state: forge.Success},
		"fails":             {run: "echo first; echo broken; exit 3", state: forge.Failure, fails: "main is red there"},
		"box dies":          {run: "true", stop: &boxes.StageError{Stage: "sync", Err: errors.New("connection lost")}, state: forge.Error, fails: "land recheck"},
		"box command fails": {run: "true", stop: errors.New("box gone"), state: forge.Error, fails: "land recheck"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			sha := w.push(t, map[string]string{".land.yaml": landYAML(config.Stages{Check: config.Checks{{Name: "test", Run: c.run}}}), "b.txt": "b\n"})
			if err := w.forge.SetMark(t.Context(), sha, forge.Mark{State: forge.Pending}); err != nil {
				t.Fatal(err)
			}
			done, going := w.forge.AddRun(sha, "completed", "failure"), w.forge.AddRun(sha, "in_progress", "")
			w.box.stop = c.stop
			err := w.l.recheck(t.Context(), sha)
			if c.fails == "" && err != nil || c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)) {
				t.Fatalf("recheck returned %v, want an error with %q", err, c.fails)
			}
			if states := w.states(t, sha); !slices.Equal(states, []forge.State{c.state, forge.Pending}) {
				t.Errorf("the marks are %q, newest first; want %s, then pending", states, c.state)
			}
			mark, comments := w.newest(t, sha), w.forge.Comments(sha)
			switch c.state {
			case forge.Failure:
				if len(comments) != 1 || mark.TargetURL == "" || !strings.HasSuffix(mark.TargetURL, "/1") {
					t.Fatalf("comments %q, the mark linking to %q; want one comment, linked", comments, mark.TargetURL)
				}
				for _, want := range []string{"**test** exited 3", c.run, "first\nbroken"} {
					if !strings.Contains(comments[0], want) {
						t.Errorf("the comment lacks %q:\n%s", want, comments[0])
					}
				}
			case forge.Error:
				if len(comments) != 0 || !strings.Contains(mark.Description, "stopped at "+map[string]string{"box dies": "sync", "box command fails": "box"}[name]) {
					t.Errorf("an error mark %q came with comments %q; want the stage named and none", mark.Description, comments)
				}
			}
			reruns := w.forge.Reruns()
			if want := []int64{done}; c.state == forge.Error && len(reruns) != 0 || c.state != forge.Error && !slices.Equal(reruns, want) {
				t.Errorf("reran runs %v; want %v after a final mark, none after error, and never the run still going, %d", reruns, want, going)
			}
		})
	}
}

// recheck finishes a mark that isn't final, an error or none, from a worktree it never touches, and refuses a final
// one, success or failure, as it refuses a commit that isn't on the trunk.
func TestRecheckFinishesOnlyAMarkThatIsntFinal(t *testing.T) {
	w := newWorld(t)
	errored := w.push(t, map[string]string{"b.txt": "b\n"}) // checked with the world's own .land.yaml
	unmarked := w.push(t, map[string]string{"c.txt": "c\n"})
	failed := w.push(t, map[string]string{"d.txt": "d\n"})
	for sha, state := range map[string]forge.State{errored: forge.Error, failed: forge.Failure} {
		if err := w.forge.SetMark(t.Context(), sha, forge.Mark{State: state, Description: "No verdict: the box stopped at sync: lost"}); err != nil {
			t.Fatal(err)
		}
	}
	// The caller's worktree is on a branch of its own, with a change of its own, which recheck leaves as they are.
	git(t, w.agent, "checkout", "-q", "-b", "work")
	commit(t, w.agent, "local", map[string]string{"local.txt": "l\n"})
	write(t, w.agent+"/a.txt", "editing\n")
	local := git(t, w.agent, "rev-parse", "HEAD")

	if err := w.l.recheck(t.Context(), local); err == nil || !strings.Contains(err.Error(), "isn't on main") {
		t.Errorf("recheck of a commit that isn't on the trunk returned %v", err)
	}
	for _, sha := range []string{errored, unmarked} {
		if err := w.l.recheck(t.Context(), sha); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := w.states(t, errored), w.states(t, unmarked); !slices.Equal(a, []forge.State{forge.Success, forge.Error}) || !slices.Equal(b, []forge.State{forge.Success}) {
		t.Errorf("the marks are %q and %q, newest first; want success over error, and success", a, b)
	}
	trees := git(t, w.remote, "rev-parse", errored+"^{tree}") + "\n" + git(t, w.remote, "rev-parse", unmarked+"^{tree}") + "\n"
	if checked := read(t, w.checks); checked != trees {
		t.Errorf("the box checked %q, want each commit's tree %q", checked, trees)
	}
	for sha, state := range map[string]forge.State{errored: forge.Success, failed: forge.Failure} {
		if err := w.l.recheck(t.Context(), sha); err == nil || !strings.Contains(err.Error(), "marked "+string(state)+" already") {
			t.Errorf("recheck of a commit marked %s returned %v", state, err)
		}
	}
	if checked := read(t, w.checks); checked != trees || len(w.states(t, failed)) != 1 {
		t.Error("a refused recheck checked, or marked")
	}
	if head, branch, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "branch", "--show-current"), git(t, w.agent, "status", "--porcelain"); head != local || branch != "work" || status != "M a.txt" {
		t.Errorf("recheck left the caller's worktree at %s on %q with %q; want %s on work with its change", head, branch, status, local)
	}
	if worktrees := git(t, w.agent, "worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 || len(w.released) != 2 {
		t.Errorf("after two rechecks, worktrees\n%s\nand boxes released for %q; want the caller's alone, and both released", worktrees, w.released)
	}
	for _, dir := range w.released {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("recheck's worktree %s is still there: %v", dir, err)
		}
	}
}

// The trunk's first commit has no parent: recheck checks it against the empty tree, as the gate's message for an
// unmarked commit tells agents to.
func TestRecheckChecksTheRootCommit(t *testing.T) {
	w := newWorld(t)
	root := git(t, w.remote, "rev-list", "--max-parents=0", "main")
	if err := w.l.recheck(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if states := w.states(t, root); !slices.Equal(states, []forge.State{forge.Success}) {
		t.Errorf("the root commit's marks are %q; want success", states)
	}
}

// A prepare error repeated at a commit is failure, with a comment listing prepare's command, exit and log: the commit
// broke the repository's setup. A prepare error after another stage's error is still error, and so is any box or
// Hetzner error, however often it repeats.
func TestOnlyARepeatedPrepareErrorBecomesFailure(t *testing.T) {
	w := newWorld(t)
	stages := config.Stages{Prepare: "tool install", Check: config.Checks{{Name: "test", Run: "true"}}}
	sha := w.push(t, map[string]string{".land.yaml": landYAML(stages), "b.txt": "b\n"})
	hetzner := w.push(t, map[string]string{"c.txt": "c\n"})
	done := w.forge.AddRun(sha, "completed", "failure")
	sync := &boxes.StageError{Stage: "sync", Err: errors.New("connection lost")}
	prepare := &boxes.StageError{Stage: "prepare", Err: fmt.Errorf("crabbox: %w", proc.Exit{Code: 2}), Log: "E: no such tool\n"}
	for i, stop := range []error{sync, sync, prepare, prepare} {
		w.box.stop = stop
		err := w.l.recheck(t.Context(), sha)
		if i < 3 && (err == nil || !strings.Contains(err.Error(), "land recheck")) || i == 3 && (err == nil || !strings.Contains(err.Error(), "red")) {
			t.Fatalf("recheck %d returned %v", i, err)
		}
	}
	if states := w.states(t, sha); !slices.Equal(states, []forge.State{forge.Failure, forge.Error, forge.Error, forge.Error}) {
		t.Fatalf("the marks are %q, newest first; want error after sync, sync and prepare, then failure", states)
	}
	comments, mark := w.forge.Comments(sha), w.newest(t, sha)
	if len(comments) != 1 || mark.TargetURL == "" {
		t.Fatalf("comments %q, the failure linking to %q; want one, linked", comments, mark.TargetURL)
	}
	for _, want := range []string{"**prepare** exited 2", "tool install", "E: no such tool"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("the comment lacks %q:\n%s", want, comments[0])
		}
	}
	if reruns := w.forge.Reruns(); !slices.Equal(reruns, []int64{done}) {
		t.Errorf("reran %v, want the completed run %d once, after the failure", reruns, done)
	}
	w.box.stop = nil
	w.l.claim = func(context.Context, string, *config.Config, string) (boxes.Box, error) {
		return nil, errors.New("hetzner: server limit reached")
	}
	for range 2 {
		if err := w.l.recheck(t.Context(), hetzner); err == nil || !strings.Contains(err.Error(), "stopped at claim") {
			t.Fatalf("a recheck Hetzner refused returned %v", err)
		}
	}
	if states := w.states(t, hetzner); !slices.Equal(states, []forge.State{forge.Error, forge.Error}) || len(w.forge.Comments(hetzner)) != 0 {
		t.Errorf("after Hetzner refused twice the marks are %q with comments %q; want error twice, none", states, w.forge.Comments(hetzner))
	}
}

// markRun is GitHub where the commit's CI run completes before land marks it, as when the mark's request waited out a
// rate limit past the gate's timeout.
type markRun struct {
	*forge.Memory
	run int64
}

func (f *markRun) SetMark(ctx context.Context, sha string, m forge.Mark) error {
	f.run = f.AddRun(sha, "completed", "failure")
	return f.Memory.SetMark(ctx, sha, m)
}

// land reruns the commit's CI run that completed before its mark was set, so the run copies the mark.
func TestALandingRerunsACompletedRun(t *testing.T) {
	w := newWorld(t)
	f := &markRun{Memory: w.forge}
	w.l.forge = f
	commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatal(err)
	}
	if reruns := w.forge.Reruns(); f.run == 0 || !slices.Equal(reruns, []int64{f.run}) {
		t.Errorf("reran %v, want the completed run %d", reruns, f.run)
	}
}
