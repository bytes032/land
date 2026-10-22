package provenance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/provenance"
)

// Agent-Session is the root session's id whichever agent lands, Agent is Main or the subagent's transcript name, and
// any other layout fails closed.
func TestFromEnvTakesTheRootSessionAndTheAgentsName(t *testing.T) {
	dir := t.TempDir()
	const folder, root = "2026-10-21T07-22-47-173Z_01a15d57-5605-7160-972b-d21edf620e25", "01a15d57-5605-7160-972b-d21edf620e25"
	for _, c := range []struct{ path, agent, fails string }{
		{"sessions/-wt/" + folder + ".jsonl", "Main", ""},
		{"sessions/-wt/" + folder + "/Lander.jsonl", "Lander", ""},
		{"sessions/-wt/" + folder + "/Coordinator/Coordinator.Lander-2.jsonl", "Coordinator.Lander-2", ""},
		{"sessions/-wt/" + folder + "/__advisor.jsonl", "", "names no agent"},
		{"sessions/-wt/" + folder + "/Bad\nUnchecked: #1.jsonl", "", "names no agent"},
		{"sessions/-wt/Lander.jsonl", "", "no omp session folder"},
		{"sessions/-wt/" + folder + "/Lander.log", "", "no omp transcript"},
		{"sessions/-wt/" + folder + "/Folder.jsonl/", "", "no omp transcript"},
	} {
		path := filepath.Join(dir, c.path)
		if strings.HasSuffix(c.path, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, path, "{}\n")
		}
		t.Setenv("PI_SESSION_FILE", path)
		id, err := provenance.FromEnv()
		switch {
		case c.fails == "" && (err != nil || id != provenance.Identity{Session: root, Agent: c.agent}):
			t.Errorf("%q: got %+v, %v; want %s, %s", c.path, id, err, root, c.agent)
		case c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)):
			t.Errorf("%q: got %+v, %v; want an error with %q", c.path, id, err, c.fails)
		}
	}
	for path, fails := range map[string]string{"": "unset", filepath.Join(dir, "sessions/-wt", folder, "Gone.jsonl"): "no such file"} {
		t.Setenv("PI_SESSION_FILE", path)
		if id, err := provenance.FromEnv(); err == nil || !strings.Contains(err.Error(), fails) {
			t.Errorf("PI_SESSION_FILE=%q: got %+v, %v; want an error with %q", path, id, err, fails)
		}
	}
}

// The trailer block is the message's last paragraph, which git's own parser reads as exactly land's trailers, even
// after a paragraph of the author's that looks like trailers; a message carrying land's trailers, in any case, or none
// at all, is refused.
func TestMessageEndsWithTheTrailerBlock(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull) // no trailer.* setting of the machine's reaches git's parser
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	id := provenance.Identity{Session: "S", Agent: "A"}
	for unchecked, want := range map[string]string{"": "Agent-Session: S\nAgent: A\n", "#9": "Unchecked: #9\nAgent-Session: S\nAgent: A\n"} {
		got, err := id.Message("feat: x\n\nWhy.\n\nRefs: #4\n\n\n", unchecked)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "feat: x\n\nWhy.\n\nRefs: #4\n\n") {
			t.Errorf("the author's text changed: %q", got)
		}
		cmd := exec.Command("git", "interpret-trailers", "--parse")
		cmd.Stdin = strings.NewReader(got)
		if parsed, err := cmd.Output(); err != nil || string(parsed) != want {
			t.Errorf("git parses the trailers of %q as %q, %v; want %q", got, parsed, err, want)
		}
	}
	for _, text := range []string{
		"feat: x\n\nAgent: B\n", "feat: x\n\nagent-session: T\n\nCloses #3\n", "feat: x\n\nUnchecked : #9\n", " \n\t\n",
	} {
		if got, err := id.Message(text, ""); err == nil {
			t.Errorf("%q was accepted as %q", text, got)
		}
	}
	if _, err := id.Message("feat: x\n\nAgents: many. The Agent: one\n", ""); err != nil {
		t.Errorf("a line that only mentions an agent was refused: %v", err)
	}
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
