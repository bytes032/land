package guard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary doubles as the hook: git runs it with hookEnv set, and it is `land pre-push`.
const hookEnv = "LAND_GUARD_TEST_HOOK"

func TestMain(m *testing.M) {
	if os.Getenv(hookEnv) == "1" {
		if err := PrePushMain(context.Background(), os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "land:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

// TestPrePushGuardsOnlyTheTrunk runs the hook as git does, in a clone whose .land.yaml names "release" as the trunk,
// so a guard that hard-codes a branch name fails.
func TestPrePushGuardsOnlyTheTrunk(t *testing.T) {
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "a", "GIT_AUTHOR_EMAIL": "a@a", "GIT_COMMITTER_NAME": "a", "GIT_COMMITTER_EMAIL": "a@a"} {
		t.Setenv(k, v)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	remote, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	mustGit(t, dir, "init", "-q", "--bare", "-b", "release", remote)
	mustGit(t, dir, "init", "-q", "-b", "release", work)
	mustGit(t, work, "remote", "add", "origin", remote)
	hook := fmt.Sprintf("#!/bin/sh\nexec env %s=1 '%s' \"$@\"\n", hookEnv, self)
	if err := os.WriteFile(filepath.Join(work, ".git", "hooks", "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "version: 0.1.0\ntrunk: release\ngate: {timeout: 30m, unverified: fail}\nbox: {type: cpx42}\n" +
		"stages: {check: {test: {run: 'true'}}}\n"
	if err := os.WriteFile(filepath.Join(work, ".land.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) string {
		mustGit(t, work, "add", "-A")
		mustGit(t, work, "commit", "-q", "--allow-empty", "-m", msg)
		return mustGit(t, work, "rev-parse", "HEAD")
	}
	remoteAt := func(branch string) string {
		out, _ := git(t, remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
		return strings.TrimSpace(out)
	}
	push := func(want bool, args ...string) {
		t.Helper()
		out, err := git(t, work, append([]string{"push", "-q", "origin"}, args...)...)
		if (err == nil) != want {
			t.Fatalf("git push %v: allowed %v, want %v\n%s", args, err == nil, want, out)
		}
		if !want && !strings.Contains(out, "refusing to") {
			t.Fatalf("git push %v was refused without the guard's reason:\n%s", args, out)
		}
	}

	first := commit("first")
	push(true, "release") // creating the trunk
	second := commit("second")
	push(true, "release") // a fast-forward
	push(true, first+":refs/heads/topic", second+":refs/heads/main")

	push(false, "--force", first+":release") // backward
	mustGit(t, work, "checkout", "-q", "-b", "side", first)
	side := commit("side")
	push(false, "--force", side+":release") // sideways
	push(false, "--delete", "release")
	push(false, "--force", side+":refs/heads/topic", first+":release") // one refused ref refuses the whole push
	if got := remoteAt("release"); got != second {
		t.Fatalf("the trunk is at %s after refused pushes, want %s", got, second)
	}
	if got := remoteAt("topic"); got != first {
		t.Fatalf("topic is at %s after a refused push, want %s", got, first)
	}

	push(true, "--force", side+":refs/heads/topic", first+":refs/heads/main") // other branches move freely
	push(true, "--delete", "topic")
	if remoteAt("topic") != "" || remoteAt("main") != first {
		t.Fatal("other branches' force-pushes and deletes didn't land")
	}
}
