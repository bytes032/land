package landing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/provenance"
)

// hook makes script the agent's pre-push hook, run after unsetting the variables with which git points a hook's own
// git commands at the agent's repository.
func (w *world) hook(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(w.agent, ".git", "hooks", "pre-push")
	write(t, path, "#!/bin/sh\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX\n"+script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// loseRaces makes the agent's next len(moves) pushes lose their race: before each, its pre-push hook commits the next
// of moves, files by path, in the other clone and pushes them, so the trunk moves under the push.
func (w *world) loseRaces(t *testing.T, moves ...map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for i, files := range moves {
		if err := os.MkdirAll(filepath.Join(dir, strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
		for path, content := range files {
			write(t, filepath.Join(dir, strconv.Itoa(i), path), content)
		}
	}
	w.hook(t, fmt.Sprintf(`n=$(cat '%[1]s/count' 2>/dev/null || echo 0)
[ -d "%[1]s/$n" ] || exit 0
echo $((n + 1)) > '%[1]s/count'
cd '%[2]s' && git pull -q --ff-only origin main && cp -R "%[1]s/$n/." . && git add -A &&
	git commit -q --allow-empty -m "moved $n" && git push -q origin HEAD:main >&2
`, dir, w.other))
}

// dropAfterPush makes the agent's next git push update the trunk and then fail, as when the connection drops once the
// remote has the commit: a git on PATH runs the real one's push, then, with onTop, pushes onTop's files from the other
// clone, and exits as git does when the remote hung up.
func (w *world) dropAfterPush(t *testing.T, onTop map[string]string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, files := t.TempDir(), t.TempDir()
	then := ":"
	if onTop != nil {
		for path, content := range onTop {
			write(t, filepath.Join(files, path), content)
		}
		then = fmt.Sprintf(`(cd '%[1]s' && '%[2]s' pull -q --ff-only origin main && cp -R '%[3]s/.' . && '%[2]s' add -A &&
		'%[2]s' commit -q -m "on top" && '%[2]s' push -q origin HEAD:main)`, w.other, gitPath, files)
	}
	write(t, filepath.Join(bin, "git"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = push ] && [ ! -e '%[1]s' ]; then
	: > '%[1]s'
	'%[2]s' "$@" && %[3]s || exit
	echo "fatal: the remote end hung up unexpectedly" >&2
	exit 128
fi
exec '%[2]s' "$@"
`, filepath.Join(bin, "dropped"), gitPath, then))
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A push git reports failed after it reached the trunk is no lost race: land marks the commit it checked and pushes no
// copy of it, whether the trunk is at the commit or another push has already gone on top of it.
func TestAPushThatReachedTheTrunkIsNoLostRace(t *testing.T) {
	for name, onTop := range map[string]map[string]string{"at the landing": nil, "past the landing": {"o.txt": "o\n"}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			base := w.trunk(t)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			w.dropAfterPush(t, onTop)
			if err := w.land(t, unchecked{}); err != nil {
				t.Fatal(err)
			}
			landing := git(t, w.agent, "rev-parse", "HEAD")
			if subjects := git(t, w.remote, "log", "--format=%s", base+"..main"); strings.Count(subjects, "feat: add b") != 1 {
				t.Errorf("the trunk gained\n%s\nwant the landing once", subjects)
			}
			if parent := git(t, w.remote, "rev-parse", landing+"^"); parent != base {
				t.Errorf("the landing's parent is %s, want the trunk it was checked on, %s", parent, base)
			}
			if states := w.states(t, landing); !slices.Equal(states, []forge.State{forge.Success}) {
				t.Errorf("the landing's marks are %q, want success alone", states)
			}
			if checked := strings.Fields(read(t, w.checks)); len(checked) != 1 {
				t.Errorf("the box checked trees %q, want the landing's once", checked)
			}
		})
	}
}

// checkTwice is a check command that appends the tree it finds on the box to checks and passes, and passes or fails
// the second time it runs, by then, the check after the push.
func (w *world) checkTwice(t *testing.T, second string) string {
	count := filepath.Join(t.TempDir(), "count")
	return fmt.Sprintf(`git write-tree >> '%s'; n=$(cat '%s' 2>/dev/null || echo 0); echo $((n + 1)) > '%[2]s'; [ "$n" = 0 ] || { %s; }`,
		w.checks, count, second)
}

// A landing whose push loses the race, once or more, rebases onto the moved trunk and pushes at once, marked pending,
// then checks the pushed commit on the box and sets its final mark: success when the check passes; failure when it
// fails, with a commit comment the mark links to.
func TestALostRaceIsCheckedAfterThePush(t *testing.T) {
	for name, c := range map[string]struct {
		moves  int
		second string // the check after the push
		state  forge.State
	}{
		"passes":        {moves: 1, second: "true", state: forge.Success},
		"passes, twice": {moves: 2, second: "true", state: forge.Success},
		"fails":         {moves: 1, second: "echo broken; exit 3", state: forge.Failure},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.configure(t, func(s *config.Stages) { s.Check[0].Run = w.checkTwice(t, c.second) })
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			var moves []map[string]string
			for i := range c.moves {
				moves = append(moves, map[string]string{fmt.Sprintf("m%d.txt", i): "m\n"})
			}
			w.loseRaces(t, moves...)
			err := w.land(t, unchecked{})
			if c.state == forge.Success && err != nil || c.state == forge.Failure && (err == nil || !strings.Contains(err.Error(), "main is red there")) {
				t.Fatalf("land returned %v", err)
			}
			pushed := w.trunk(t)
			if moved := git(t, w.remote, "log", "-1", "--format=%s", "main^"); moved != fmt.Sprintf("moved %d", c.moves-1) {
				t.Errorf("the landing sits on %q, want on the last commit that moved the trunk", moved)
			}
			if b := git(t, w.remote, "show", "main:b.txt"); b != "b" {
				t.Errorf("the pushed commit's b.txt is %q", b)
			}
			if states := w.states(t, pushed); !slices.Equal(states, []forge.State{c.state, forge.Pending}) {
				t.Errorf("the pushed commit's marks are %q, newest first; want pending, then %s", states, c.state)
			}
			checked := strings.Fields(read(t, w.checks))
			if tree := git(t, w.remote, "rev-parse", "main^{tree}"); len(checked) != 2 || checked[0] == tree || checked[1] != tree {
				t.Errorf("the box checked trees %q; want the landing's before the push, then the pushed %s after it", checked, tree)
			}
			if comments := w.forge.Comments(pushed); c.state == forge.Failure && (len(comments) != 1 || !strings.Contains(comments[0], "broken") || w.newest(t, pushed).TargetURL == "") {
				t.Errorf("the failure's comments are %q, linked as %q; want one, linked", comments, w.newest(t, pushed).TargetURL)
			}
			if head, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); head != pushed || status != "" {
				t.Errorf("the worktree is at %s with changes %q, want at the pushed commit, clean", head, status)
			}
		})
	}
}

// A lost race whose only conflict is a derived file takes the trunk's side, regenerates it from both sides' sources on
// the box, checks the regenerated commit and only then pushes it, marked success with no pending mark. An unchecked
// landing regenerates the same, but pushes with no check, before or after, and gets the mark it names.
func TestALostRaceWithADerivedConflictIsCheckedBeforeThePush(t *testing.T) {
	for name, u := range map[string]unchecked{"checked": {}, "unchecked": {on: true, issue: "#5"}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.forge.SetIssue(5, true)
			w.configure(t, func(s *config.Stages) { s.Regenerate = regenerations(w, filepath.Join(t.TempDir(), "untouched")) })
			w.push(t, map[string]string{"x.src": "x\n", "y.src": "y\n", "all.out": "x\ny\n"})
			git(t, w.agent, "pull", "-q", "--ff-only", "origin", "main")
			commit(t, w.agent, "agent's", map[string]string{"y.src": "Y\n", "all.out": "x\nY\n"})
			w.loseRaces(t, map[string]string{"x.src": "X\n", "all.out": "X\ny\n"})
			if err := w.land(t, u); err != nil {
				t.Fatal(err)
			}
			pushed := w.trunk(t)
			if all, moved := git(t, w.remote, "show", "main:all.out"), git(t, w.remote, "log", "-1", "--format=%s", "main^"); all != "X\nY" || moved != "moved 0" {
				t.Errorf("the pushed all.out is %q on %q; want it regenerated from both sides, on the moved trunk", all, moved)
			}
			checked, tree := strings.Fields(read(t, w.checks)), git(t, w.remote, "rev-parse", "main^{tree}")
			if u.on && len(checked) != 0 || !u.on && (len(checked) != 2 || checked[1] != tree) {
				t.Errorf("the box checked trees %q; want the regenerated commit's, %s, last, and none unchecked", checked, tree)
			}
			if states := w.states(t, pushed); !slices.Equal(states, []forge.State{forge.Success}) {
				t.Errorf("the pushed commit's marks are %q, want success alone: no check is left for after the push", states)
			}
			if trailer, err := provenance.Trailer(t.Context(), w.remote, pushed, provenance.Unchecked); err != nil || trailer != u.issue {
				t.Errorf("the pushed commit's Unchecked trailer is %q, %v; want %q", trailer, err, u.issue)
			}
		})
	}
}

// A lost race that conflicts in a lockfile, or in a file with no regenerate entry, stops land with nothing pushed or
// marked, the worktree clean.
func TestAConflictInALostRaceStopsLand(t *testing.T) {
	for name, c := range map[string]struct {
		trunk, agent, move map[string]string
		stops              string
	}{
		"a lockfile": {
			trunk: map[string]string{"deps.txt": "v1\n", "deps.lock": "locked\nv1\n"},
			agent: map[string]string{"deps.txt": "v2\n", "deps.lock": "locked\nv2\n"},
			move:  map[string]string{"deps.txt": "v3\n", "deps.lock": "locked\nv3\n"},
			stops: "conflicts in deps.lock",
		},
		"a file with no entry": {
			trunk: map[string]string{"c.txt": "c\n"},
			agent: map[string]string{"c.txt": "w\n"},
			move:  map[string]string{"c.txt": "m\n"},
			stops: "conflicts in c.txt",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.configure(t, func(s *config.Stages) { s.Regenerate = regenerations(w, filepath.Join(t.TempDir(), "untouched")) })
			w.push(t, c.trunk)
			git(t, w.agent, "pull", "-q", "--ff-only", "origin", "main")
			commit(t, w.agent, "agent's", c.agent)
			w.loseRaces(t, c.move)
			err := w.land(t, unchecked{})
			if err == nil || !strings.Contains(err.Error(), c.stops) || !strings.Contains(err.Error(), "nothing was pushed") {
				t.Fatalf("land returned %v, want a stop with %q", err, c.stops)
			}
			head := git(t, w.agent, "rev-parse", "HEAD")
			if moved := git(t, w.remote, "log", "-1", "--format=%s", "main"); moved != "moved 0" || len(w.states(t, head)) != 0 {
				t.Errorf("the trunk's tip is %q and the landing's marks %q; want the move, and none", moved, w.states(t, head))
			}
			if status := git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); status != "" {
				t.Errorf("the stop left changes %q", status)
			}
		})
	}
}

// pendingRun is GitHub where the commit's CI run completes while its mark is pending, as when the check after the push
// outlasts the gate's timeout.
type pendingRun struct {
	*forge.Memory
	run int64
}

func (f *pendingRun) SetMark(ctx context.Context, sha string, m forge.Mark) error {
	if err := f.Memory.SetMark(ctx, sha, m); err != nil || m.State != forge.Pending {
		return err
	}
	f.run = f.AddRun(sha, "completed", "failure")
	return nil
}

// A lost race whose check outlasts the gate reruns the CI run that completed meanwhile, once the final mark is set.
func TestALostRaceWhoseCheckOutlastsTheGateRerunsItsRun(t *testing.T) {
	w := newWorld(t)
	f := &pendingRun{Memory: w.forge}
	w.l.forge = f
	commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
	w.loseRaces(t, map[string]string{"m.txt": "m\n"})
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatal(err)
	}
	if states := w.states(t, w.trunk(t)); !slices.Equal(states, []forge.State{forge.Success, forge.Pending}) {
		t.Errorf("the marks are %q, newest first; want pending, then success", states)
	}
	if reruns := w.forge.Reruns(); f.run == 0 || !slices.Equal(reruns, []int64{f.run}) {
		t.Errorf("reran %v, want the run %d that completed while the mark was pending", reruns, f.run)
	}
}

// A push refused while the trunk stayed where it was, as by a hook, is no lost race: land fails, naming it, with
// nothing pushed or marked.
func TestAPushRefusedWithoutARaceFails(t *testing.T) {
	w := newWorld(t)
	trunk := w.trunk(t)
	commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
	w.hook(t, "echo refused by a hook >&2; exit 1\n")
	err := w.land(t, unchecked{})
	if err == nil || !strings.Contains(err.Error(), "didn't move") || !strings.Contains(err.Error(), "refused by a hook") {
		t.Fatalf("land returned %v, want the refusal named", err)
	}
	if head := git(t, w.agent, "rev-parse", "HEAD"); w.trunk(t) != trunk || len(w.states(t, head)) != 0 {
		t.Error("a refused push pushed, or marked")
	}
}
