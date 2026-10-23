// Package check is the check at the push, end to end: the box half runs the selected checks at the commit's tree and
// writes the result file; the Mac half reads it and classifies success, failure or error.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bytes032/land/internal/boxes"
	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/stage"
)

// Dir is where the box half writes the result file and each check's log, relative to the repository root on the box
// and on the Mac, where the box command copies it back. A .gitignore of its own keeps git out of it on both.
const Dir = ".land"

// ResultFile is the result's path relative to the repository root, on the box and after it is fetched back.
const ResultFile = Dir + "/result.json"

// Result is the box half's result file.
type Result struct {
	Tree   string        `json:"tree"`  // the tree the box held
	Stage  string        `json:"stage"` // "check" once checks ran; else where the run stopped: "tree"
	Checks []CheckResult `json:"checks"`
}

// CheckResult is one check's outcome.
type CheckResult struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Exit    int    `json:"exit"`
	Log     string `json:"log"` // repository-relative, fetched back with the result
	Tail    string `json:"-"`   // the log's last lines, read on the Mac for a failed check
}

// Outcome is the check's classification.
type Outcome struct {
	State  forge.State // success, failure or error
	Stage  string      // where an error stopped: a *boxes.StageError's stage, "box", "result", "tree", or a caller's own
	Failed []CheckResult
	Detail string // a one-line description for the mark
	Err    error  // why an error stopped: the box command's error, a *boxes.StageError when one of its stages failed
}

const (
	tailLines = 40
	tailBytes = 4000
)

// script is the box half, run by bash at the box's repository root with the commit's tree, then each check's name
// and command, as its arguments. It removes the last result first, then reads the tree the box holds as git would
// commit it, and when that isn't the commit's tree, writes a result at stage "tree" and stops before any check.
// Otherwise it runs every check, each under bash -eo pipefail -c to its own log, and writes each one's exit. It exits
// 0 exactly when it wrote the result, whatever the checks did.
// shortcut: a file the repository tracks though .gitignore ignores it never enters the box's index, so its commit
// fails the tree check; force-add such paths here if a repository ever tracks one.
const script = `set -u
tree=$1
shift
rm -rf .land && mkdir .land && echo '*' > .land/.gitignore || exit
# json prints $1 as a JSON string.
json() {
	local LC_ALL=C s=$1 c u
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	while [[ $s =~ [[:cntrl:]] ]]; do
		c=${BASH_REMATCH[0]}
		printf -v u '\\u%04x' "'$c"
		s=${s//"$c"/"$u"}
	done
	printf '"%s"' "$s"
}
# result writes the result file: the stage reached, $1, and the checks' results, $2.
result() {
	printf '{"tree":%s,"stage":"%s","checks":[%s]}\n' "$(json "$found")" "$1" "$2" > .land/result.json.part &&
		mv .land/result.json.part .land/result.json
}
found=$(git add -A && git write-tree) || found=
if [ "$found" != "$tree" ]; then
	echo "land: the box holds tree ${found:-(none)}, not the commit's $tree, so no check runs" >&2
	result tree ''
	exit
fi
checks= i=0
while [ $# -ge 2 ]; do
	i=$((i + 1)) name=$1 run=$2
	shift 2
	code=0
	` + stage.Bash + ` "$run" land > ".land/$i.log" 2>&1 < /dev/null || code=$?
	if [ "$code" = 0 ]; then
		echo "land: check $name passed" >&2
	else
		echo "land: check $name failed with exit $code; its log is .land/$i.log" >&2
	fi
	checks+="${checks:+,}{\"name\":$(json "$name"),\"command\":$(json "$run"),\"exit\":$code,\"log\":\".land/$i.log\"}"
done
result check "$checks"
`

// Run runs checks on box at tree, the commit's, and classifies the result: success when the box held tree and every
// check exited 0; failure, with each failed check, when the box held tree and one didn't; error, naming the stage
// where the box command stopped, for anything else. Its error, a local one or ctx ending the run, has no verdict.
func Run(ctx context.Context, box boxes.Box, root, tree string, checks []config.Check) (Outcome, error) {
	// A result an earlier command left here must never stand for this one.
	if err := os.RemoveAll(filepath.Join(root, Dir)); err != nil {
		return Outcome{}, err
	}
	argv := []string{"bash", "-c", script, "land", tree}
	for _, c := range checks {
		argv = append(argv, c.Name, c.Run)
	}
	if err := box.Run(ctx, []string{Dir}, argv...); err != nil {
		if ctx.Err() != nil {
			return Outcome{}, err
		}
		// A box that stopped before the command may have copied back what an earlier command left on it.
		stage, why := "box", err.Error()
		if stopped := (*boxes.StageError)(nil); errors.As(err, &stopped) {
			stage, why = stopped.Stage, stopped.Err.Error()
		}
		out := Stopped(stage, why)
		out.Err = err
		return out, nil
	}
	var res Result
	data, err := read(root, ResultFile)
	if err == nil {
		err = json.Unmarshal(data, &res)
	}
	switch {
	case err != nil:
		return Stopped("result", "no result came back from the box: "+err.Error()), nil
	case res.Tree != tree:
		return Stopped("tree", fmt.Sprintf("the box held tree %s, not the commit's %s", res.Tree, tree)), nil
	case res.Stage != "check" || len(res.Checks) != len(checks):
		return Stopped(res.Stage, fmt.Sprintf("the box stopped at %s after %d of %d checks", res.Stage, len(res.Checks), len(checks))), nil
	}
	var names, failures []string
	out := Outcome{State: forge.Success}
	for _, c := range res.Checks {
		names = append(names, c.Name)
		if c.Exit != 0 {
			c.Tail = tail(root, c.Log)
			out.Failed = append(out.Failed, c)
			failures = append(failures, c.Name)
		}
	}
	if len(out.Failed) > 0 {
		out.State, out.Detail = forge.Failure, "The check at the push failed: "+strings.Join(failures, ", ")
		return out, nil
	}
	out.Detail = "The check at the push passed: " + strings.Join(names, ", ")
	return out, nil
}

// stoppedAt begins an error outcome's detail, and so its mark's description, which names the stage next.
const stoppedAt = "No verdict: the box stopped at "

// Stopped is an error outcome at stage, for why.
func Stopped(stage, why string) Outcome {
	return Outcome{State: forge.Error, Stage: stage, Detail: stoppedAt + stage + ": " + why}
}

// StageOf is the stage an error mark's description names, as Stopped wrote it; "" for any other description.
func StageOf(description string) string {
	rest, ok := strings.CutPrefix(description, stoppedAt)
	stage, _, named := strings.Cut(rest, ":")
	if !ok || !named {
		return ""
	}
	return stage
}

// read reads path, a file of Dir that came back from the box. The box runs the repository's commands, so only a
// regular file in Dir itself is read: never through a symlink, which could read one of the Mac's files instead, nor
// from a FIFO, which would never end.
func read(root, path string) ([]byte, error) {
	if filepath.Dir(path) != Dir {
		return nil, fmt.Errorf("%s isn't in %s", path, Dir)
	}
	dir, err := os.Lstat(filepath.Join(root, Dir))
	if err != nil {
		return nil, err
	}
	file, err := os.Lstat(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || !file.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file in the directory %s", path, Dir)
	}
	return os.ReadFile(filepath.Join(root, path))
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// tail is the last lines of a check's log, its colours removed; "" when it can't be read.
func tail(root, log string) string {
	data, err := read(root, log)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(ansi.ReplaceAllString(string(data), ""), "\n"), "\n")
	t := strings.Join(lines[max(0, len(lines)-tailLines):], "\n")
	return strings.ToValidUTF8(t[max(0, len(t)-tailBytes):], "")
}
