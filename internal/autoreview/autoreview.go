// Package autoreview is the bundled reviewer, `land autoreview BASE DIR [ARGS…]`: autoreview (openclaw/agent-skills,
// MIT) pinned per land release, adapted to the review stage's contract. upstream/PIN names the pinned commit and the
// helper's --version; the pin moves by hand in a land release.
package autoreview

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bytes032/land/internal/proc"
)

//go:embed upstream/autoreview
var helper []byte

// diffFlags are the helper's own diff flags, so land sees the change as the helper does.
var diffFlags = []string{"-r", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-abbrev",
	"--ignore-submodules=none", "--end-of-options"}

// Main is `land autoreview BASE DIR [ARGS…]`: it reviews merge-base(BASE, HEAD)..HEAD in the current repository,
// writes DIR/report.md and DIR/report.json, and passes ARGS to the helper. It succeeds exactly when the helper
// exited 0 (no findings) or 1 (findings) and wrote a non-empty JSON report.
func Main(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: land autoreview BASE DIR [ARGS…]")
	}
	base, dir, rest := args[0], args[1], args[2:]
	report, reportJSON := filepath.Join(dir, "report.md"), filepath.Join(dir, "report.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, path := range []string{report, reportJSON} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	target := []string{"--mode", "branch", "--base", base}
	commit, leftOut, err := withoutBinaries(ctx, base)
	if err != nil {
		return err
	}
	if commit != "" {
		target = []string{"--mode", "commit", "--commit", commit}
	}

	script, err := writeHelper()
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(script) }()
	// The helper keeps the last of a repeated flag, so land's target and outputs come after ARGS and always win: the
	// review is of exactly this change, whatever ARGS hold.
	argv := slices.Concat([]string{"python3", script}, rest, target, []string{"--output", report, "--json-output", reportJSON})
	runErr := proc.Run(ctx, proc.Spec{Argv: argv, Stdout: os.Stdout, Stderr: os.Stderr})
	var exit proc.Exit
	if runErr != nil && (ctx.Err() != nil || !errors.As(runErr, &exit) || exit != (proc.Exit{Code: 1})) {
		return fmt.Errorf("autoreview didn't finish: %w", runErr)
	}
	if info, err := os.Stat(reportJSON); err != nil || info.Size() == 0 {
		return fmt.Errorf("autoreview wrote no JSON report at %s", reportJSON)
	}
	if len(leftOut) > 0 {
		return appendLeftOut(report, commit, leftOut)
	}
	return nil
}

// writeHelper writes the embedded helper to a fresh temporary file and returns its path.
func writeHelper() (string, error) {
	f, err := os.CreateTemp("", "land-autoreview-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(helper)
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// withoutBinaries returns, when merge-base(base, HEAD)..HEAD holds a binary change the helper's branch mode refuses,
// an edit of an existing binary file or an added binary that isn't an image, a temporary commit on the merge-base with
// HEAD's tree but every binary change the helper's commit mode refuses left out (an edited file as it was at the
// merge-base, an added one gone), and the paths left out. Otherwise it returns "", nil: added images and deleted
// binaries go through branch mode.
func withoutBinaries(ctx context.Context, base string) (string, []string, error) {
	mergeBase, err := git(ctx, nil, "", "merge-base", "--end-of-options", base, "HEAD")
	if err != nil {
		return "", nil, err
	}
	mergeBase = strings.TrimSpace(mergeBase)
	raw, err := git(ctx, nil, "", slices.Concat([]string{"diff-tree", "--raw"}, diffFlags, []string{mergeBase, "HEAD"})...)
	if err != nil {
		return "", nil, err
	}
	numstat, err := git(ctx, nil, "", slices.Concat([]string{"diff-tree", "--numstat"}, diffFlags, []string{mergeBase, "HEAD"})...)
	if err != nil {
		return "", nil, err
	}
	binary := map[string]bool{}
	for record := range strings.SplitSeq(numstat, "\x00") {
		if path, ok := strings.CutPrefix(record, "-\t-\t"); ok {
			binary[path] = true
		}
	}

	type change struct{ oldMode, oldOID, newMode, newOID, status, path string }
	var changes []change
	var blobs []string // regular-file sides of changed files, which the helper reads for binary content
	records := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	for i := 0; i+1 < len(records); i += 2 {
		f := strings.Fields(strings.TrimPrefix(records[i], ":"))
		if len(f) != 5 {
			return "", nil, fmt.Errorf("unexpected diff-tree record %q", records[i])
		}
		c := change{f[0], f[2], f[1], f[3], f[4], records[i+1]}
		changes = append(changes, c)
		if c.status == "D" {
			continue // both modes take a deletion, binary or not
		}
		for _, side := range [][2]string{{c.oldMode, c.oldOID}, {c.newMode, c.newOID}} {
			if side[0] == "100644" || side[0] == "100755" {
				blobs = append(blobs, side[1])
			}
		}
	}
	unreadable, err := unreadableBlobs(ctx, blobs)
	if err != nil {
		return "", nil, err
	}

	// binaryChange is a change the helper takes as binary: git's numstat says so, or a side holds what it can't read.
	binaryChange := func(c change) bool {
		return c.status != "D" && (binary[c.path] || unreadable[c.oldOID] || unreadable[c.newOID])
	}
	// The helper's branch mode takes an added image and refuses any other binary change; its commit mode refuses them
	// all, added images included. Both take deletions.
	if !slices.ContainsFunc(changes, func(c change) bool { return binaryChange(c) && !(c.status == "A" && image(c.path)) }) {
		return "", nil, nil
	}
	var leftOut []string
	var index strings.Builder
	for _, c := range changes {
		switch {
		case !binaryChange(c):
			continue
		case c.status == "A":
			fmt.Fprintf(&index, "0 %s\t%s\x00", strings.Repeat("0", len(c.newOID)), c.path) // mode 0 removes it
		default:
			fmt.Fprintf(&index, "%s %s\t%s\x00", c.oldMode, c.oldOID, c.path)
		}
		leftOut = append(leftOut, c.path)
	}

	tmp, err := os.MkdirTemp("", "land-autoreview-index-*")
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := git(ctx, env, "", "read-tree", "HEAD"); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, env, index.String(), "update-index", "-z", "--index-info"); err != nil {
		return "", nil, err
	}
	tree, err := git(ctx, env, "", "write-tree")
	if err != nil {
		return "", nil, err
	}
	head, err := git(ctx, nil, "", "rev-parse", "HEAD")
	if err != nil {
		return "", nil, err
	}
	message := fmt.Sprintf("land autoreview: the change from %s to %s without its binary files\n\nLeft out, as at the base:\n- %s\n",
		mergeBase, strings.TrimSpace(head), strings.Join(leftOut, "\n- "))
	// A fixed identity and no signing, so the temporary commit never depends on the user's git configuration.
	identity := []string{"GIT_AUTHOR_NAME=land", "GIT_AUTHOR_EMAIL=land@localhost", "GIT_COMMITTER_NAME=land", "GIT_COMMITTER_EMAIL=land@localhost"}
	commit, err := git(ctx, identity, message, "commit-tree", "--no-gpg-sign", "-p", mergeBase, strings.TrimSpace(tree))
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(commit), leftOut, nil
}

// unreadableBlobs returns the blobs among oids the helper treats as binary: holding a NUL byte or not UTF-8.
func unreadableBlobs(ctx context.Context, oids []string) (map[string]bool, error) {
	unreadable := map[string]bool{}
	if len(oids) == 0 {
		return unreadable, nil
	}
	out, err := git(ctx, nil, strings.Join(oids, "\n")+"\n", "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	// Each blob is "<oid> blob <size>\n<content>\n".
	for _, oid := range oids {
		header, rest, _ := strings.Cut(out, "\n")
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != oid {
			return nil, fmt.Errorf("unexpected git cat-file output %q", header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size+1 > len(rest) {
			return nil, fmt.Errorf("unexpected git cat-file output %q", header)
		}
		content := rest[:size]
		unreadable[oid] = strings.ContainsRune(content, 0) || !utf8.ValidString(content)
		out = rest[size+1:]
	}
	return unreadable, nil
}

// appendLeftOut names the files left out of the review at the end of its report.
func appendLeftOut(report, commit string, paths []string) error {
	f, err := os.OpenFile(report, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "\n## Left out of the review\n\nThe change holds binary files the reviewer can't read, so it was reviewed as %s: the change with these files as they were at the base, an added one absent.\n\n- %s\n",
		commit, strings.Join(paths, "\n- "))
	return errors.Join(err, f.Close())
}

// image reports whether path names an image the helper's branch mode reviews: a PNG, JPEG or WebP by its suffix, read
// as the helper reads one, so neither ".png" nor "logo." has one.
func image(path string) bool {
	name := path[strings.LastIndexByte(path, '/')+1:]
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return false
	}
	switch strings.ToLower(name[dot:]) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	}
	return false
}

// git runs git in the current directory with env added to the environment and stdin as its input, and returns its
// stdout; git's stderr goes to land's.
func git(ctx context.Context, env []string, stdin string, args ...string) (string, error) {
	spec := proc.Spec{Argv: append([]string{"git"}, args...), Stdin: strings.NewReader(stdin), Stderr: os.Stderr}
	if env != nil {
		spec.Env = append(os.Environ(), env...)
	}
	return proc.Output(ctx, spec)
}
