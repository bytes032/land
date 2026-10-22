package landing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/docs"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/provenance"
	"github.com/bytes032/land/internal/review"
)

// root is the omp session the tests' transcripts belong to, and session its folder.
const (
	root    = "01a15d57-5605-7160-972b-d21edf620e25"
	session = "2026-10-21T07-22-47-173Z_" + root
)

// world is a bare remote whose trunk is main, with this land version's LANDING.md and a .land.yaml whose one check,
// test, appends the tree it finds on the box to checks; the agent's clone, which lands, with its message file and its
// omp transcript, a subagent's, Lander; another clone, which stands for pushes from elsewhere; GitHub, in memory; and
// the box, of whichever worktree claims it; released are the worktrees whose box land released.
type world struct {
	remote, agent, other, message, checks string
	forge                                 *forge.Memory
	box                                   *box
	released                              []string
	l                                     *lander
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// Neither the machine's git config nor its hooks reach the scratch repositories.
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "HOME": t.TempDir(),
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(name, value)
	}
	dir := t.TempDir()
	w := &world{
		remote: filepath.Join(dir, "remote.git"), agent: filepath.Join(dir, "agent"), other: filepath.Join(dir, "other"),
		message: filepath.Join(dir, "message.txt"), checks: filepath.Join(dir, "checks.log"), forge: &forge.Memory{},
	}
	git(t, dir, "init", "-q", "--bare", "-b", "main", w.remote)
	git(t, dir, "clone", "-q", w.remote, w.other)
	if err := docs.Write(w.other); err != nil { // the rules of this land version, which a landing requires
		t.Fatal(err)
	}
	commit(t, w.other, "base", map[string]string{"a.txt": "a\n", config.File: landYAML(w.stages())})
	git(t, w.other, "push", "-q", "origin", "HEAD:main")
	git(t, dir, "clone", "-q", w.remote, w.agent)
	transcript := filepath.Join(dir, "sessions/-wt", session, "Lander.jsonl")
	write(t, transcript, "{}\n")
	t.Setenv("PI_SESSION_FILE", transcript)
	write(t, w.message, "feat: add b\n\nWhy.\n\nCloses #7\n")
	w.box = &box{worktree: w.agent, dir: filepath.Join(dir, "box")}
	w.l = &lander{
		root:     w.agent,
		cfg:      load(t, w.agent),
		forge:    w.forge,
		reviewed: func(context.Context, string) error { return nil },
		claim: func(_ context.Context, root string, _ *config.Config, _ string) (boxes.Box, error) {
			w.box.worktree = root
			return w.box, nil
		},
		release: func(_ context.Context, root string, _ *config.Config) error {
			w.released = append(w.released, root)
			return nil
		},
		out: io.Discard,
	}
	return w
}

// stages are the world's stages until a test configures others: one check, test, appending the tree it finds to checks.
func (w *world) stages() config.Stages {
	return config.Stages{Check: config.Checks{{Name: "test", Run: "git write-tree >> '" + w.checks + "'"}}}
}

// landYAML is a .land.yaml with stages, for the trunk main and messages with a Closes line.
func landYAML(stages config.Stages) string {
	quote := func(v any) string {
		data, err := json.Marshal(v) // JSON is YAML
		if err != nil {
			panic(err)
		}
		return string(data)
	}
	var b strings.Builder
	b.WriteString("version: 0.1.0\ntrunk: main\nmessage: {require: ['^Closes #\\d+$']}\ngate: {timeout: 30m, unverified: fail}\n" +
		"box: {type: cpx42}\nstages:\n")
	if stages.Prepare != "" {
		fmt.Fprintf(&b, "  prepare: %s\n", quote(stages.Prepare))
	}
	b.WriteString("  check:\n")
	for _, c := range stages.Check {
		fmt.Fprintf(&b, "    %s: {run: %s, paths: %s, ignore: %s}\n", quote(c.Name), quote(c.Run), quote(c.Paths), quote(c.Ignore))
	}
	if len(stages.Regenerate) > 0 {
		b.WriteString("  regenerate:\n")
		for _, r := range stages.Regenerate {
			fmt.Fprintf(&b, "    - {kind: %s, path: %s, run: %s}\n", quote(r.Kind), quote(r.Path), quote(r.Run))
		}
	}
	return b.String()
}

// configure pushes to the trunk a .land.yaml with the world's stages as edit leaves them, as a change to the
// repository's configuration would. The agent's clone has it only once it pulls.
func (w *world) configure(t *testing.T, edit func(*config.Stages)) {
	t.Helper()
	stages := w.stages()
	edit(&stages)
	w.push(t, map[string]string{config.File: landYAML(stages)})
}

func load(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// land runs land in the agent's clone, which reads its .land.yaml first, as the command does.
func (w *world) land(t *testing.T, u unchecked) error {
	t.Helper()
	w.l.cfg = load(t, w.agent)
	return w.l.land(t.Context(), w.message, u)
}

// push commits files in the other clone and pushes them to the trunk, as another landing would, returning the commit.
func (w *world) push(t *testing.T, files map[string]string) string {
	t.Helper()
	git(t, w.other, "pull", "-q", "--ff-only", "origin", "main")
	commit(t, w.other, "elsewhere", files)
	git(t, w.other, "push", "-q", "origin", "HEAD:main")
	return git(t, w.other, "rev-parse", "HEAD")
}

func (w *world) trunk(t *testing.T) string {
	t.Helper()
	return git(t, w.remote, "rev-parse", "main")
}

// box is a fake box in a directory of its own. Run syncs the worktree's files that git doesn't ignore into it, uncommitted
// ones included, as crabbox does, and leftover, files the sync left behind; runs the command there; then copies each
// fetch path back over the worktree's. With stop set, it returns stop at once, as a box that stopped before the command.
type box struct {
	worktree, dir string
	leftover      map[string]string
	stop          error
}

func (b *box) Run(ctx context.Context, fetch []string, argv ...string) error {
	if b.stop != nil {
		return b.stop
	}
	if err := os.RemoveAll(b.dir); err != nil {
		return err
	}
	files, err := exec.CommandContext(ctx, "git", "-C", b.worktree, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return err
	}
	for _, path := range strings.Split(strings.TrimRight(string(files), "\x00"), "\x00") {
		info, err := os.Stat(filepath.Join(b.worktree, path))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(b.worktree, path))
		if err == nil {
			err = os.MkdirAll(filepath.Dir(filepath.Join(b.dir, path)), 0o755)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(b.dir, path), data, info.Mode())
		}
		if err != nil {
			return err
		}
	}
	for path, content := range b.leftover {
		if err := os.WriteFile(filepath.Join(b.dir, path), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := exec.CommandContext(ctx, "git", "init", "-q", b.dir).Run(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = b.dir
	ran := cmd.Run()
	for _, path := range fetch {
		if err := os.RemoveAll(filepath.Join(b.worktree, path)); err != nil {
			return err
		}
		src, dst := filepath.Join(b.dir, path), filepath.Join(b.worktree, path)
		info, err := os.Stat(src)
		switch {
		case err != nil:
		case info.IsDir():
			err = os.CopyFS(dst, os.DirFS(src))
		default:
			var data []byte
			if data, err = os.ReadFile(src); err == nil {
				err = os.WriteFile(dst, data, info.Mode())
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return ran
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commit writes each path's content in dir and commits them all.
func commit(t *testing.T, dir, message string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		write(t, filepath.Join(dir, path), content)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

// The work of two commits, made while the trunk moved on, lands as one commit on the moved trunk, with the file's
// message and the Agent-Session and Agent trailers, after the review of the change from where it started and the
// check on the box at its exact tree; it is marked success, and the worktree is at it, clean.
func TestALandingIsOneCheckedCommitWithTrailersAndASuccessMark(t *testing.T) {
	w := newWorld(t)
	started := w.trunk(t)
	commit(t, w.agent, "one", map[string]string{"b.txt": "b\n"})
	commit(t, w.agent, "two", map[string]string{"b.txt": "b2\n", "c/d.txt": "d\n"})
	moved := w.push(t, map[string]string{"e.txt": "e\n"})
	var reviewed string
	w.l.reviewed = func(_ context.Context, base string) error {
		reviewed = base
		return nil
	}
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatal(err)
	}
	pushed := w.trunk(t)
	if parent := git(t, w.remote, "rev-parse", "main^"); parent != moved {
		t.Errorf("the landing's parent is %s, want the moved trunk %s", parent, moved)
	}
	if files, b := git(t, w.remote, "ls-tree", "-r", "--name-only", "main"), git(t, w.remote, "show", "main:b.txt"); files != ".land.yaml\nLANDING.md\na.txt\nb.txt\nc/d.txt\ne.txt" || b != "b2" {
		t.Errorf("the landing holds %q with b.txt %q, want the work on the moved trunk", files, b)
	}
	if got, want := git(t, w.remote, "log", "-1", "--format=%B", "main"), "feat: add b\n\nWhy.\n\nCloses #7\n\nAgent-Session: "+root+"\nAgent: Lander"; got != want {
		t.Errorf("the message is\n%s\nwant\n%s", got, want)
	}
	for key, want := range map[string]string{provenance.AgentSession: root, provenance.Agent: "Lander"} {
		if got, err := provenance.Trailer(t.Context(), w.remote, pushed, key); err != nil || got != want {
			t.Errorf("git reads the %s trailer as %q, %v; want %q", key, got, err, want)
		}
	}
	if marks, _ := w.forge.Marks(t.Context(), pushed); len(marks) != 1 || marks[0].State != forge.Success {
		t.Errorf("the landing's marks are %+v, want one success", marks)
	}
	if checked, tree := read(t, w.checks), git(t, w.remote, "rev-parse", "main^{tree}"); checked != tree+"\n" {
		t.Errorf("the box checked trees %q, want the landing's %s once", checked, tree)
	}
	if reviewed != started {
		t.Errorf("the review was asked for the change from %s, want from %s, where the work started", reviewed, started)
	}
	if head, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); head != pushed || status != "" {
		t.Errorf("the worktree is at %s with changes %q, want at the landing, clean", head, status)
	}
	if ours := git(t, w.agent, "rev-parse", "refs/worktree/main"); ours != moved {
		t.Errorf("the trunk was fetched into refs/worktree/main as %s, want %s", ours, moved)
	}
}

// land refuses, before it pushes, marks, checks or moves anything: uncommitted changes, untracked files, a message
// without a required line or with land's own trailers, a session layout omp doesn't write, and a change with no
// finished review.
func TestLandRefusesBeforeTouchingAnything(t *testing.T) {
	for name, c := range map[string]struct {
		setup   func(t *testing.T, w *world)
		refusal string
	}{
		"an uncommitted change": {func(t *testing.T, w *world) { write(t, filepath.Join(w.agent, "a.txt"), "edited\n") }, "uncommitted changes"},
		"an untracked file":     {func(t *testing.T, w *world) { write(t, filepath.Join(w.agent, "new.txt"), "new\n") }, "untracked files"},
		"no required line": {func(t *testing.T, w *world) { write(t, w.message, "feat: add b\n\nCloses #7 and #8\n") },
			`no line matching "^Closes #\\d+$"`},
		"land's own trailers": {func(t *testing.T, w *world) { write(t, w.message, "feat: add b\n\nCloses #7\nagent-session: forged\n") },
			"which land writes itself"},
		"an unknown omp layout": {func(t *testing.T, w *world) {
			transcript := filepath.Join(t.TempDir(), "sessions/-wt/Lander.jsonl")
			write(t, transcript, "{}\n")
			t.Setenv("PI_SESSION_FILE", transcript)
		}, "no omp session folder"},
		"no omp session": {func(t *testing.T, w *world) { t.Setenv("PI_SESSION_FILE", "") }, "PI_SESSION_FILE is unset"},
		"no review": {func(t *testing.T, w *world) {
			w.l.reviewed = func(context.Context, string) error { return errors.New("no record") }
		}, "no finished review"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			base := w.trunk(t)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			head := git(t, w.agent, "rev-parse", "HEAD")
			c.setup(t, w)
			if err := w.land(t, unchecked{}); err == nil || !strings.Contains(err.Error(), c.refusal) {
				t.Fatalf("land returned %v, want a refusal with %q", err, c.refusal)
			}
			if w.trunk(t) != base || git(t, w.agent, "rev-parse", "HEAD") != head || read(t, w.checks) != "" {
				t.Error("a refused landing pushed, moved HEAD or ran a check")
			}
			if marks, _ := w.forge.Marks(t.Context(), head); len(marks) != 0 {
				t.Errorf("a refused landing set marks %+v", marks)
			}
		})
	}
}

// land refuses a LANDING.md that is missing or isn't this land version's, naming land docs, before it pushes, marks,
// checks or moves anything.
func TestLandRefusesRulesThatArentThisVersions(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, w *world){
		"edited": func(t *testing.T, w *world) {
			commit(t, w.agent, "my own rules", map[string]string{docs.File: "Land as you like.\n"})
		},
		"missing": func(t *testing.T, w *world) {
			git(t, w.agent, "rm", "-q", docs.File)
			git(t, w.agent, "commit", "-q", "-m", "no rules")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			base := w.trunk(t)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			edit(t, w)
			head := git(t, w.agent, "rev-parse", "HEAD")
			if err := w.land(t, unchecked{}); err == nil || !strings.Contains(err.Error(), "run land docs") {
				t.Fatalf("land returned %v, want a refusal naming land docs", err)
			}
			if marks, _ := w.forge.Marks(t.Context(), head); w.trunk(t) != base || git(t, w.agent, "rev-parse", "HEAD") != head || read(t, w.checks) != "" || len(marks) != 0 {
				t.Error("a refused landing pushed, moved HEAD, ran a check or set a mark")
			}
		})
	}
}

// A box whose tree isn't the landing's, a check that fails, and a box that stops before the command each stop land
// before the push with no mark, naming why; only the first leaves no check run.
func TestNoVerdictOrAFailurePushesNothing(t *testing.T) {
	for name, c := range map[string]struct {
		setup   func(t *testing.T, w *world)
		refusal string
		checked bool
	}{
		"a tree mismatch": {func(t *testing.T, w *world) { w.box.leftover = map[string]string{"stale.txt": "a snapshot's\n"} },
			"the box stopped at tree", false},
		"a failed check": {func(t *testing.T, w *world) {
			w.configure(t, func(s *config.Stages) { s.Check[0].Run += "; echo broken; exit 3" })
		}, "the check at the push failed in test", true},
		"a box that stopped": {func(t *testing.T, w *world) {
			w.box.stop = &boxes.StageError{Stage: "prepare", Err: errors.New("exit status 1")}
		}, "the box stopped at prepare", false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
			c.setup(t, w)
			base := w.trunk(t)
			err := w.land(t, unchecked{})
			if err == nil || !strings.Contains(err.Error(), c.refusal) || !strings.Contains(err.Error(), "nothing was pushed") {
				t.Fatalf("land returned %v, want an error with %q", err, c.refusal)
			}
			landing := git(t, w.agent, "rev-parse", "HEAD")
			if marks, _ := w.forge.Marks(t.Context(), landing); w.trunk(t) != base || len(marks) != 0 {
				t.Errorf("land pushed, or set marks %+v", marks)
			}
			if checked := read(t, w.checks) != ""; checked != c.checked {
				t.Errorf("a check ran: %t, want %t", checked, c.checked)
			}
		})
	}
}

// A conflict with the trunk stops land before the box, with HEAD, the index and the worktree as they were, and the
// remote-tracking branch too, since land fetches the trunk into the worktree's own ref.
func TestAConflictStopsLandWithTheWorktreeAsItWas(t *testing.T) {
	w := newWorld(t)
	tracking := git(t, w.agent, "rev-parse", "refs/remotes/origin/main")
	commit(t, w.agent, "agent's", map[string]string{"a.txt": "w\n"})
	head := git(t, w.agent, "rev-parse", "HEAD")
	moved := w.push(t, map[string]string{"a.txt": "m\n"})
	if err := w.land(t, unchecked{}); err == nil || !strings.Contains(err.Error(), "conflicts in a.txt") {
		t.Fatalf("land went on past the conflict: %v", err)
	}
	if now, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); now != head || status != "" {
		t.Errorf("the conflict left HEAD at %s with changes %q, want %s and none", now, status, head)
	}
	if ours, now := git(t, w.agent, "rev-parse", "refs/worktree/main"), git(t, w.agent, "rev-parse", "refs/remotes/origin/main"); ours != moved || now != tracking {
		t.Errorf("refs/worktree/main is %s and origin/main %s; want the fetched trunk %s, and origin/main left at %s", ours, now, moved, tracking)
	}
	if w.trunk(t) != moved || read(t, w.checks) != "" {
		t.Error("land pushed, or checked, past a conflict")
	}
}

// ran is a check command that appends name to log, where the tests read which checks ran.
func ran(log, name string) string { return "echo " + name + " >> '" + log + "'" }

// A docs-only change runs only the checks without a matching ignore; any other file runs them all; a check with paths
// runs only when the change touches one.
func TestTheChangeSelectsTheChecks(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"docs only":          {map[string]string{"README.md": "r\n", "docs/guide.md": "g\n"}, "lint\n"},
		"docs and code":      {map[string]string{"README.md": "r\n", "main.go": "package main\n"}, "lint\ntest\n"},
		"under a check path": {map[string]string{"core/x.go": "package core\n"}, "lint\ntest\ncore\n"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.configure(t, func(s *config.Stages) {
				s.Check = config.Checks{
					{Name: "lint", Run: ran(w.checks, "lint")},
					{Name: "test", Run: ran(w.checks, "test"), Ignore: []string{".md"}},
					{Name: "core", Run: ran(w.checks, "core"), Paths: []string{"core/"}},
				}
			})
			commit(t, w.agent, "work", c.files)
			if err := w.land(t, unchecked{}); err != nil {
				t.Fatal(err)
			}
			if got := read(t, w.checks); got != c.want {
				t.Errorf("the checks that ran are %q, want %q", got, c.want)
			}
			if marks, _ := w.forge.Marks(t.Context(), w.trunk(t)); len(marks) != 1 || marks[0].State != forge.Success {
				t.Errorf("the landing's marks are %+v, want one success", marks)
			}
		})
	}
}

// regenerations is a world's regenerate stage: a lockfile written from its manifest, a derived directory written from
// the lockfile (so it is right only when the lockfile was regenerated first), a derived file written from two sources,
// and a lockfile no test changes, whose command must never run.
func regenerations(w *world, untouched string) []config.Regenerate {
	return []config.Regenerate{
		{Kind: config.Derived, Path: "gen/", Run: "rm -rf gen && mkdir gen && cat deps.lock > gen/out.txt"},
		{Kind: config.Lockfile, Path: "deps.lock", Run: "{ echo locked; cat deps.txt; } > deps.lock"},
		{Kind: config.Derived, Path: "all.out", Run: "cat x.src y.src > all.out"},
		{Kind: config.Lockfile, Path: "web/other.lock", Run: "echo ran >> '" + untouched + "'"},
	}
}

// A changed lockfile and a changed derived directory are regenerated on the box, lockfiles first, and amended into the
// one landing commit, which the box then checks at its exact tree.
func TestChangedLockfilesAndDerivedFilesAreRegeneratedAndAmended(t *testing.T) {
	w := newWorld(t)
	untouched := filepath.Join(t.TempDir(), "untouched")
	w.configure(t, func(s *config.Stages) { s.Regenerate = regenerations(w, untouched) })
	trunk := w.trunk(t)
	commit(t, w.agent, "bump", map[string]string{"deps.txt": "v2\n", "deps.lock": "stale\n", "gen/stale.txt": "stale\n"})
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatal(err)
	}
	if parent := git(t, w.remote, "rev-parse", "main^"); parent != trunk {
		t.Errorf("the landing's parent is %s, want the trunk %s: the regeneration must amend the one commit", parent, trunk)
	}
	if files := git(t, w.remote, "ls-tree", "-r", "--name-only", "main"); files != ".land.yaml\nLANDING.md\na.txt\ndeps.lock\ndeps.txt\ngen/out.txt" {
		t.Errorf("the landing holds %q, want gen/ as its command rewrote it", files)
	}
	for path, want := range map[string]string{"deps.lock": "locked\nv2", "gen/out.txt": "locked\nv2"} {
		if got := git(t, w.remote, "show", "main:"+path); got != want {
			t.Errorf("the landing's %s is %q, want %q", path, got, want)
		}
	}
	if got := git(t, w.remote, "log", "-1", "--format=%B", "main"); !strings.HasPrefix(got, "feat: add b\n") || !strings.HasSuffix(got, "Agent: Lander") {
		t.Errorf("the amended landing's message is\n%s", got)
	}
	if checked, tree := read(t, w.checks), git(t, w.remote, "rev-parse", "main^{tree}"); checked != tree+"\n" {
		t.Errorf("the box checked trees %q, want the amended landing's %s", checked, tree)
	}
	if head, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); head != w.trunk(t) || status != "" {
		t.Errorf("the worktree is at %s with changes %q, want at the landing, clean", head, status)
	}
	if read(t, untouched) != "" {
		t.Error("an unchanged lockfile was regenerated")
	}
}

// A regeneration that fails stops land before the check with nothing pushed, and leaves the worktree clean.
func TestAFailedRegenerationPushesNothing(t *testing.T) {
	w := newWorld(t)
	w.configure(t, func(s *config.Stages) {
		s.Regenerate = []config.Regenerate{{Kind: config.Lockfile, Path: "deps.lock", Run: "echo half > deps.lock; exit 4"}}
	})
	trunk := w.trunk(t)
	commit(t, w.agent, "bump", map[string]string{"deps.txt": "v2\n", "deps.lock": "stale\n"})
	if err := w.land(t, unchecked{}); err == nil || !strings.Contains(err.Error(), "regenerating deps.lock on the box failed") {
		t.Fatalf("land returned %v, want the failed regeneration named", err)
	}
	if w.trunk(t) != trunk || read(t, w.checks) != "" {
		t.Error("land pushed, or checked, past a failed regeneration")
	}
	if status := git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Errorf("the failed regeneration left changes %q", status)
	}
}

// A stop after a regeneration, with nothing pushed, puts HEAD back at the squashed commit, before the amend: the change
// is the reviewed one again, so landing again needs no new review. Here the box dies during the check that follows the
// lockfile's regeneration, and the next landing lands, regenerating afresh.
func TestAStopAfterARegenerationKeepsTheReview(t *testing.T) {
	w := newWorld(t)
	died := filepath.Join(t.TempDir(), "died")
	w.configure(t, func(s *config.Stages) {
		s.Regenerate = []config.Regenerate{{Kind: config.Lockfile, Path: "deps.lock", Run: "{ echo locked; cat deps.txt; } > deps.lock"}}
		// The first check kills the box's command before it writes a result, as a box that dies does.
		s.Check[0].Run = "[ -e '" + died + "' ] || { : > '" + died + "'; kill -9 $PPID; }; " + s.Check[0].Run
	})
	trunk := w.trunk(t)
	commit(t, w.agent, "bump", map[string]string{"deps.txt": "v2\n", "deps.lock": "stale\n"})
	key, err := review.Key(t.Context(), w.agent, git(t, w.agent, "rev-parse", "HEAD^"), nil) // the agent's review
	if err != nil {
		t.Fatal(err)
	}
	w.l.reviewed = func(ctx context.Context, base string) error {
		if now, err := review.Key(ctx, w.agent, base, nil); err != nil || now != key {
			return fmt.Errorf("no finished review of the change (review key %s, reviewed %s): %v", now, key, err)
		}
		return nil
	}
	err = w.land(t, unchecked{})
	if err == nil || !strings.Contains(err.Error(), "nothing was pushed") || w.trunk(t) != trunk {
		t.Fatalf("land returned %v and moved the trunk to %s; want a stop with nothing pushed", err, w.trunk(t))
	}
	if lock, status := git(t, w.agent, "show", "HEAD:deps.lock"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); lock != "stale" || status != "" {
		t.Errorf("the stop left HEAD's deps.lock %q and changes %q; want the squashed commit's, as reviewed, and none", lock, status)
	}
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatalf("landing again: %v", err)
	}
	if lock := git(t, w.remote, "show", "main:deps.lock"); lock != "locked\nv2" || len(w.states(t, w.trunk(t))) != 1 {
		t.Errorf("the trunk's deps.lock is %q, marked %q; want the regenerated one, marked once", lock, w.states(t, w.trunk(t)))
	}
}

// The landing commit is checked with its own .land.yaml, not the one the agent's worktree held when it landed: a check
// and a prepare stage the trunk added after the work branched apply to it, as land recheck would apply them.
func TestTheLandingCommitsOwnConfigurationChecksIt(t *testing.T) {
	w := newWorld(t)
	commit(t, w.agent, "add b", map[string]string{"b.txt": "b\n"})
	w.configure(t, func(s *config.Stages) {
		s.Prepare = "the trunk's prepare"
		s.Check = append(s.Check, config.Check{Name: "schema", Run: ran(w.checks, "schema")})
	})
	var prepares []string
	claim := w.l.claim
	w.l.claim = func(ctx context.Context, root string, cfg *config.Config, typ string) (boxes.Box, error) {
		prepares = append(prepares, cfg.Stages.Prepare) // the box runs it before the command
		return claim(ctx, root, cfg, typ)
	}
	if err := w.land(t, unchecked{}); err != nil {
		t.Fatal(err)
	}
	if checked, tree := read(t, w.checks), git(t, w.remote, "rev-parse", "main^{tree}"); checked != tree+"\nschema\n" {
		t.Errorf("the checks that ran wrote %q, want the landing's tree, then schema", checked)
	}
	if !slices.Equal(prepares, []string{"the trunk's prepare"}) {
		t.Errorf("the box was claimed with prepare %q, want the landing commit's", prepares)
	}
}

// A rebase whose only conflicts are derived files takes the trunk's side of each, regenerates them on the box, and
// lands; a conflicted lockfile, or a conflict in a path with no regenerate entry, stops land before the box with
// nothing pushed, naming only the paths that stop it.
func TestConflictsFollowTheirKind(t *testing.T) {
	for name, c := range map[string]struct {
		agent, trunk map[string]string
		stops        string // "" when land lands
	}{
		"a derived file": {
			agent: map[string]string{"y.src": "Y\n", "all.out": "x\nY\n"},
			trunk: map[string]string{"x.src": "X\n", "all.out": "X\ny\n"},
		},
		"a lockfile": {
			agent: map[string]string{"deps.txt": "v2\n", "deps.lock": "locked\nv2\n"},
			trunk: map[string]string{"deps.txt": "v3\n", "deps.lock": "locked\nv3\n"},
			stops: "conflicts in deps.lock, deps.txt,",
		},
		"a derived file and a path with no entry": {
			agent: map[string]string{"a.txt": "w\n", "all.out": "x\nY\n"},
			trunk: map[string]string{"a.txt": "m\n", "all.out": "X\ny\n"},
			stops: "conflicts in a.txt,",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			untouched := filepath.Join(t.TempDir(), "untouched")
			w.configure(t, func(s *config.Stages) { s.Regenerate = regenerations(w, untouched) })
			w.push(t, map[string]string{"x.src": "x\n", "y.src": "y\n", "all.out": "x\ny\n", "deps.txt": "v1\n", "deps.lock": "locked\nv1\n"})
			git(t, w.agent, "pull", "-q", "--ff-only", "origin", "main")
			commit(t, w.agent, "agent's", c.agent)
			head := git(t, w.agent, "rev-parse", "HEAD")
			moved := w.push(t, c.trunk)
			err := w.land(t, unchecked{})
			if c.stops != "" {
				if err == nil || !strings.Contains(err.Error(), c.stops) || !strings.Contains(err.Error(), "nothing was pushed") {
					t.Fatalf("land returned %v, want a stop with %q", err, c.stops)
				}
				if now, status := git(t, w.agent, "rev-parse", "HEAD"), git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); now != head || status != "" {
					t.Errorf("the conflict left HEAD at %s with changes %q, want %s and none", now, status, head)
				}
				if w.trunk(t) != moved || read(t, w.checks) != "" {
					t.Error("land pushed, or checked, past a conflict")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parent := git(t, w.remote, "rev-parse", "main^"); parent != moved {
				t.Errorf("the landing's parent is %s, want the moved trunk %s", parent, moved)
			}
			if got := git(t, w.remote, "show", "main:all.out"); got != "X\nY" {
				t.Errorf("the landing's all.out is %q, want it regenerated from both sides' sources", got)
			}
			if checked, tree := read(t, w.checks), git(t, w.remote, "rev-parse", "main^{tree}"); checked != tree+"\n" {
				t.Errorf("the box checked trees %q, want the landing's %s", checked, tree)
			}
			if status := git(t, w.agent, "status", "--porcelain", "--untracked-files=all"); status != "" {
				t.Errorf("the worktree has changes %q, want none", status)
			}
		})
	}
}
