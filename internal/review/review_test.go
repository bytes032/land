package review

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/config"
)

// repo is a scratch clone of a bare origin named acme/app, on a trunk named main holding src.txt.
func repo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(v, "agent")
	}
	for _, v := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(v, "agent@example.com")
	}
	origin := filepath.Join(tmp, "acme", "app.git")
	run(t, tmp, "git", "init", "-q", "--bare", "-b", "main", origin)
	dir := filepath.Join(tmp, "work")
	run(t, tmp, "git", "clone", "-q", origin, dir)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "line " + string(rune('a'+i))
	}
	write(t, dir, "src.txt", strings.Join(lines, "\n")+"\n")
	commit(t, dir, "trunk")
	run(t, dir, "git", "push", "-q", "origin", "HEAD:main")
	return dir
}

func run(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	path = filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// edit replaces line n (1-based) of dir's path.
func edit(t *testing.T, dir, path string, n int, line string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	lines[n-1] = line
	write(t, dir, path, strings.Join(lines, "\n"))
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", msg)
}

func key(t *testing.T, dir, base string, exclude ...string) string {
	t.Helper()
	k, err := Key(context.Background(), dir, base, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCleanRebaseKeepsKeyAndFixChangesIt(t *testing.T) {
	dir := repo(t)
	base := run(t, dir, "git", "rev-parse", "HEAD")
	edit(t, dir, "src.txt", 15, "the change")
	commit(t, dir, "change")
	before := key(t, dir, base)

	// The trunk moves: lines inserted above the change and one edited in its context.
	run(t, dir, "git", "checkout", "-q", "-b", "trunk", base)
	edit(t, dir, "src.txt", 13, "trunk's edit")
	data, _ := os.ReadFile(filepath.Join(dir, "src.txt"))
	write(t, dir, "src.txt", "new 1\nnew 2\n"+string(data))
	commit(t, dir, "trunk moves")
	run(t, dir, "git", "checkout", "-q", "-")
	run(t, dir, "git", "rebase", "-q", "trunk")
	if got := key(t, dir, run(t, dir, "git", "rev-parse", "trunk")); got != before {
		t.Fatalf("a clean rebase changed the key: %s, was %s", got, before)
	}

	edit(t, dir, "src.txt", 4, "a fix")
	commit(t, dir, "fix")
	if got := key(t, dir, run(t, dir, "git", "rev-parse", "trunk")); got == before {
		t.Fatal("a fix kept the key")
	}
}

// No git setting changes the key: not the diff's prefixes, algorithm or context, nor the abbreviation of the object
// ids a binary file's diff is made of.
func TestKeyIgnoresGitConfig(t *testing.T) {
	dir := repo(t)
	write(t, dir, "x", "b\n}\na\nc\na\n{\n{\n{\n")
	write(t, dir, "logo.bin", "\x00one\x01")
	commit(t, dir, "x")
	base := run(t, dir, "git", "rev-parse", "HEAD")
	// myers and patience diff this pair differently.
	write(t, dir, "x", "{\nb\na\n{\na\n{\n{\n}\n")
	write(t, dir, "logo.bin", "\x00two\x01")
	edit(t, dir, "src.txt", 3, "the change")
	commit(t, dir, "change")
	before := key(t, dir, base)

	for _, kv := range [][2]string{
		{"diff.noprefix", "true"}, {"diff.algorithm", "patience"}, {"diff.mnemonicPrefix", "true"},
		{"diff.context", "10"}, {"color.ui", "always"}, {"core.abbrev", "20"},
	} {
		run(t, dir, "git", "config", kv[0], kv[1])
	}
	if got := key(t, dir, base); got != before {
		t.Fatalf("git config changed the key: %s, was %s", got, before)
	}
}

func TestKeyLeavesOutExclusionsUnlessTheyAreAll(t *testing.T) {
	dir := repo(t)
	base := run(t, dir, "git", "rev-parse", "HEAD")
	if _, err := Key(context.Background(), dir, base, nil); err == nil {
		t.Fatal("an empty change got a key")
	}
	commit(t, dir, "empty")
	if _, err := Key(context.Background(), dir, base, nil); err == nil {
		t.Fatal("an empty commit got a key")
	}

	write(t, dir, "gen/out", "one\n")
	commit(t, dir, "derived only")
	derivedOnly := key(t, dir, base, "gen/")
	if derivedOnly != key(t, dir, base) {
		t.Fatal("a derived-only change didn't key its whole diff")
	}
	write(t, dir, "gen/out", "two\n")
	commit(t, dir, "derived again")
	if key(t, dir, base, "gen/") == derivedOnly {
		t.Fatal("a different derived-only change kept the key")
	}

	edit(t, dir, "src.txt", 2, "the change")
	commit(t, dir, "source")
	withDerived := key(t, dir, base, "gen/")
	write(t, dir, "gen/out", "three\n")
	commit(t, dir, "regenerated")
	if key(t, dir, base, "gen/") != withDerived {
		t.Fatal("an excluded file changed the key")
	}
	if key(t, dir, base) == withDerived {
		t.Fatal("the exclusion left nothing out")
	}
}

// review runs `land review ARGS…` in dir with the review stage command.
func review(dir, command string, args ...string) error {
	cfg := &config.Config{Trunk: "main", Stages: config.Stages{Review: command}}
	return runReview(context.Background(), dir, cfg, []string{"gen/"}, args, io.Discard, io.Discard)
}

func finished(t *testing.T, dir string) error {
	t.Helper()
	return check(context.Background(), dir, &config.Config{Trunk: "main"}, []string{"gen/"}, io.Discard)
}

// records are the finished markers recorded for any key.
func records(t *testing.T) []string {
	t.Helper()
	state, err := config.StateDir("acme.app")
	if err != nil {
		t.Fatal(err)
	}
	found, _ := filepath.Glob(filepath.Join(state, "reviews", "*", "finished"))
	return found
}

func TestReviewRecordsAFinishedReview(t *testing.T) {
	dir := repo(t)
	base := run(t, dir, "git", "rev-parse", "HEAD")
	edit(t, dir, "src.txt", 5, "the change")
	commit(t, dir, "change")
	if err := finished(t, dir); err == nil {
		t.Fatal("check passed with no review")
	}
	if err := Finished(context.Background(), dir, base, nil); err == nil {
		t.Fatal("Finished passed with no review")
	}

	if err := review(dir, "exit 1", "--ticket"); err == nil {
		t.Fatal("a failed review stage succeeded")
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("a failed review recorded %v", got)
	}

	stage := `test -z "$(ls -A "$2")" && printf '%s\n' "$@" > "$2/args"`
	if err := review(dir, stage, "--ticket", "issue 7"); err != nil {
		t.Fatal(err)
	}
	if err := finished(t, dir); err != nil {
		t.Fatal(err)
	}
	if err := Finished(context.Background(), dir, base, []string{"gen/"}); err != nil {
		t.Fatal(err)
	}
	got := records(t)
	if len(got) != 1 {
		t.Fatalf("records: %v", got)
	}
	args, err := os.ReadFile(filepath.Join(filepath.Dir(got[0]), "report", "args"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 4 || lines[0] != base || !filepath.IsAbs(lines[1]) || lines[2] != "--ticket" || lines[3] != "issue 7" {
		t.Fatalf("the stage got %q", lines)
	}

	// A finished review isn't rerun.
	if err := review(dir, "exit 1"); err != nil {
		t.Fatal(err)
	}

	edit(t, dir, "src.txt", 6, "a fix")
	commit(t, dir, "fix")
	if err := finished(t, dir); err == nil {
		t.Fatal("check passed after a fix")
	}
	if err := Finished(context.Background(), dir, base, []string{"gen/"}); err == nil {
		t.Fatal("Finished passed after a fix")
	}
}

func TestReviewRecordsNothingWhenHeadMoves(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "src.txt", 5, "the change")
	commit(t, dir, "change")
	err := review(dir, `echo more >> src.txt && git commit -qam moved`)
	if err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("got %v, want HEAD moved", err)
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("a review whose HEAD moved recorded %v", got)
	}
}

func TestReviewRefusesADirtyWorktree(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "src.txt", 5, "the change")
	commit(t, dir, "change")
	edit(t, dir, "src.txt", 6, "uncommitted")
	if err := review(dir, "true"); err == nil || !strings.Contains(err.Error(), "commit your changes") {
		t.Fatalf("got %v, want a dirty worktree refused", err)
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("a dirty worktree recorded %v", got)
	}
}

func TestReviewRefusesAnEmptyReviewStage(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "src.txt", 5, "the change")
	commit(t, dir, "change")
	if err := review(dir, ""); err == nil {
		t.Fatal("an empty review stage recorded a review")
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("an empty review stage recorded %v", got)
	}
}
