package check_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/check"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
)

// box is a fake box: dir, a scratch repository, stands for the box's copy of the worktree. Run runs the command there
// for real, unless skip is set, then copies each fetch path back over root's, symlinks as symlinks, as a box command
// does, and returns err. A ctx ending the run ends the command with SIGTERM.
type box struct {
	dir, root string
	skip      bool
	err       error
}

func (b *box) Run(ctx context.Context, fetch []string, argv ...string) error {
	var ran error
	if !b.skip {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir, cmd.Stderr = b.dir, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		ran = cmd.Run()
	}
	for _, path := range fetch {
		if err := os.RemoveAll(filepath.Join(b.root, path)); err != nil {
			return err
		}
		if exists(filepath.Join(b.dir, path)) {
			if out, err := exec.Command("cp", "-Rp", filepath.Join(b.dir, path), filepath.Join(b.root, path)).CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, out)
			}
		}
	}
	return errors.Join(b.err, ran)
}

// newBox is a box whose copy of the worktree holds files, committed so the test knows their tree, which it returns,
// and a .gitignore for the *.ran files the tests' checks leave, as a repository ignores its build output.
func newBox(t *testing.T, files map[string]string) (*box, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	b := &box{dir: t.TempDir(), root: t.TempDir()}
	git(t, b.dir, "init", "-q")
	files[".gitignore"] = "*.ran\n"
	for path, content := range files {
		write(t, filepath.Join(b.dir, path), content)
	}
	git(t, b.dir, "add", "-A")
	git(t, b.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "tree")
	return b, git(t, b.dir, "rev-parse", "HEAD^{tree}")
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

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Every check runs, each to its own log, and the outcome is success when all pass, else failure naming each failed
// check with its command, its exit and its log's last lines, colours removed.
func TestRunClassifiesEveryCheck(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	pass := config.Check{Name: "lint", Run: "true"}
	got, err := check.Run(t.Context(), b, b.root, tree, []config.Check{pass, {Name: "unit", Run: "echo ok > unit.ran"}})
	if err != nil || got.State != forge.Success || len(got.Failed) != 0 || !strings.Contains(got.Detail, "lint, unit") {
		t.Fatalf("two passing checks: %+v, %v", got, err)
	}
	logs := strings.Repeat("noise\n", 100) + "one\n\x1b[31mtwo\x1b[0m\n"
	fails := config.Check{Name: "test", Run: "printf '" + strings.ReplaceAll(logs, "\x1b", `\033`) + "'; exit 3"}
	after := config.Check{Name: "after", Run: "touch after.ran"}
	got, err = check.Run(t.Context(), b, b.root, tree, []config.Check{pass, fails, after})
	if err != nil || got.State != forge.Failure || len(got.Failed) != 1 {
		t.Fatalf("a failing check: %+v, %v", got, err)
	}
	f := got.Failed[0]
	if f.Name != "test" || f.Command != fails.Run || f.Exit != 3 || f.Log != ".land/2.log" {
		t.Errorf("the failed check is %+v", f)
	}
	if lines := strings.Split(f.Tail, "\n"); len(lines) != 40 || !strings.HasSuffix(f.Tail, "noise\none\ntwo") {
		t.Errorf("the tail is %d lines ending %q, want the log's last 40, colours removed", len(lines), f.Tail[max(0, len(f.Tail)-20):])
	}
	if !exists(filepath.Join(b.dir, "after.ran")) || !strings.Contains(got.Detail, "test") {
		t.Errorf("the check after the failure didn't run, or the detail %q names no failed check", got.Detail)
	}
	git(t, b.root, "init", "-q")
	if status := git(t, b.root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Errorf("git sees the result on the Mac: %q", status)
	}
}

// A parallel check runs at the same time as the others, which still run one at a time in the file's order; a failing
// parallel check fails alone, with its own log, while the others run to their end; results come back in the file's
// order. Each check waits for the other's mark, so checks run one at a time would fail rather than pass slowly.
func TestAParallelCheckRunsAlongsideTheOthers(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	waitFor := func(file string) string {
		return "for i in $(seq 200); do [ -e " + file + " ] && break; sleep 0.05; done; [ -e " + file + " ]"
	}
	lint := config.Check{Name: "lint", Run: "touch lint.ran && " + waitFor("side.ran") + " && touch lint-done.ran"}
	after := config.Check{Name: "after", Run: "[ -e lint-done.ran ] && touch after.ran"}
	side := config.Check{Name: "side", Run: "touch side.ran && " + waitFor("lint.ran"), Parallel: true}
	got, err := check.Run(t.Context(), b, b.root, tree, []config.Check{side, lint, after})
	if err != nil || got.State != forge.Success || got.Detail != "The check at the push passed: side, lint, after" {
		t.Fatalf("a parallel check beside two in order: %+v, %v; want success, in the file's order", got, err)
	}
	for _, f := range []string{"lint.ran", "side.ran", "lint-done.ran", "after.ran"} {
		if err := os.Remove(filepath.Join(b.dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	side.Run += " && exit 4"
	got, err = check.Run(t.Context(), b, b.root, tree, []config.Check{side, lint, after})
	if err != nil || got.State != forge.Failure || len(got.Failed) != 1 {
		t.Fatalf("a failing parallel check: %+v, %v; want a failure", got, err)
	}
	if f := got.Failed[0]; f.Name != "side" || f.Exit != 4 || f.Log != ".land/1.log" {
		t.Errorf("the failed check is %+v; want side, exit 4, .land/1.log", f)
	}
	if !exists(filepath.Join(b.dir, "after.ran")) {
		t.Error("the checks in order didn't run to their end beside a failing parallel check")
	}
}

// When the box half ends before its checks do, every check still running goes with it, the parallel ones and the one
// running in order alike, with what they started: none keeps writing on the box under the next command.
func TestEveryCheckGoesWithTheBoxHalf(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pids := []string{filepath.Join(b.dir, "par.ran"), filepath.Join(b.dir, "ser.ran")}
	go func() {
		for !exists(pids[0]) || !exists(pids[1]) {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	checks := []config.Check{
		{Name: "ser", Run: "echo $$ > ser.ran.tmp && mv ser.ran.tmp ser.ran && exec sleep 60"},
		{Name: "par", Run: "echo $$ > par.ran.tmp && mv par.ran.tmp par.ran && exec sleep 60", Parallel: true},
	}
	if _, err := check.Run(ctx, b, b.root, tree, checks); err == nil {
		t.Fatal("a box half ended midway gave no error")
	}
	for _, file := range pids {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if _, err := fmt.Sscan(string(data), &pid); err != nil {
			t.Fatal(err)
		}
		for start := time.Now(); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
			if time.Since(start) > 5*time.Second {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("%s's check, process %d, outlived the box half", filepath.Base(file), pid)
				break
			}
		}
	}
}

// A box whose tree isn't the commit's stops before any check runs, and the outcome is an error at "tree".
func TestATreeMismatchStopsBeforeAnyCheck(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	write(t, filepath.Join(b.dir, "stale.txt"), "left by a snapshot\n")
	got, err := check.Run(t.Context(), b, b.root, tree, []config.Check{{Name: "test", Run: "touch test.ran"}})
	if err != nil || got.State != forge.Error || got.Stage != "tree" {
		t.Fatalf("got %+v, %v; want an error at tree", got, err)
	}
	if exists(filepath.Join(b.dir, "test.ran")) {
		t.Error("a check ran on a box holding another tree")
	}
}

// A box command that stopped, before or around the box half, is no verdict, even when an earlier command's result,
// at the same tree and passing, is what came back; its detail, the mark's description, names the stage StageOf reads
// back. A result left on the Mac never stands for a command that brought none back.
func TestAStoppedBoxIsAnErrorAtItsStage(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	checks := []config.Check{{Name: "test", Run: "true"}}
	if got, err := check.Run(t.Context(), b, b.root, tree, checks); err != nil || got.State != forge.Success {
		t.Fatalf("got %+v, %v", got, err)
	}
	b.skip = true // the box half doesn't run, and the earlier result comes back
	for err, stage := range map[error]string{
		&boxes.StageError{Stage: "prepare", Err: errors.New("exit status 1")}: "prepare",
		errors.New("connection lost"):                                         "box",
	} {
		b.err = err
		got, runErr := check.Run(t.Context(), b, b.root, tree, checks)
		if runErr != nil || got.State != forge.Error || got.Stage != stage || !strings.Contains(got.Detail, err.Error()) ||
			check.StageOf(got.Detail) != stage || !errors.Is(got.Err, err) {
			t.Errorf("%v: got %+v, %v; want an error at %s", err, got, runErr, stage)
		}
	}
	if stage := check.StageOf("error: prepare failed"); stage != "" {
		t.Errorf("StageOf read %q from a description land didn't write", stage)
	}
	write(t, filepath.Join(b.root, check.ResultFile), `{"tree":"`+tree+`","stage":"check","checks":[{"name":"test","exit":0}]}`)
	b.dir, b.err = t.TempDir(), nil // a box command that ran nothing and brought nothing back
	if got, err := check.Run(t.Context(), b, b.root, tree, checks); err != nil || got.State != forge.Error || got.Stage != "result" {
		t.Errorf("with a stale result on the Mac: got %+v, %v; want an error at result", got, err)
	}
}

// The box half removes the last result before anything else, so a run that dies leaves none on the box.
func TestTheBoxRemovesTheLastResultFirst(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	write(t, filepath.Join(b.dir, check.ResultFile), `{"tree":"`+tree+`","stage":"check","checks":[{"name":"test","exit":0}]}`)
	got, err := check.Run(t.Context(), b, b.root, tree, []config.Check{{Name: "test", Run: "kill -9 $PPID"}})
	if err != nil || got.State != forge.Error || got.Stage != "box" {
		t.Errorf("a box half that died: got %+v, %v; want an error at box", got, err)
	}
	if exists(filepath.Join(b.dir, check.ResultFile)) {
		t.Error("the last result is still on the box")
	}
}

// A check's name and command come back exactly, whatever characters they hold.
func TestNamesAndCommandsSurviveTheResultFile(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	odd := config.Check{Name: "it's \"odd\"\t& 100%", Run: "# \\ \\\\n \"quoted\" & ünïcode €\x01\x1b\n: 'single' \\t\nexit 1"}
	got, err := check.Run(t.Context(), b, b.root, tree, []config.Check{odd})
	if err != nil || got.State != forge.Failure || len(got.Failed) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if f := got.Failed[0]; f.Name != odd.Name || f.Command != odd.Run {
		t.Errorf("came back as %q, %q; want %q, %q", f.Name, f.Command, odd.Name, odd.Run)
	}
}

// The box runs the repository's commands, so a log that came back as a symlink, or outside .land, isn't read: it could
// put a file of the Mac's into a failure comment.
func TestALogIsReadOnlyAsAFileInLand(t *testing.T) {
	b, tree := newBox(t, map[string]string{"a.txt": "a\n"})
	secret := filepath.Join(b.root, "secret.txt")
	write(t, secret, "the Mac's secret\n")
	result := `{"tree":"` + tree + `","stage":"check","checks":[` +
		`{"name":"link","exit":1,"log":".land/1.log"},{"name":"escape","exit":1,"log":"secret.txt"},{"name":"up","exit":1,"log":".land/../secret.txt"}]}`
	write(t, filepath.Join(b.dir, check.ResultFile), result)
	if err := os.Symlink(secret, filepath.Join(b.dir, ".land/1.log")); err != nil {
		t.Fatal(err)
	}
	b.skip = true
	got, err := check.Run(t.Context(), b, b.root, tree, make([]config.Check, 3))
	if err != nil || got.State != forge.Failure || len(got.Failed) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, f := range got.Failed {
		if f.Tail != "" {
			t.Errorf("%s's log %s was read: %q", f.Name, f.Log, f.Tail)
		}
	}
}
