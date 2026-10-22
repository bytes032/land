package landing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/provenance"
	"github.com/bytes032/land/internal/verdict"
)

// redTrunk pushes, after the trunk's first commit, which is red from long ago, a green commit, an unmarked one, the
// first red one, a later red one and an unmarked tip, and returns the first red one.
func redTrunk(t *testing.T, w *world) string {
	t.Helper()
	mark := func(sha string, state forge.State) {
		if err := w.forge.SetMark(t.Context(), sha, forge.Mark{State: state}); err != nil {
			t.Fatal(err)
		}
	}
	mark(w.trunk(t), forge.Failure)
	mark(w.push(t, map[string]string{"g.txt": "g\n"}), forge.Success)
	w.push(t, map[string]string{"u.txt": "u\n"})
	red := w.push(t, map[string]string{"r1.txt": "r\n"})
	mark(red, forge.Failure)
	mark(w.push(t, map[string]string{"r2.txt": "r\n"}), forge.Failure)
	w.push(t, map[string]string{"tip.txt": "t\n"})
	return red
}

// --unchecked lands without the check, still reviewed, naming the first red commit since the green one in its
// Unchecked trailer and marked failure; --unchecked '#N' names its open issue and is marked success. Verdict reports
// neither as verified.
func TestAnUncheckedLandingNamesWhyAndIsNeverVerified(t *testing.T) {
	for name, c := range map[string]struct {
		u     unchecked
		flaky bool // an earlier --unchecked '#5' landing, marked success, tops the red trunk
		state forge.State
	}{
		"the trunk's failure":                              {u: unchecked{on: true}, state: forge.Failure},
		"the trunk's failure under a flaky test's landing": {u: unchecked{on: true}, flaky: true, state: forge.Failure},
		"a flaky test":                                     {u: unchecked{on: true, issue: "#5"}, state: forge.Success},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			red := redTrunk(t, w)
			if c.flaky {
				git(t, w.other, "pull", "-q", "--ff-only", "origin", "main")
				commit(t, w.other, "an earlier landing past a flaky test\n\nUnchecked: #5", map[string]string{"f.txt": "f\n"})
				git(t, w.other, "push", "-q", "origin", "HEAD:main")
				if err := w.forge.SetMark(t.Context(), w.trunk(t), forge.Mark{State: forge.Success}); err != nil {
					t.Fatal(err)
				}
			}
			w.forge.SetIssue(5, true)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			var reviewed bool
			w.l.reviewed = func(context.Context, string) error {
				reviewed = true
				return nil
			}
			if err := w.land(t, c.u); err != nil {
				t.Fatal(err)
			}
			pushed := w.trunk(t)
			want := red
			if c.u.issue != "" {
				want = c.u.issue
			}
			if got, err := provenance.Trailer(t.Context(), w.remote, pushed, provenance.Unchecked); err != nil || got != want {
				t.Errorf("the landing's Unchecked trailer is %q, %v; want %q", got, err, want)
			}
			if got := git(t, w.remote, "show", "main:b.txt"); got != "b" {
				t.Errorf("the landing's b.txt is %q, want the work", got)
			}
			if marks, _ := w.forge.Marks(t.Context(), pushed); len(marks) != 1 || marks[0].State != c.state {
				t.Errorf("the landing's marks are %+v, want one %s", marks, c.state)
			}
			v, err := verdict.Of(t.Context(), w.forge, w.remote, pushed)
			if err != nil || v.Verified {
				t.Errorf("the verdict is %+v, %v; want unverified", v, err)
			}
			if read(t, w.checks) != "" || !reviewed {
				t.Errorf("an unchecked landing ran a check, or wasn't reviewed (%t)", reviewed)
			}
		})
	}
}

// An unchecked landing refuses, before it pushes, marks or moves anything, when there is no failure to inherit, or the
// issue it names isn't open, and like any landing, an unreviewed change.
func TestAnUncheckedLandingRefusesWithNothingToName(t *testing.T) {
	for name, c := range map[string]struct {
		setup   func(t *testing.T, w *world)
		u       unchecked
		refusal string
	}{
		"a green tip": {func(t *testing.T, w *world) {
			_ = w.forge.SetMark(t.Context(), w.push(t, map[string]string{"g.txt": "g\n"}), forge.Mark{State: forge.Success})
		}, unchecked{on: true}, "no commit on main since its green commit"},
		"red only before the green commit": {func(t *testing.T, w *world) {
			_ = w.forge.SetMark(t.Context(), w.trunk(t), forge.Mark{State: forge.Failure})
			_ = w.forge.SetMark(t.Context(), w.push(t, map[string]string{"g.txt": "g\n"}), forge.Mark{State: forge.Success})
			w.push(t, map[string]string{"u.txt": "u\n"})
		}, unchecked{on: true}, "no commit on main since its green commit"},
		"no green commit": {func(t *testing.T, w *world) {
			_ = w.forge.SetMark(t.Context(), w.push(t, map[string]string{"r.txt": "r\n"}), forge.Mark{State: forge.Failure})
		}, unchecked{on: true}, "none of main's newest commits is green"},
		"a closed issue": {func(t *testing.T, w *world) { w.forge.SetIssue(6, false) }, unchecked{on: true, issue: "#6"},
			"#6 isn't an open issue"},
		"a missing issue": {func(*testing.T, *world) {}, unchecked{on: true, issue: "#7"}, "#7 isn't an open issue"},
		"no review": {func(t *testing.T, w *world) {
			redTrunk(t, w)
			w.l.reviewed = func(context.Context, string) error { return errors.New("no record") }
		}, unchecked{on: true}, "no finished review"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			c.setup(t, w)
			trunk := w.trunk(t)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			head := git(t, w.agent, "rev-parse", "HEAD")
			err := w.land(t, c.u)
			if err == nil || !strings.Contains(err.Error(), c.refusal) || !strings.Contains(err.Error(), "nothing was pushed") {
				t.Fatalf("land returned %v, want a refusal with %q", err, c.refusal)
			}
			if w.trunk(t) != trunk || git(t, w.agent, "rev-parse", "HEAD") != head || read(t, w.checks) != "" {
				t.Error("a refused landing pushed, moved HEAD or ran a check")
			}
		})
	}
}

// land's arguments: --unchecked alone or with '#N', before or after --message, and nothing else after it.
func TestParse(t *testing.T) {
	for _, c := range []struct {
		args  []string
		u     unchecked
		error string
	}{
		{args: []string{"--message", "m"}},
		{args: []string{"--message", "m", "--unchecked"}, u: unchecked{on: true}},
		{args: []string{"--unchecked", "--message", "m"}, u: unchecked{on: true}},
		{args: []string{"--message", "m", "--unchecked", "#12"}, u: unchecked{on: true, issue: "#12"}},
		{args: []string{"--unchecked", "#12", "--message", "m"}, u: unchecked{on: true, issue: "#12"}},
		{args: []string{"--unchecked", "12", "--message", "m"}, error: `open issue as '#N', not "12"`},
		{args: []string{"--message", "m", "#12"}, error: "usage"},
		{args: []string{"--unchecked", "#12", "#13", "--message", "m"}, error: "usage"},
		{args: []string{"--unchecked"}, error: "usage"},
	} {
		file, u, err := parse(c.args)
		switch {
		case c.error != "":
			if err == nil || !strings.Contains(err.Error(), c.error) {
				t.Errorf("parse(%q) = %v, want an error with %q", c.args, err, c.error)
			}
		case err != nil || file != "m" || u != c.u:
			t.Errorf("parse(%q) = %q, %+v, %v; want m, %+v", c.args, file, u, err, c.u)
		}
	}
}
