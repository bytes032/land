// Package shim tests the land shim, a POSIX sh script, against a fake `go` on PATH.
package shim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGo stands in for `go install github.com/bytes032/land@vX`: it logs the version and installs a `land` that prints
// its version and arguments and exits with $LAND_EXIT. FAKE_GO_OFFLINE fails before installing; FAKE_GO_BROKEN leaves
// a half-written binary and fails, as an interrupted install would.
const fakeGo = `#!/bin/sh
[ "$1" = install ] || exit 3
[ -z "${FAKE_GO_OFFLINE:-}" ] || { echo "go: no network" >&2; exit 1; }
v=${2#github.com/bytes032/land@}
echo "$v" >> "$FAKE_GO_LOG"
if [ -n "${FAKE_GO_BROKEN:-}" ]; then printf '#!/bin/sh\nexit 0' > "$GOBIN/land"; chmod +x "$GOBIN/land"; exit 1; fi
printf '#!/bin/sh\necho "land %s $*"\nexit ${LAND_EXIT:-0}\n' "$v" > "$GOBIN/land"
chmod +x "$GOBIN/land"
`

type env struct {
	t     *testing.T
	shim  string
	log   string
	cache string
	path  string
}

func setup(t *testing.T) *env {
	shim, err := filepath.Abs("land")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "a", "GIT_AUTHOR_EMAIL": "a@a", "GIT_COMMITTER_NAME": "a", "GIT_COMMITTER_EMAIL": "a@a"} {
		t.Setenv(k, v)
	}
	return &env{t: t, shim: shim, log: filepath.Join(dir, "go.log"), cache: filepath.Join(dir, "cache"),
		path: bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// run runs the shim in dir with extra environment, returning its stdout, stderr and exit code.
func (e *env) run(dir string, extra []string, args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.shim, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + e.path, "XDG_CACHE_HOME=" + e.cache,
		"FAKE_GO_LOG=" + e.log}, extra...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		e.t.Fatal(err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

func (e *env) installs() []string {
	data, err := os.ReadFile(e.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		e.t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func pin(t *testing.T, root, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".land.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShimInstallsEachPinnedVersionOnce(t *testing.T) {
	e := setup(t)
	a := filepath.Join(t.TempDir(), "a")
	git(t, t.TempDir(), "init", "-q", "-b", "trunk", a)
	pin(t, a, "version: 0.1.0 # the pinned land version\ntrunk: trunk\n")
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "pin")
	b := filepath.Join(filepath.Dir(a), "b")
	git(t, a, "worktree", "add", "-q", "-b", "bump", b)
	pin(t, b, "trunk: trunk\nversion: 0.2.0\n")
	sub := filepath.Join(a, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if out, errOut, code := e.run(sub, nil, "gate", "--x", "two words"); code != 0 || out != "land v0.1.0 gate --x two words\n" {
		t.Fatalf("first run: %q %q exit %d", out, errOut, code)
	}
	// Offline from here on: an installed version never calls go again.
	offline := []string{"FAKE_GO_OFFLINE=1"}
	if out, errOut, code := e.run(a, offline, "green"); code != 0 || out != "land v0.1.0 green\n" {
		t.Fatalf("offline rerun: %q %q exit %d", out, errOut, code)
	}
	if out, _, code := e.run(b, nil, "green"); code != 0 || out != "land v0.2.0 green\n" {
		t.Fatalf("the other worktree: %q exit %d", out, code)
	}
	for dir, want := range map[string]string{a: "land v0.1.0 green\n", b: "land v0.2.0 green\n"} {
		if out, errOut, code := e.run(dir, offline, "green"); code != 0 || out != want {
			t.Fatalf("offline in %s: %q %q exit %d, want %q", dir, out, errOut, code, want)
		}
	}
	if got := strings.Join(e.installs(), " "); got != "v0.1.0 v0.2.0" {
		t.Fatalf("go installed %q, want each version once", got)
	}
	for _, code := range []int{0, 1, 7} {
		if _, _, got := e.run(a, append(offline, fmt.Sprint("LAND_EXIT=", code)), "gate"); got != code {
			t.Errorf("land exited %d, the shim %d", code, got)
		}
	}
}

func TestShimRetriesAnInterruptedInstall(t *testing.T) {
	e := setup(t)
	root := t.TempDir()
	git(t, root, "init", "-q")
	pin(t, root, "version: 1.0.0\n")
	if _, _, code := e.run(root, []string{"FAKE_GO_BROKEN=1"}); code == 0 {
		t.Fatal("a failed install ran land")
	}
	if out, errOut, code := e.run(root, nil, "x"); code != 0 || out != "land v1.0.0 x\n" {
		t.Fatalf("after a failed install: %q %q exit %d", out, errOut, code)
	}
	if got := len(e.installs()); got != 2 {
		t.Fatalf("go ran %d times, want 2", got)
	}
}

func TestShimRefusesAMissingOrQuotedVersion(t *testing.T) {
	e := setup(t)
	for _, yaml := range []string{
		"trunk: main\n",
		"version: \"0.1.0\"\n",
		"version: '0.1.0'\n",
		"version: v0.1.0\n",
		"version:\n",
		"stages:\n  version: 0.1.0\n",
		"version: 0.1.0\nversion: 0.2.0\n",
	} {
		root := t.TempDir()
		git(t, root, "init", "-q")
		pin(t, root, yaml)
		_, errOut, code := e.run(root, nil, "gate")
		if code == 0 || !strings.Contains(errOut, "bare, unquoted version") {
			t.Errorf("%q: exit %d, stderr %q", yaml, code, errOut)
		}
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	if _, errOut, code := e.run(root, nil); code == 0 || !strings.Contains(errOut, ".land.yaml") {
		t.Errorf("no .land.yaml: exit %d, stderr %q", code, errOut)
	}
	if got := e.installs(); len(got) != 0 {
		t.Fatalf("go ran for a bad pin: %v", got)
	}
}
