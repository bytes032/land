package boxes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bytes032/land/internal/proc"
)

// box is a worktree's claimed box.
type box struct {
	p *pool
	c claim
}

func (b *box) Run(ctx context.Context, fetch []string, argv ...string) error {
	return b.run(ctx, b.p.cfg.Stages.Prepare, fetch, argv...)
}

// run is Run with prepare, the command the box script runs before argv, "" for none: a box readied by setup has none.
// crabbox uploads the box script from stdin, content-hashed under the workspace's ignored .crabbox/, and runs it with
// argv, so that it prints only argv, not the script, on every command.
func (b *box) run(ctx context.Context, prepare string, fetch []string, argv ...string) error {
	var reached stages
	err := b.p.crabboxIn(ctx, b.c.Box, strings.NewReader("set -eo pipefail\n"+b.p.script(prepare)), b.p.stdout,
		io.MultiWriter(b.p.stderr, &reached), append([]string{"run", "--id", b.c.Lease, "--reclaim", "--script-stdin", "--"}, argv...)...)
	if err != nil && reached.stage != "command" {
		// The script says when it stops itself; anything else that ends a box command before its command is
		// crabbox's, the box's or the connection's.
		at := "sync"
		if reached.stopped {
			at = reached.stage
		}
		return &StageError{Stage: at, Err: err, Log: reached.tail()}
	}
	errs := []error{err}
	for _, rel := range fetch {
		if ctx.Err() == nil {
			errs = append(errs, b.fetch(ctx, rel))
		}
	}
	return errors.Join(errs...)
}

// script is the core's box script, which `bash -eo pipefail -c SCRIPT land CMD...` runs around every box command
// CMD, in crabbox's workspace for the lease. In order, it enters the build root, and stops unless the workspace links
// there; sets box.env; turns apt's daily timers off if either is on, which a box from an image that has them on would;
// makes the build root a git repository of the box's own if it isn't one; removes each file Git doesn't ignore that
// crabbox's sync manifest doesn't list; gives each file back the time the box last gave it; runs prepare; runs CMD;
// and records the tree CMD left and its files' times. It names each stage on stderr as it starts it, and says if it
// stops before CMD (stages).
//
// crabbox prunes only what its previous sync listed, so a file the box got another way, from its image or an earlier
// command, would outlive the worktree's deletion of it and enter the box's tree. The manifest is trusted only if it
// lists .land.yaml, which every worktree has, and a path under a link, which the box's index can still name after the
// sync made its directory a link, is left alone, so rm never follows a link out of the tree.
//
// crabbox's copy gives each file its time on the Mac, which can be older than a build on the box from other content,
// or newer than every build of the same content, so build tools that compare times would rebuild or, worse, not: so
// each file gets back the time the box recorded for it, in .git/land-times with the tree it recorded, and the current
// time if its content changed since, in the copy or in an index the record doesn't describe, which a killed command
// left. The box's repository ignores crabbox's .crabbox/, which holds crabbox's manifest, and land's .land/, neither of
// them repository files; a snapshot carries the repository, and with it what it ignores.
//
// Box commands may run side by side; under a lock, one at a time readies the build root and records it.
// shortcut: a CMD killed after changing tracked files leaves them unrecorded, so a build may keep artifacts built from
// them once the copy restores the old content; record the tree from a trap if that ever bites.
func (p *pool) script(prepare string) string {
	var exports strings.Builder
	for _, name := range slices.Sorted(maps.Keys(p.cfg.Box.Env)) {
		exports.WriteString("export " + quote(name+"="+p.cfg.Box.Env[name]) + "\n")
	}
	if prepare != "" {
		prepare = "printf 'land: %s\\n' prepare >&2\n" +
			"bash -eo pipefail -c " + quote(prepare) + " land 9>&- >&2\n"
	}
	return `printf 'land: %s\n' script >&2
trap 'printf "land: %s\n" stopped >&2' EXIT
cd -P .
if [ "$PWD" != ` + quote(p.build) + ` ]; then
  echo "land: the command runs in $PWD, not in ` + p.build + `: this lease's workspace isn't linked to the build root" >&2
  exit 1
fi
` + exports.String() + `if systemctl --quiet is-enabled apt-daily.timer apt-daily-upgrade.timer ||
  systemctl --quiet is-active apt-daily.timer apt-daily-upgrade.timer; then
  echo "land: turning apt's daily timers off" >&2
  sudo systemctl disable --now apt-daily.timer apt-daily-upgrade.timer </dev/null >&2
fi
exec 9>>"${TMPDIR:-/tmp}/land.lock"
flock 9
if [ ! -d .git ]; then
  git init -q
  printf '%s\n' /.crabbox/ /.land/ >>.git/info/exclude
fi
manifest=$(git rev-parse --git-path crabbox/sync-manifest)
if [ -f "$manifest" ]; then
  git ls-files -z --cached --others --exclude-standard | perl -0 -ne '
    BEGIN {
      open(my $m, "<", shift) or die "land: $!\n";
      while (<$m>) { chomp; $listed{$_} = 1 }
      $trusted = $listed{".land.yaml"} or print STDERR "land: the sync manifest lists no .land.yaml, so no file is removed\n";
    }
    chomp; next if !$trusted || $listed{$_};
    my @dirs = split m{/}; pop @dirs; my $dir = "";
    for (@dirs) { $dir .= $_; next LINE if -l $dir || !-d _; $dir .= "/" }
    print "$_\0" if -l $_ || -f _' "$manifest" | xargs -0r rm -f --
fi
record() {
  git add -A && { git write-tree | tr '\n' '\0' && git ls-files -z |
    perl -MTime::HiRes=lstat -0lne 'my @s = lstat $_ or next; -l $_ or printf "%s\0%.9f\0", $_, $s[9]'; } \
    >.git/land-times.new && mv .git/land-times.new .git/land-times
}
if [ -f .git/land-times ] && IFS= read -r -d '' recorded <.git/land-times &&
  git diff -z --name-only --no-renames "$recorded^{tree}" "$(git write-tree)" >.git/land-moved 2>/dev/null; then
  perl -MTime::HiRes=utime -0lne '$. > 1 or next; chomp(my $time = <>); utime $time, $time, $_' .git/land-times
else
  : >.git/land-moved
fi
{ cat .git/land-moved && git diff -z --name-only --no-renames && git ls-files -z --others --exclude-standard; } |
  xargs -0r touch -c --
record
` + prepare + `exec 9>&-
trap - EXIT
printf 'land: %s\n' command >&2
code=0
"$@" || code=$?
exec 9>>"${TMPDIR:-/tmp}/land.lock"
flock 9
record || echo "land: the box couldn't record its tree, so its next command may rebuild what it needn't" >&2
sudo -n ` + runDir + `/rearm 2>/dev/null || true
exit $code
`
}

// stages follows what the box script writes on stderr before the command: `land: STAGE` as it starts each stage, and
// `land: stopped` if it stops before the command. stage is the last it started, "" before the script's own.
type stages struct {
	stage   string
	stopped bool
	line    []byte
	log     []byte // the end of what was written since the stage started, up to a stop
}

// A stage's log, StageError.Log, is at most logLines lines of at most logBytes.
const logLines, logBytes = 40, 4000

func (s *stages) Write(p []byte) (int, error) {
	for _, c := range p {
		switch {
		case s.stage == "command", s.stopped:
			return len(p), nil // what follows is the command's own, or crabbox's
		case c != '\n':
			if len(s.line) <= len("land: command") { // a longer line is no stage line
				s.line = append(s.line, c)
			}
		default:
			line := string(s.line)
			s.line = s.line[:0]
			switch line {
			case "land: script", "land: prepare", "land: command":
				s.stage, s.log = strings.TrimPrefix(line, "land: "), s.log[:0]
				continue
			case "land: stopped":
				s.stopped, s.log = true, s.log[:len(s.log)-len(line)]
				continue
			}
		}
		if s.log = append(s.log, c); len(s.log) > 2*logBytes {
			s.log = append(s.log[:0], s.log[len(s.log)-logBytes:]...)
		}
	}
	return len(p), nil
}

// tail is the end of the stage's log: whole lines, at most logLines of them in at most logBytes.
func (s *stages) tail() string {
	log := strings.TrimRight(string(s.log), "\n")
	if len(log) > logBytes {
		log = log[len(log)-logBytes:]
		log = log[strings.IndexByte(log, '\n')+1:]
	}
	lines := strings.Split(log, "\n")
	return strings.Join(lines[max(len(lines)-logLines, 0):], "\n")
}

// fetch copies rel from the box's build root over the worktree's copy, with tar over the SSH command crabbox prints for
// the box. The copy lands beside the worktree's, on its filesystem, and the worktree's moves aside only once the new
// one is whole, and back if the new one can't take its place.
// shortcut: a fetch killed midway leaves a .land-fetch-* directory beside rel, which the next command syncs to the box.
func (b *box) fetch(ctx context.Context, rel string) error {
	local := filepath.Join(b.p.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(local), ".land-fetch-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	ssh, err := b.p.ssh(ctx, b.c)
	if err != nil {
		return fmt.Errorf("fetching %s from the box: %w", rel, err)
	}
	remote, copied, old := path.Join(b.p.build, rel), filepath.Join(temp, "new"), filepath.Join(temp, "old")
	if err := os.Mkdir(copied, 0o700); err != nil {
		return err
	}
	// The box's tar packs rel under its last name, which the Mac's unpacks in new/. bash reads crabbox's quoting of the
	// SSH command's words, and pipefail fails the copy when the box's tar does.
	if err := proc.Run(ctx, proc.Spec{
		Argv: []string{"bash", "-o", "pipefail", "-c", ssh + ` "$1" | tar -xf - -C "$2"`, "land",
			"tar -C " + quote(path.Dir(remote)) + " -cf - " + quote(path.Base(remote)), copied},
		Stderr: b.p.stderr,
	}); err != nil {
		return fmt.Errorf("fetching %s from the box: %w", rel, err)
	}
	if err := os.Rename(local, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(filepath.Join(copied, path.Base(remote)), local); err != nil {
		return errors.Join(err, os.Rename(old, local))
	}
	return nil
}
