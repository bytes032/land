package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoNameIsOwnerDotName(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/acme/api.git":   "acme.api",
		"https://github.com/acme/api":       "acme.api",
		"git@github.com:acme-labs/api.git":  "acme-labs.api",
		"ssh://git@github.com/acme/api.git": "acme.api",
	} {
		if got, err := repoName(remote); err != nil || got != want {
			t.Errorf("repoName(%q) = %q, %v; want %q", remote, got, err, want)
		}
	}
	for _, remote := range []string{"https://github.com/api", "api"} {
		if got, err := repoName(remote); err == nil {
			t.Errorf("repoName(%q) = %q, want an error", remote, got)
		}
	}
}

func TestChecksKeepTheFilesOrder(t *testing.T) {
	c, err := Parse([]byte("stages:\n  check:\n    zeta: {run: z}\n    alpha: {run: a, ignore: [.md]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Stages.Check) != 2 || c.Stages.Check[0].Name != "zeta" || c.Stages.Check[1].Ignore[0] != ".md" {
		t.Fatalf("checks = %+v", c.Stages.Check)
	}
}

const valid = `version: 0.1.0
trunk: main
message: {require: ['^Closes #\d+$']}
gate: {timeout: 30m, unverified: fail}
box: {type: cpx42}
stages:
  check: {lint: {run: make lint}, test: {run: make test, ignore: [.md]}}
  regenerate: [{kind: lockfile, path: go.sum, run: go mod tidy}, {kind: derived, path: gen/, run: make gen}]
`

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, File), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(root)
}

func TestLoadReadsAValidFile(t *testing.T) {
	c, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "0.1.0" || c.Trunk != "main" || c.Gate.Timeout.Minutes() != 30 || c.Gate.Unverified != "fail" ||
		len(c.Stages.Check) != 2 || len(c.Stages.Regenerate) != 2 {
		t.Fatalf("config = %+v", c)
	}
	for _, other := range []string{"version: 1.2.3-rc.1+build.5", "unverified: skip", "regenerate: []"} {
		yaml := valid
		switch {
		case strings.HasPrefix(other, "version"):
			yaml = strings.Replace(yaml, "version: 0.1.0", other, 1)
		case strings.HasPrefix(other, "unverified"):
			yaml = strings.Replace(yaml, "unverified: fail", other, 1)
		default:
			yaml = valid[:strings.Index(valid, "  regenerate:")] + "  " + other + "\n"
		}
		if _, err := load(t, yaml); err != nil {
			t.Errorf("with %q: %v", other, err)
		}
	}
}

// Each case breaks one key of the valid file; the error must name that key.
func TestLoadNamesTheKeyAtFault(t *testing.T) {
	for _, tc := range []struct{ old, new, key string }{
		{"version: 0.1.0\n", "", "version is required"},
		{"version: 0.1.0", "version: v0.1.0", "version"},
		{"version: 0.1.0", "version: 0.1", "version"},
		{"version: 0.1.0", "version: latest", "version"},
		{"trunk: main\n", "", "trunk"},
		{"timeout: 30m, ", "", "gate.timeout"},
		{"timeout: 30m", "timeout: -1m", "gate.timeout"},
		{"timeout: 30m", "timeout: soon", "line 4"},
		{"unverified: fail", "unverified: pass", "gate.unverified"},
		{"gate: {timeout: 30m, unverified: fail}", "gate: {timeout: 30m}", "gate.unverified"},
		{"box: {type: cpx42}\n", "", "box.type"},
		{"box: {type: cpx42}", "box: {env: {A: b}}", "box.type"},
		{"  check: {lint: {run: make lint}, test: {run: make test, ignore: [.md]}}\n", "", "stages.check"},
		{"  check: {lint: {run: make lint}, test: {run: make test, ignore: [.md]}}", "  check: {}", "stages.check"},
		{"test: {run: make test, ignore: [.md]}", "lint: {run: make test}", "stages.check.lint is listed twice"},
		{"test: {run: make test, ignore: [.md]}", "test: {ignore: [.md]}", "stages.check.test.run"},
		{"kind: derived", "kind: generated", "stages.regenerate[1].kind"},
		{"path: go.sum, ", "", "stages.regenerate[0].path"},
		{", run: make gen", "", "stages.regenerate[1].run"},
		{`'^Closes #\d+$'`, `'^Closes (#\d+$'`, "message.require[0]"},
	} {
		yaml := strings.Replace(valid, tc.old, tc.new, 1)
		if yaml == valid {
			t.Fatalf("case %q doesn't change the file", tc.old)
		}
		_, err := load(t, yaml)
		if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.HasPrefix(err.Error(), File+": ") {
			t.Errorf("replacing %q with %q: error %v, want one naming %q", tc.old, tc.new, err, tc.key)
		}
	}
}

// A key land doesn't know, at the top, inside a section or inside a check, is refused and named, so a misspelled or
// removed key is never silently ignored.
func TestUnknownKeysAreRefused(t *testing.T) {
	for name, yaml := range map[string]string{
		"status": "version: 0.1.0\nstatus: box/verify\n",
		"tools":  "version: 0.1.0\nstages:\n  tools: []\n",
		"timout": "version: 0.1.0\ngate: {timout: 30m}\n",
		"comand": "version: 0.1.0\nstages:\n  check:\n    lint: {comand: make lint}\n",
	} {
		if _, err := Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a .land.yaml with %q: error %v, want one naming it", name, err)
		}
	}
	if c, err := Parse([]byte("version: 0.1.0\nstages:\n  check:\n    lint: {run: make lint, paths: [src/]}\n")); err != nil || c.Stages.Check[0].Paths[0] != "src/" {
		t.Fatalf("a valid file: %+v, %v", c, err)
	}
	if _, err := Parse(nil); err != nil {
		t.Fatalf("an empty file: %v", err)
	}
}
