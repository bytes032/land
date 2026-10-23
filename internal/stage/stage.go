// Package stage runs a repository's stage command on the Mac: `bash -eo pipefail -c`, at the repository root, with the
// stage's inputs as $1, $2, …, and the running land binary's directory first on PATH, so a `land …` inside the command
// runs the same pinned version. On the box, the core's scripts run stage commands with the same Bash line.
package stage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bytes032/land/internal/proc"
)

// Spec is one stage command to run.
type Spec struct {
	Dir     string   // the repository root
	Command string   // the repository's command, as .land.yaml writes it
	Args    []string // the stage's inputs, as $1, $2, …
	Stdout  io.Writer
	Stderr  io.Writer
}

// Bash is how every stage command runs, on the Mac and in the box's scripts: `Bash COMMAND land ARGS…`, so the
// command's $0 is land and its inputs are $1, $2, ….
const Bash = "bash -eo pipefail -c"

// Argv is the command line that runs spec's command: Bash COMMAND land ARGS….
func Argv(command string, args []string) []string {
	return slices.Concat(strings.Fields(Bash), []string{command, "land"}, args)
}

// Run runs the stage command and returns its error; a non-zero exit is a proc.Exit error.
func Run(ctx context.Context, spec Spec) error {
	env := os.Environ()
	if self, err := os.Executable(); err == nil {
		// os/exec keeps the last of a variable set twice, so this PATH is the command's.
		env = append(env, "PATH="+filepath.Dir(self)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return proc.Run(ctx, proc.Spec{
		Argv:   Argv(spec.Command, spec.Args),
		Dir:    spec.Dir,
		Env:    env,
		Stdout: spec.Stdout,
		Stderr: spec.Stderr,
	})
}
