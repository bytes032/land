package verdict

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytes032/land/internal/forge"
)

// repo is a scratch repository whose .land.yaml has the given gate policy; it returns its root.
func repo(t *testing.T, unverified string) string {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "a", "GIT_AUTHOR_EMAIL": "a@example.com", "GIT_COMMITTER_NAME": "a", "GIT_COMMITTER_EMAIL": "a@example.com",
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "--initial-branch=main")
	yaml := "version: 0.1.0\ntrunk: main\ngate: {timeout: 30m, unverified: " + unverified + "}\n" +
		"box: {type: cpx42}\nstages: {check: {test: {run: 'true'}}}\n"
	if err := os.WriteFile(filepath.Join(dir, ".land.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".land.yaml")
	run(t, dir, "commit", "--quiet", "-m", "adopt land")
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit makes a commit with message and returns its sha.
func commit(t *testing.T, dir, message string) string {
	t.Helper()
	run(t, dir, "commit", "--quiet", "--allow-empty", "-m", message)
	return run(t, dir, "rev-parse", "HEAD")
}

const (
	checked          = "feat: a change\n\nAgent-Session: s1\nAgent: Main"
	uncheckedMessage = "feat: a change\n\nAgent-Session: s1\nAgent: Main\nUnchecked: #7"
)

// Every mark and trailer gives the right verdict, and the newest mark counts whoever set it.
func TestOf(t *testing.T) {
	dir := repo(t, "fail")
	plain, unchecked := commit(t, dir, checked), commit(t, dir, uncheckedMessage)
	cases := []struct {
		marks    []forge.State // oldest first
		sha      string
		verified bool
		mark     forge.State // "" for none
	}{
		{sha: plain},
		{sha: unchecked},
		{marks: []forge.State{forge.Success}, sha: plain, verified: true, mark: forge.Success},
		{marks: []forge.State{forge.Success}, sha: unchecked, mark: forge.Success},
		{marks: []forge.State{forge.Failure}, sha: plain, mark: forge.Failure},
		{marks: []forge.State{forge.Failure}, sha: unchecked, mark: forge.Failure},
		{marks: []forge.State{forge.Error}, sha: plain, mark: forge.Error},
		{marks: []forge.State{forge.Pending}, sha: plain, mark: forge.Pending},
		{marks: []forge.State{forge.Success, forge.Failure}, sha: plain, mark: forge.Failure},
		{marks: []forge.State{forge.Failure, forge.Success}, sha: plain, verified: true, mark: forge.Success},
		{marks: []forge.State{forge.Success, forge.Pending}, sha: plain, mark: forge.Pending},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%v on %s", tc.marks, map[string]string{plain: "checked", unchecked: "unchecked"}[tc.sha])
		t.Run(name, func(t *testing.T) {
			f := &forge.Memory{}
			for i, s := range tc.marks {
				// a different account sets each mark: any account's mark counts
				if err := f.SetMark(context.Background(), tc.sha, forge.Mark{State: s, Creator: fmt.Sprint("account", i)}); err != nil {
					t.Fatal(err)
				}
			}
			v, err := Of(context.Background(), f, dir, tc.sha)
			if err != nil {
				t.Fatal(err)
			}
			got := forge.State("")
			if v.Mark != nil {
				got = v.Mark.State
			}
			if got != tc.mark || v.Verified != tc.verified {
				t.Errorf("mark %q verified %t; want %q %t", got, v.Verified, tc.mark, tc.verified)
			}
		})
	}
}

// fakeTime replaces the clock and sleep: each sleep advances the clock and then calls during(sleeps so far).
func fakeTime(t *testing.T, during func(int)) *int {
	t.Helper()
	clock, sleeps := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 0
	oldNow, oldSleep := now, sleep
	now = func() time.Time { return clock }
	sleep = func(_ context.Context, d time.Duration) error {
		if d != poll {
			t.Errorf("slept %s, want %s", d, poll)
		}
		clock = clock.Add(d)
		sleeps++
		if during != nil {
			during(sleeps)
		}
		return nil
	}
	t.Cleanup(func() { now, sleep = oldNow, oldSleep })
	return &sleeps
}

// useForge makes GateMain and GreenMain use f.
func useForge(t *testing.T, f forge.Forge) {
	old := newForge
	newForge = func(context.Context, string) (forge.Forge, error) { return f, nil }
	t.Cleanup(func() { newForge = old })
}

// gateRun runs `land gate` in dir for sha and returns its error and what it wrote to GITHUB_OUTPUT.
func gateRun(t *testing.T, dir, sha string) (error, string) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "output")
	t.Setenv("GITHUB_SHA", sha)
	t.Setenv("GITHUB_OUTPUT", output)
	t.Chdir(dir)
	err := GateMain(context.Background(), nil)
	data, readErr := os.ReadFile(output)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	return err, string(data)
}

// Every combination of mark and Unchecked trailer gives the right verified output and gate outcome under both
// unverified policies.
func TestGate(t *testing.T) {
	type outcome struct {
		err    string // the run's failure; "" passes
		output string // GITHUB_OUTPUT
	}
	pass := func(verified bool) outcome { return outcome{output: fmt.Sprintf("verified=%t\n", verified)} }
	fail := func(err string) outcome { return outcome{err: err} }
	cases := []struct {
		name       string
		mark       forge.State // "" for none
		unchecked  bool
		fail, skip outcome
		sleeps     int
	}{
		{name: "success passes verified", mark: forge.Success, fail: pass(true), skip: pass(true)},
		{name: "unchecked success passes unverified", mark: forge.Success, unchecked: true, fail: pass(false), skip: pass(false)},
		{name: "failure fails with its description", mark: forge.Failure, fail: fail("marked failure: the description"), skip: fail("marked failure: the description")},
		{name: "unchecked failure fails", mark: forge.Failure, unchecked: true, fail: fail("marked failure: the description"), skip: fail("marked failure: the description")},
		{name: "error follows the policy", mark: forge.Error, fail: fail("marked error: the description"), skip: pass(false)},
		{name: "unchecked error follows the policy", mark: forge.Error, unchecked: true, fail: fail("marked error: the description"), skip: pass(false)},
		{name: "pending until the timeout follows the policy", mark: forge.Pending, fail: fail("no final mark after 30m0s"), skip: pass(false), sleeps: 120},
		{name: "no mark until the timeout follows the policy", fail: fail("no final mark after 30m0s"), skip: pass(false), sleeps: 120},
		{name: "unchecked with no mark follows the policy", unchecked: true, fail: fail("no final mark after 30m0s"), skip: pass(false), sleeps: 120},
	}
	for _, tc := range cases {
		for policy, want := range map[string]outcome{"fail": tc.fail, "skip": tc.skip} {
			t.Run(tc.name+" under "+policy, func(t *testing.T) {
				dir := repo(t, policy)
				sha := commit(t, dir, map[bool]string{false: checked, true: uncheckedMessage}[tc.unchecked])
				f := &forge.Memory{}
				if tc.mark != "" {
					if err := f.SetMark(context.Background(), sha, forge.Mark{State: tc.mark, Description: "the description"}); err != nil {
						t.Fatal(err)
					}
				}
				useForge(t, f)
				sleeps := fakeTime(t, nil)
				err, output := gateRun(t, dir, sha)
				got := ""
				if err != nil {
					got = err.Error()
				}
				if want.err == "" && err != nil || want.err != "" && !strings.Contains(got, want.err) || output != want.output || *sleeps != tc.sleeps {
					t.Errorf("error %q, output %q after %d sleeps; want error %q, output %q after %d", got, output, *sleeps, want.err, want.output, tc.sleeps)
				}
			})
		}
	}
}

// flaky fails its first Marks read.
type flaky struct {
	*forge.Memory
	failed bool
}

func (f *flaky) Marks(ctx context.Context, sha string) ([]forge.Mark, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("statuses unavailable")
	}
	return f.Memory.Marks(ctx, sha)
}

// The gate waits out a pending mark and a failed read for a final mark that arrives before the timeout, and a final
// mark that arrives after it comes too late.
func TestGateWaits(t *testing.T) {
	cases := []struct {
		name    string
		arrives int // the sleep after which the success mark is set
		err     string
		output  string
	}{
		{name: "a final mark within the timeout passes", arrives: 3, output: "verified=true\n"},
		{name: "a final mark on the last read passes", arrives: 120, output: "verified=true\n"},
		{name: "a final mark after the timeout is too late", arrives: 121, err: "no final mark after 30m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := repo(t, "fail")
			sha := commit(t, dir, checked)
			f := &flaky{Memory: &forge.Memory{}}
			ctx := context.Background()
			if err := f.SetMark(ctx, sha, forge.Mark{State: forge.Pending}); err != nil {
				t.Fatal(err)
			}
			useForge(t, f)
			fakeTime(t, func(n int) {
				if n == tc.arrives {
					if err := f.SetMark(ctx, sha, forge.Mark{State: forge.Success}); err != nil {
						t.Error(err)
					}
				}
			})
			err, output := gateRun(t, dir, sha)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if tc.err == "" && err != nil || tc.err != "" && !strings.Contains(got, tc.err) || output != tc.output {
				t.Errorf("error %q, output %q; want error %q, output %q", got, output, tc.err, tc.output)
			}
		})
	}
}

// GreenCommit names the newest verified first-parent commit among ref's 100 newest, never an unchecked landing marked
// success, and the tip when none is.
func TestGreenCommit(t *testing.T) {
	ctx := context.Background()
	dir := repo(t, "fail")
	var trunk []string // oldest first; trunk[85] is an unchecked landing
	for i := range 101 {
		message := fmt.Sprint("trunk ", i)
		if i == 85 {
			message += "\n\nUnchecked: #5"
		}
		trunk = append(trunk, commit(t, dir, message))
	}
	run(t, dir, "switch", "--quiet", "-c", "side", trunk[99])
	side := commit(t, dir, "side")
	run(t, dir, "switch", "--quiet", "main")
	run(t, dir, "merge", "--quiet", "--no-ff", "-m", "merge side", "side")
	tip := run(t, dir, "rev-parse", "HEAD")
	// first-parent from the tip: tip, trunk[100] … trunk[2] are the 100 newest; trunk[1] is the 101st.
	set := func(f *forge.Memory, sha string, s forge.State) {
		if err := f.SetMark(ctx, sha, forge.Mark{State: s}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		marks func(f *forge.Memory)
		want  string
		ok    bool
	}{
		{name: "the newest green", marks: func(f *forge.Memory) {
			set(f, trunk[50], forge.Success)
			set(f, trunk[80], forge.Success)
			set(f, trunk[90], forge.Failure)
		}, want: trunk[80], ok: true},
		{name: "the tip itself", marks: func(f *forge.Memory) { set(f, tip, forge.Success) }, want: tip, ok: true},
		{name: "a commit later marked failure isn't green", marks: func(f *forge.Memory) {
			set(f, trunk[60], forge.Success)
			set(f, trunk[80], forge.Success)
			set(f, trunk[80], forge.Failure)
		}, want: trunk[60], ok: true},
		{name: "the 100th newest counts", marks: func(f *forge.Memory) { set(f, trunk[2], forge.Success) }, want: trunk[2], ok: true},
		{name: "none green among the 100 newest names the tip", marks: func(f *forge.Memory) {
			set(f, trunk[1], forge.Success)
			set(f, side, forge.Success)
			set(f, trunk[70], forge.Error)
		}, want: tip},
		{name: "an unchecked landing marked success isn't green", marks: func(f *forge.Memory) {
			set(f, trunk[50], forge.Success)
			set(f, trunk[85], forge.Success)
		}, want: trunk[50], ok: true},
		{name: "only unchecked landings marked success names the tip", marks: func(f *forge.Memory) { set(f, trunk[85], forge.Success) },
			want: tip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &forge.Memory{}
			tc.marks(f)
			got, ok, err := GreenCommit(ctx, f, dir, "main")
			if err != nil || got != tc.want || ok != tc.ok {
				t.Errorf("GreenCommit = %s, %t, %v; want %s, %t", got, ok, err, tc.want, tc.ok)
			}
		})
	}
}

// `land green` fetches origin's trunk and prints its green commit, or its tip when none is green.
func TestGreenMain(t *testing.T) {
	ctx := context.Background()
	origin := repo(t, "fail")
	clone := filepath.Join(t.TempDir(), "clone")
	run(t, origin, "clone", "--quiet", origin, clone)
	green := commit(t, origin, "green")
	tip := commit(t, origin, "on top") // the clone has neither until land green fetches them
	f := &forge.Memory{}
	useForge(t, f)
	t.Chdir(clone)

	if got := stdout(t, func() error { return GreenMain(ctx, nil) }); got != tip+"\n" {
		t.Errorf("with no green commit, land green printed %q; want the tip %s", got, tip)
	}
	if err := f.SetMark(ctx, green, forge.Mark{State: forge.Success}); err != nil {
		t.Fatal(err)
	}
	if got := stdout(t, func() error { return GreenMain(ctx, nil) }); got != green+"\n" {
		t.Errorf("land green printed %q; want the green commit %s", got, green)
	}
}

// stdout runs fn and returns what it printed to standard output.
func stdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil || runErr != nil {
		t.Fatal(err, runErr)
	}
	return string(data)
}
