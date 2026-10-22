package autoreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bytes032/land/internal/stage"
)

// asLand makes the test binary a stand-in `land` that runs Main, for the review stage's line.
const asLand = "LAND_AUTOREVIEW_TEST_AS_LAND"

func TestMain(m *testing.M) {
	if os.Getenv(asLand) != "" {
		if len(os.Args) < 2 || os.Args[1] != "autoreview" {
			os.Stderr.WriteString("stand-in land: not autoreview\n")
			os.Exit(2)
		}
		if err := Main(context.Background(), os.Args[2:]); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePython puts a fake python3 on PATH standing in for the helper. It records its arguments, one per line, in the
// returned file and a copy of the script it was given next to it, writes "# Review" to --output, writes $FAKE_JSON to
// --json-output when FAKE_JSON is set, and exits $FAKE_EXIT.
func fakePython(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "argv")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_LOG"
cp "$1" "$FAKE_LOG.helper"
while [ $# -gt 0 ]; do
  case $1 in
    --output) md=$2 ;;
    --json-output) json=$2 ;;
  esac
  shift
done
printf '# Review\n' > "$md"
if [ "${FAKE_JSON+set}" ]; then printf '%s' "$FAKE_JSON" > "$json"; fi
exit "${FAKE_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	return log
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo makes a scratch repository whose first commit holds files, then commits change (nil content deletes) on top,
// changes into it and returns the base commit.
func repo(t *testing.T, files, change map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "trunk")
	run(t, dir, "config", "user.name", "test")
	run(t, dir, "config", "user.email", "test@localhost")
	commit := func(files map[string][]byte) {
		for path, content := range files {
			if content == nil {
				run(t, dir, "rm", "-q", path)
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, dir, "add", path)
		}
		run(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "commit")
	}
	commit(files)
	base := run(t, dir, "rev-parse", "HEAD")
	commit(change)
	t.Chdir(dir)
	return base
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestExitFollowsHelperExitAndJSONReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit string
		json *string
		ok   bool
	}{
		{"clean with report", "0", new(`{"findings":[]}`), true},
		{"findings with report", "1", new(`{"findings":[{}]}`), true},
		{"failure with report", "2", new(`{"findings":[]}`), false},
		{"clean without report", "0", nil, false},
		{"findings without report", "1", nil, false},
		{"failure without report", "2", nil, false},
		{"clean with empty report", "0", new(""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := fakePython(t)
			t.Setenv("FAKE_EXIT", tc.exit)
			if tc.json != nil {
				t.Setenv("FAKE_JSON", *tc.json)
			}
			base := repo(t, map[string][]byte{"a.txt": []byte("a\n")}, map[string][]byte{"a.txt": []byte("b\n")})
			out := filepath.Join(t.TempDir(), "review")
			err := Main(context.Background(), []string{base, out, "--max-priority", "P1"})
			if (err == nil) != tc.ok {
				t.Fatalf("Main = %v, want ok=%v", err, tc.ok)
			}
			argv := lines(t, log)
			want := []string{"--max-priority", "P1", "--mode", "branch", "--base", base, "--output", filepath.Join(out, "report.md"),
				"--json-output", filepath.Join(out, "report.json")}
			if !slices.Equal(argv[1:], want) {
				t.Errorf("helper argv = %q, want %q", argv[1:], want)
			}
			if copied, err := os.ReadFile(log + ".helper"); err != nil || !bytes.Equal(copied, helper) {
				t.Errorf("python3 wasn't given the embedded helper (%v)", err)
			}
			if _, err := os.Stat(argv[0]); !os.IsNotExist(err) {
				t.Errorf("temporary helper %s left behind: %v", argv[0], err)
			}
		})
	}
}

func TestReviewStageLinePassesBaseDirAndArgs(t *testing.T) {
	log := fakePython(t)
	t.Setenv("FAKE_JSON", "{}")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "land"), []byte("#!/bin/sh\nexec "+self+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(asLand, "1")
	base := repo(t, map[string][]byte{"a.txt": []byte("a\n")}, map[string][]byte{"a.txt": []byte("b\n"), "ticket.md": []byte("Do b.\n")})
	out := filepath.Join(t.TempDir(), "review")
	root, _ := os.Getwd()

	err = stage.Run(context.Background(), stage.Spec{
		Dir:     root,
		Command: `land autoreview "$@" --max-priority P1`,
		Args:    []string{base, out, "--prompt-file", "ticket.md"},
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		t.Fatalf("review stage: %v", err)
	}
	want := []string{"--prompt-file", "ticket.md", "--max-priority", "P1", "--mode", "branch", "--base", base,
		"--output", filepath.Join(out, "report.md"), "--json-output", filepath.Join(out, "report.json")}
	if argv := lines(t, log); !slices.Equal(argv[1:], want) {
		t.Errorf("helper argv = %q, want %q", argv[1:], want)
	}
}

func TestBinaryEditIsReviewedAsCommitWithoutIt(t *testing.T) {
	log := fakePython(t)
	t.Setenv("FAKE_JSON", "{}")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	base := repo(t,
		map[string][]byte{"logo.png": png, "latin1.txt": []byte("caf\xe9\n"), "old.bin": []byte("\x00old"), "a.txt": []byte("a\n")},
		map[string][]byte{"logo.png": append(png, 1), "latin1.txt": []byte("caf\xe9s\n"), "old.bin": nil,
			"new.png": png, "a.txt": []byte("b\n")})
	out := filepath.Join(t.TempDir(), "review")
	if err := Main(context.Background(), []string{base, out}); err != nil {
		t.Fatal(err)
	}
	argv := lines(t, log)
	if len(argv) < 5 || !slices.Equal(argv[1:4], []string{"--mode", "commit", "--commit"}) {
		t.Fatalf("helper argv = %q, want commit mode", argv)
	}
	commit := argv[4]
	if parent := run(t, ".", "rev-parse", commit+"^"); parent != base {
		t.Errorf("temporary commit's parent = %s, want the base %s", parent, base)
	}
	// Only the text edit and the binary deletion are left for the reviewer.
	if changed := run(t, ".", "diff", "--name-status", "--no-renames", base, commit); changed != "M\ta.txt\nD\told.bin" {
		t.Errorf("temporary commit changes\n%s\nwant the text edit and the deletion only", changed)
	}
	report, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"latin1.txt", "logo.png", "new.png"} {
		if !strings.Contains(string(report), "- "+path+"\n") {
			t.Errorf("report doesn't name %s as left out:\n%s", path, report)
		}
	}
	if strings.Contains(string(report), "a.txt") || strings.Contains(string(report), "old.bin") {
		t.Errorf("report names a file that was reviewed:\n%s", report)
	}
}

// Added images, whatever their suffix's case, go through branch mode, which reviews them.
func TestAddedImagesGoThroughBranchMode(t *testing.T) {
	log := fakePython(t)
	t.Setenv("FAKE_JSON", "{}")
	base := repo(t, map[string][]byte{"a.txt": []byte("a\n")},
		map[string][]byte{"new.png": []byte("\x89PNG\r\n\x1a\n\x00"), "photo.JPEG": []byte("\xff\xd8\xff\x00"), "a.txt": []byte("b\n")})
	out := filepath.Join(t.TempDir(), "review")
	if err := Main(context.Background(), []string{base, out}); err != nil {
		t.Fatal(err)
	}
	if argv := lines(t, log); !slices.Equal(argv[1:5], []string{"--mode", "branch", "--base", base}) {
		t.Errorf("helper argv = %q, want branch mode", argv)
	}
	if report, _ := os.ReadFile(filepath.Join(out, "report.md")); string(report) != "# Review\n" {
		t.Errorf("report = %q, want the helper's own", report)
	}
}

// A change that adds a binary file the reviewer can't read, a font or a file that isn't UTF-8, is reviewed as a
// temporary commit without it, and without any other added binary, an image included, which commit mode refuses; the
// report names each file left out.
func TestAddedBinaryIsReviewedAsCommitWithoutIt(t *testing.T) {
	log := fakePython(t)
	t.Setenv("FAKE_JSON", "{}")
	base := repo(t, map[string][]byte{"a.txt": []byte("a\n")}, map[string][]byte{
		"font.woff2": []byte("wOF2\x00\x01\x00\x00"), "latin1.txt": []byte("caf\xe9\n"),
		"new.png": []byte("\x89PNG\r\n\x1a\n\x00"), "a.txt": []byte("b\n"),
	})
	out := filepath.Join(t.TempDir(), "review")
	if err := Main(context.Background(), []string{base, out}); err != nil {
		t.Fatal(err)
	}
	argv := lines(t, log)
	if len(argv) < 5 || !slices.Equal(argv[1:4], []string{"--mode", "commit", "--commit"}) {
		t.Fatalf("helper argv = %q, want commit mode", argv)
	}
	if changed := run(t, ".", "diff", "--name-status", "--no-renames", base, argv[4]); changed != "M\ta.txt" {
		t.Errorf("temporary commit changes\n%s\nwant the text edit only", changed)
	}
	report, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"font.woff2", "latin1.txt", "new.png"} {
		if !strings.Contains(string(report), "- "+path+"\n") {
			t.Errorf("report doesn't name %s as left out:\n%s", path, report)
		}
	}
}

// ARGS can't point the review at another range or move its reports: the helper keeps the last of a repeated flag, and
// land's own come last.
func TestArgsCantRedirectTheReview(t *testing.T) {
	log := fakePython(t)
	t.Setenv("FAKE_JSON", "{}")
	base := repo(t, map[string][]byte{"a.txt": []byte("a\n")}, map[string][]byte{"a.txt": []byte("b\n")})
	out, elsewhere := filepath.Join(t.TempDir(), "review"), t.TempDir()
	err := Main(context.Background(), []string{base, out, "--mode", "commit", "--commit", "HEAD~1", "--base", "HEAD~1",
		"--output", filepath.Join(elsewhere, "r.md"), "--json-output", filepath.Join(elsewhere, "r.json")})
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	argv := lines(t, log)
	for i := 1; i+1 < len(argv); i++ {
		if strings.HasPrefix(argv[i], "--") {
			last[argv[i]] = argv[i+1]
		}
	}
	want := map[string]string{"--mode": "branch", "--base": base, "--output": filepath.Join(out, "report.md"),
		"--json-output": filepath.Join(out, "report.json")}
	for flag, value := range want {
		if last[flag] != value {
			t.Errorf("the helper reads %s as %q, want land's %q; argv %q", flag, last[flag], value, argv)
		}
	}
}

func TestPinRecordsEmbeddedHelpersVersion(t *testing.T) {
	sum := sha256.Sum256(helper)
	pin, err := os.ReadFile("upstream/PIN")
	if err != nil {
		t.Fatal(err)
	}
	if want := "version: autoreview source-sha256:" + hex.EncodeToString(sum[:]) + "\n"; !strings.Contains(string(pin), want) {
		t.Errorf("upstream/PIN doesn't record the embedded helper's --version %q", want)
	}
}
