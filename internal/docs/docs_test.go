package docs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findings is the findings rule as the spec words it, from its first finding to its last verdict.
const findings = "each finding at or above the threshold, verify it against the code and task; fix it if likely or rare " +
	"but dangerous (data loss, security, money), with a regression test; refute it with evidence if wrong; decline it " +
	"if real but unlikely, recoverable and costlier to fix than the risk, with a one-line reason (a finding that only " +
	"adds restrictions with no concrete harm is declined the same way); record refutations and declines in the commit " +
	"message's `Review:` paragraph. A fix needs a new review; a refutation or decline doesn't; never rerun a review " +
	"for a nicer verdict."

// LANDING.md has every section the spec names, the findings rule in the spec's words, and each land command an agent
// runs.
func TestLandingHoldsItsSectionsTheFindingsRuleAndTheCommands(t *testing.T) {
	text := string(landing)
	for _, section := range []string{
		"## Branch", "## Build and test on the box", "## Ready to land", "## Land", "## CI", "## Stay with the commit",
		"## A failure your change didn't cause", "## A red trunk", "## Conflicts", "## Parallel work",
	} {
		if !strings.Contains(text, "\n"+section+"\n") {
			t.Errorf("LANDING.md has no %q section", section)
		}
	}
	if flat := strings.Join(strings.Fields(text), " "); !strings.Contains(flat, findings) {
		t.Error("LANDING.md's findings rule isn't the spec's")
	}
	for _, command := range []string{
		"land green", "land boxes run --", "land boxes status", "land boxes claim --type", "land boxes release",
		"land boxes snapshot", "land review --prompt-file", "land review check", "land --message FILE",
		"--unchecked`", "--unchecked '#N'", "land recheck SHA", "land gate", "land docs", "land pre-push",
		"crabbox tunnel", "crabbox cp",
	} {
		if !strings.Contains(text, command) {
			t.Errorf("LANDING.md never names %q", command)
		}
	}
}

// Every crabbox command LANDING.md shows runs with land's 24-hour lease ceiling: crabbox cuts a lease to its own
// 4-hour default on every call without it, and the box land claimed shuts down under the agent.
func TestLandingsCrabboxCommandsKeepTheBoxsLease(t *testing.T) {
	text := string(landing)
	for _, command := range []string{"crabbox tunnel ", "crabbox cp "} {
		n := strings.Count(text, command)
		if n == 0 || strings.Count(text, "CRABBOX_MAX_TTL=24h "+command) != n {
			t.Errorf("LANDING.md shows %q %d times, not each run with CRABBOX_MAX_TTL=24h", command, n)
		}
	}
}

// land docs writes this version's LANDING.md at the worktree's root, from any directory in it, and Current then passes;
// a missing or edited copy fails Current, naming land docs.
func TestLandDocsWritesTheRulesThatCurrentChecks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := Current(root); err == nil || !strings.Contains(err.Error(), "run land docs") {
		t.Errorf("Current passed a repository with no LANDING.md: %v", err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if err := Main(t.Context(), []string{"extra"}); err == nil {
		t.Error("land docs took an argument")
	}
	if err := Main(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, File)); err != nil || string(data) != string(landing) {
		t.Fatalf("land docs wrote %d bytes, %v; want this version's LANDING.md at the root", len(data), err)
	}
	if err := Current(root); err != nil {
		t.Errorf("Current refused the copy land docs wrote: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, File), append([]byte("Edited.\n"), landing...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Current(root); err == nil || !strings.Contains(err.Error(), "run land docs") {
		t.Errorf("Current passed an edited LANDING.md: %v", err)
	}
}
