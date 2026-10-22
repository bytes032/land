package stage_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/stage"
)

// A stage command runs at its directory with its inputs as $1, $2, …, in land's environment but with the running
// binary's directory first on PATH, so a `land` it runs is this one; and under -eo pipefail, so a failed stage of a
// pipe fails it.
func TestRunGivesTheCommandItsInputsAndThisLand(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("LAND_STAGE_TEST", "kept")
	var out strings.Builder
	err = stage.Run(t.Context(), stage.Spec{
		Dir:     dir,
		Command: `printf '%s\n' "${PATH%%:*}" "$LAND_STAGE_TEST" "$1" "$2" "$(pwd -P)"`,
		Args:    []string{"one", "two words"},
		Stdout:  &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), strings.Join([]string{filepath.Dir(self), "kept", "one", "two words", physical}, "\n")+"\n"; got != want {
		t.Errorf("the command saw\n%s\nwant\n%s", got, want)
	}

	out.Reset()
	err = stage.Run(t.Context(), stage.Spec{Dir: dir, Command: "false | true; echo reached", Stdout: &out})
	var exit proc.Exit
	if !errors.As(err, &exit) || exit.Code != 1 || out.String() != "" {
		t.Errorf("a failed pipe stage: %v, output %q; want exit 1 before the next line", err, out.String())
	}
}
