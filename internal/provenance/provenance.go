// Package provenance writes a landing's trailer block (Agent-Session, Agent, and Unchecked on an unchecked landing) and
// reads trailers from any commit with git's own parser. omp is the only identity adapter.
package provenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Trailer keys.
const (
	AgentSession = "Agent-Session"
	Agent        = "Agent"
	Unchecked    = "Unchecked"
)

// Identity is the agent landing the work.
type Identity struct {
	Session string // the root omp session id
	Agent   string // "Main", or the subagent's name
}

var (
	// session is an omp session's transcript or folder name, <time>_<session id>.
	session = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_([0-9A-Za-z-]+)$`)
	// agent is a subagent's transcript name: its name, or <parent>.<name> for a subagent's own subagent. Anything else,
	// such as omp's own __advisor, is no agent land attributes a landing to.
	agent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	// ours is a line carrying one of land's trailers, its key in any case, as git's parser matches keys.
	ours = regexp.MustCompile(`(?im)^(` + AgentSession + `|` + Agent + `|` + Unchecked + `)[ \t]*:`)
)

// FromEnv derives the identity from omp's PI_SESSION_FILE (omp >= 18.12.4), the agent's own transcript: Main's is
// sessions/<folder>/<time>_<session id>.jsonl, and a subagent's is <name>.jsonl under that session's folder,
// <time>_<session id>/, nested once more per level of subagent. Agent-Session is always the root session's id, never a
// subagent's own. It fails closed on any other layout.
func FromEnv() (Identity, error) {
	path := os.Getenv("PI_SESSION_FILE")
	if path == "" {
		return Identity{}, errors.New("PI_SESSION_FILE is unset: land lands only from omp >= 18.12.4, which sets it to the agent's transcript")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, fmt.Errorf("PI_SESSION_FILE: %w", err)
	}
	name, ok := strings.CutSuffix(filepath.Base(path), ".jsonl")
	if !ok || !info.Mode().IsRegular() {
		return Identity{}, fmt.Errorf("PI_SESSION_FILE %s is no omp transcript, a .jsonl file", path)
	}
	if m := session.FindStringSubmatch(name); m != nil {
		return Identity{Session: m[1], Agent: "Main"}, nil
	}
	if !agent.MatchString(name) {
		return Identity{}, fmt.Errorf("PI_SESSION_FILE %s names no agent land knows: an omp session's or a subagent's transcript", path)
	}
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if m := session.FindStringSubmatch(filepath.Base(dir)); m != nil {
			return Identity{Session: m[1], Agent: name}, nil
		}
	}
	return Identity{}, fmt.Errorf("PI_SESSION_FILE %s is in no omp session folder, <time>_<session id>", path)
}

// Message is a landing commit's message: text, which must carry none of land's trailers, with the trailer block as its
// last paragraph, where git reads trailers: Unchecked when unchecked isn't "", then Agent-Session and Agent.
func (id Identity) Message(text, unchecked string) (string, error) {
	text = strings.TrimRight(text, " \t\r\n")
	switch {
	case text == "":
		return "", errors.New("the message is empty")
	case ours.MatchString(text):
		return "", fmt.Errorf("the message carries an %s, %s or %s line, which land writes itself", AgentSession, Agent, Unchecked)
	}
	block := ""
	if unchecked != "" {
		block = Unchecked + ": " + unchecked + "\n"
	}
	return fmt.Sprintf("%s\n\n%s%s: %s\n%s: %s\n", text, block, AgentSession, id.Session, Agent, id.Agent), nil
}

// Trailer is the value of the commit's trailer key, or "" when it has none.
func Trailer(ctx context.Context, dir, sha, key string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "log", "-1", "--format=%(trailers:key="+key+",valueonly,separator=%x00)", sha, "--").Output()
	if err != nil {
		return "", err
	}
	value, _, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\x00")
	return strings.TrimSpace(value), nil
}
