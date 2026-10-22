package classify_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/classify"
	"github.com/bytes032/land/internal/config"
)

const landYAML = `
stages:
  check:
    lint: {run: lint}
    test: {run: test, ignore: [.md, .txt]}
    proofs: {run: prove, paths: [core/, toolchain.toml]}
    api: {run: api, paths: [internal/api]}
  regenerate:
    - {kind: derived, path: vectors/, run: gen-vectors}
    - {kind: lockfile, path: go.sum, run: go mod tidy}
    - {kind: derived, path: api/schema.json, run: gen-schema}
    - {kind: lockfile, path: web/bun.lock, run: bun install}
    - {kind: derived, path: gen, run: gen-code}
`

func TestClassify(t *testing.T) {
	c, err := config.Parse([]byte(landYAML))
	if err != nil {
		t.Fatal(err)
	}
	stages := c.Stages

	type want struct {
		checks   []string
		regen    []string
		policies []classify.Policy // one per conflicted path
	}
	for _, tc := range []struct {
		name       string
		changed    []string
		conflicted []string
		want       want
	}{
		{name: "the empty change runs no path-filtered check", want: want{checks: []string{"lint", "test"}}},
		{name: "docs only skip the check ignoring them", changed: []string{"README.md", "docs/notes.txt"},
			want: want{checks: []string{"lint"}}},
		{name: "one path outside ignore runs it", changed: []string{"README.md", "main.go"},
			want: want{checks: []string{"lint", "test"}}},
		{name: "a suffix is not a prefix", changed: []string{"md/main.go"}, want: want{checks: []string{"lint", "test"}}},
		{name: "a directory prefix runs its check", changed: []string{"core/a/b.go"},
			want: want{checks: []string{"lint", "test", "proofs"}}},
		{name: "a file prefix runs its check", changed: []string{"toolchain.toml"},
			want: want{checks: []string{"lint", "test", "proofs"}}},
		{name: "a check path is a plain prefix", changed: []string{"toolchain.toml.bak"},
			want: want{checks: []string{"lint", "test", "proofs"}}},
		{name: "a check path without its slash covers its directory", changed: []string{"internal/api/handler.go"},
			want: want{checks: []string{"lint", "test", "api"}}},
		{name: "a check path with its slash needs the slash", changed: []string{"core"},
			want: want{checks: []string{"lint", "test"}}},
		{name: "paths and ignore apply together", changed: []string{"core/README.md"},
			want: want{checks: []string{"lint", "proofs"}}},
		{name: "lockfiles first, then derived, each in configured order",
			changed: []string{"api/schema.json", "web/bun.lock", "vectors/a.json", "go.sum"},
			want:    want{checks: []string{"lint", "test"}, regen: []string{"go.sum", "web/bun.lock", "vectors/", "api/schema.json"}}},
		{name: "a conflicted derived file is taken from the trunk and regenerated",
			changed: []string{"main.go"}, conflicted: []string{"vectors/x/y.json"},
			want: want{checks: []string{"lint", "test"}, regen: []string{"vectors/"}, policies: []classify.Policy{classify.TakeTrunk}}},
		{name: "a conflicted lockfile stops land and isn't regenerated for the conflict",
			changed: []string{"main.go"}, conflicted: []string{"go.sum"},
			want: want{checks: []string{"lint", "test"}, policies: []classify.Policy{classify.Stop}}},
		{name: "a derived directory named without its slash is regenerated, as git's pathspec names it",
			changed: []string{"gen/out.go"}, conflicted: []string{"gen/x/y.go"},
			want: want{checks: []string{"lint", "test"}, regen: []string{"gen"}, policies: []classify.Policy{classify.TakeTrunk}}},
		{name: "a conflicted path with no entry stops land",
			changed: []string{"main.go"}, conflicted: []string{"main.go", "api/schema.json.orig", "vectors", "gen.go"},
			want: want{checks: []string{"lint", "test"}, policies: []classify.Policy{classify.Stop, classify.Stop, classify.Stop, classify.Stop}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks []string
			for _, c := range classify.Checks(stages, tc.changed) {
				checks = append(checks, c.Name)
			}
			if !slices.Equal(checks, tc.want.checks) {
				t.Errorf("Checks = %q, want %q", checks, tc.want.checks)
			}
			var regen []string
			for _, r := range classify.Regenerations(stages, tc.changed, tc.conflicted) {
				regen = append(regen, r.Path)
			}
			if !slices.Equal(regen, tc.want.regen) {
				t.Errorf("Regenerations = %q, want %q", regen, tc.want.regen)
			}
			var policies []classify.Policy
			for _, path := range tc.conflicted {
				policies = append(policies, classify.OnConflict(stages, path))
			}
			if !slices.Equal(policies, tc.want.policies) {
				t.Errorf("OnConflict = %v, want %v", policies, tc.want.policies)
			}
		})
	}

	if got, want := classify.ReviewExclusions(stages), []string{"vectors/", "api/schema.json", "gen"}; !slices.Equal(got, want) {
		t.Errorf("ReviewExclusions = %q, want %q", got, want)
	}
}

// A derived entry names the files git's literal pathspec of its path names, so the review key, which leaves the
// entries out through such pathspecs, leaves out exactly the files land regenerates and takes the trunk's side of.
func TestDerivedEntriesNameWhatGitsPathspecsName(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	files := []string{"gen/out.go", "gen/x/y.go", "gen.go", "genx/a", "api.json", "api.json.orig", "vectors", "sub/gen/z"}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(path+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "files")
	for _, entry := range []string{"gen", "gen/", "api.json", "vectors/", "sub/gen", "sub/gen/"} {
		named := strings.FieldsFunc(git("diff-tree", "-r", "-z", "--name-only", "--no-commit-id", "--root", "HEAD", "--", ":(top,literal)"+entry),
			func(r rune) bool { return r == 0 })
		stages := config.Stages{Regenerate: []config.Regenerate{{Kind: config.Derived, Path: entry, Run: "gen"}}}
		var derived []string
		for _, path := range files {
			if len(classify.Regenerations(stages, []string{path}, nil)) > 0 {
				derived = append(derived, path)
			}
		}
		slices.Sort(named)
		slices.Sort(derived)
		if !slices.Equal(derived, named) {
			t.Errorf("the derived entry %q names %q; git's pathspec names %q", entry, derived, named)
		}
	}
}
