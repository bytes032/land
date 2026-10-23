package boxes

import (
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bytes032/land/internal/stage"
)

// boxCode is land's box code: every script land runs on a box, which a caller asks for by what it does, and where those
// scripts keep things there, which it holds: the build root of one repository, the self-delete's directory, and
// Hetzner's metadata service. hetznerBox is a real box's; a test's fake boxes keep theirs in directories of their own.
// The scripts tell land how far a box command got on stderr (stages), and what a new box's image lacks on stdout
// (needs).
type boxCode struct {
	build    string // the repository's build root, which crabbox's workspace for every lease links to
	run      string // the self-delete's directory, whose files runFiles names
	metadata string // Hetzner's metadata service, which names the server the self-delete deletes
}

// hetznerBox is the box code of the repository repo, <owner>.<name>, on a box crabbox makes on Hetzner: its build root
// is /work/<owner>.<name>, and its self-delete in /run, in memory, so that no snapshot of the box's disk carries it,
// and none outlives a reboot, after which the sweep is the backstop.
func hetznerBox(repo string) boxCode {
	return boxCode{build: path.Join("/work", repo), run: "/run/land", metadata: "http://169.254.169.254/hetzner/v1/metadata"}
}

// runFiles is the shell that names the self-delete's directory, run, and its files, for a script that uses them: auth,
// the Hetzner token as a header curl reads; ssh_keys, the IDs of the SSH keys crabbox registered for the box's lease;
// created, the server's creation, and deadline, when the box deletes itself, both in Unix seconds; spare, there while
// the box is a spare; self_delete, the script a timer runs every minute, and rearm; and needs, setup's (needs).
func (c boxCode) runFiles() string {
	return "run=" + quote(c.run) + `
auth=$run/auth ssh_keys=$run/ssh-keys created=$run/created deadline=$run/deadline spare=$run/spare
self_delete=$run/self-delete rearm=$run/rearm needs=$run/needs
`
}

// The paid-hour rule. Hetzner bills a server's lifetime from its creation rounded up to whole hours, so a box deletes
// itself paidLead before the end of one of its paid hours, at its deadline. A spare's is that of the paid hour it is
// released in (deadline), and a box with less than spareMargin left of it is no spare: a box nobody holds never runs
// into a paid hour it may not be used in. A held box's, which rearm sets when the box is made, taken over and after each
// box command, is the first at least heldFloor from then, so that the SSH connection crabbox keeps open for 10 minutes
// after a command has closed by then: the self-delete would keep a box someone is connected to into its next paid hour
// anyway.
const paidHour, paidLead, heldFloor = time.Hour, 5 * time.Minute, 15 * time.Minute

// deadline is the deadline of the box created at created in the paid hour now is in, which may have passed. rearm
// reckons it the same way, in whole seconds, before it moves on to a held box's.
func deadline(created, now time.Time) time.Time {
	return created.Add(now.Sub(created).Truncate(paidHour) + paidHour - paidLead)
}

// seconds is d in whole seconds, as the box's scripts reckon time.
func seconds(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) }

// script is the box script, which bash runs around every box command CMD, `bash SCRIPT CMD...`, in crabbox's workspace
// for the lease, with env, box.env, set and prepare, the command it runs before CMD, "" for none. In order, it enters
// the build root, and stops unless the workspace links there; sets box.env; turns apt's daily timers off if either is
// on, which a box from an image that has them on would; makes the build root a git repository of the box's own if it
// isn't one; removes each file Git doesn't ignore that crabbox's sync manifest doesn't list; gives each file back the
// time the box last gave it; runs prepare; runs CMD; records the tree CMD left and its files' times; and rearms the
// box. It names each stage on stderr as it starts it, and says if it stops before CMD (stages).
//
// A step that guards what CMD runs on stops the box command when it fails: the build root check, box.env, the apt
// timers (whose apt-daily would take dpkg's lock from prepare), the lock, git init, the prune, the record before CMD
// and prepare. Giving times back only keeps builds incremental, so a failure there warns and CMD still runs; a file it
// couldn't touch keeps the copy's time, as it would without land. The record after CMD, likewise, only warns.
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
// Each record also makes the tree it records the box repository's HEAD, so that a repository's command that reads the
// commit (git rev-parse HEAD, git log -1) finds one, and git status finds the tree clean. The commit has
// no parent and land's own identity, and is made without hooks or signing; a tree HEAD already holds keeps its commit,
// so a re-sync of the same tree, and a box readied from a snapshot of it, keep the same HEAD.
//
// Box commands may run side by side; under a lock, one at a time readies the build root and records it. The
// self-delete's files are named only after CMD, so that box.env can't rename them.
// shortcut: a CMD killed after changing tracked files leaves them unrecorded, so a build may keep artifacts built from
// them once the copy restores the old content; record the tree from a trap if that ever bites.
func (c boxCode) script(env map[string]string, prepare string) string {
	var exports strings.Builder
	for _, name := range slices.Sorted(maps.Keys(env)) {
		exports.WriteString("export " + quote(name+"="+env[name]) + "\n")
	}
	if prepare != "" {
		prepare = "printf 'land: %s\\n' prepare >&2\n" +
			stage.Bash + " " + quote(prepare) + " land 9>&- >&2\n"
	}
	return `set -eo pipefail
printf 'land: %s\n' script >&2
trap 'printf "land: %s\n" stopped >&2' EXIT
cd -P .
if [ "$PWD" != ` + quote(c.build) + ` ]; then
  echo "land: the command runs in $PWD, not in ` + c.build + `: this lease's workspace isn't linked to the build root" >&2
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
  git add -A && tree=$(git write-tree) &&
    { [ "$(git rev-parse -q --verify 'HEAD^{tree}')" = "$tree" ] ||
      git update-ref HEAD "$(git -c user.name=land -c user.email=land@localhost -c commit.gpgsign=false commit-tree -m land "$tree")"; } &&
    { printf '%s\0' "$tree" && git ls-files -z |
      perl -MTime::HiRes=lstat -0lne 'my @s = lstat $_ or next; -l $_ or printf "%s\0%.9f\0", $_, $s[9]'; } \
    >.git/land-times.new && mv .git/land-times.new .git/land-times
}
restore() {
  if [ -f .git/land-times ] && IFS= read -r -d '' recorded <.git/land-times &&
    git diff -z --name-only --no-renames "$recorded^{tree}" "$(git write-tree)" >.git/land-moved 2>/dev/null; then
    perl -MTime::HiRes=utime -0lne '$. > 1 or next; chomp(my $time = <>); utime $time, $time, $_' .git/land-times
  else
    : >.git/land-moved
  fi && { cat .git/land-moved && git diff -z --name-only --no-renames && git ls-files -z --others --exclude-standard; } |
    perl -0 -ne 'chomp; print "$_\0" if -e $_' | xargs -0r touch -c --
}
failed=$(restore 2>&1) ||
  printf 'land: could not restore file times: %s; the build may redo work\n' "${failed:-exit status $?}" >&2
record
` + prepare + `exec 9>&-
trap - EXIT
printf 'land: %s\n' command >&2
code=0
"$@" || code=$?
exec 9>>"${TMPDIR:-/tmp}/land.lock"
flock 9
record || echo "land: the box couldn't record its tree, so its next command may rebuild what it needn't" >&2
` + c.runFiles() + `sudo -n bash "$rearm" 2>/dev/null || true
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

// selfDelete is what a box's self-delete is made of: the Hetzner project's token, which deletes the box; its server's
// creation, from which its paid hours count; and the IDs of the SSH keys crabbox registered for its lease.
type selfDelete struct {
	token   string
	created time.Time
	keys    []int64
}

// setup is the script that readies a new box, and what it reads on stdin: first it checks that the box's image has all
// land's box code needs (needs), failing with all it lacks at once; then crabbox's workspace for lease, workspace,
// becomes a link to the build root, so that every box builds at the same path whatever its lease; the box drops the
// SSH keys of other crabbox leases, which a box from a snapshot of a spare inherits; and it gets its self-delete, s.
func (c boxCode) setup(lease, workspace string, s selfDelete) (script, stdin string) {
	install, stdin := c.install(s)
	return "set -eu\n" + c.runFiles() + c.needs() + `
sudo install -d -o "$(id -un)" -g "$(id -gn)" ` + quote(c.build) + `
mkdir -p ` + quote(path.Dir(workspace)) + `
ln -s ` + quote(c.build) + ` ` + quote(workspace) + `
for keys in ~/.ssh/authorized_keys /root/.ssh/authorized_keys; do
  if sudo test -f "$keys"; then sudo sed -i -e '/ crabbox ` + lease + `$/b' -e '/ crabbox cbx_[0-9a-f]\{12\}$/d' "$keys"; fi
done
` + install, stdin
}

// tools and checkedTools are the commands land's box code calls, but bash's builtins and coreutils, which every image
// has: needs checks that a box's image has each of tools, and more than that of each of checkedTools.
var (
	tools        = []string{"/bin/bash", "git", "flock", "xargs", "sed", "grep", "ss", "curl", "tar", "systemctl", "cloud-init"}
	checkedTools = []string{"perl", "systemd-run", "sudo"}
)

// needs is the shell that stops a new box, printing a line on stdout, where nothing else of setup's script prints, for
// each thing its image lacks that land's box code uses: each of tools, of which /bin/bash runs every script of
// land's, git, flock and xargs the box script, sed setup, grep, ss and curl the self-delete, tar fetch, systemctl the
// self-delete's timer, the warm-up and apt's timers, and cloud-init a snapshot's build; and of checkedTools, perl with
// Time::HiRes (the box script), systemd-run with systemd 254's --expand-environment (the self-delete's timer, the
// warm-up) and sudo without a password, which every root step takes; a self-delete's directory whose scripts bash runs
// as root, wherever /run forbids executing them; and Hetzner's metadata service, which names the server the
// self-delete deletes.
func (c boxCode) needs() string {
	return `lacks=$(
  for cmd in ` + strings.Join(tools, " ") + `; do command -v $cmd >/dev/null || echo $cmd; done
  perl -MTime::HiRes -e 1 2>/dev/null || echo "perl with Time::HiRes"
  systemd-run --help 2>/dev/null | grep -q -e --expand-environment || echo "systemd-run with --expand-environment (systemd 254)"
  if ! sudo -n true 2>/dev/null; then echo "sudo without a password"
  elif ! sudo -n bash -c 'mkdir -p "$1" && echo : >"$2" && bash "$2"' land "$run" "$needs" 2>/dev/null; then
    echo "a $run whose scripts bash runs"
  fi
  ! command -v curl >/dev/null || curl -fsS --max-time 10 -o /dev/null ` + c.metadata + `/instance-id 2>/dev/null ||
    echo "Hetzner's metadata service"
)
if [ -n "$lacks" ]; then echo "$lacks"; exit 1; fi`
}

// install is the shell, after runFiles, that gives the box its self-delete, s, and what it reads on stdin: the Hetzner
// token, which never enters an argument list, and so no process listing. A timer runs selfDeleteScript every minute;
// the box starts as a held box, its deadline set by rearm.
func (c boxCode) install(s selfDelete) (script, stdin string) {
	var keys []string
	for _, k := range s.keys {
		keys = append(keys, strconv.FormatInt(k, 10))
	}
	return stopSelfDelete + `
sudo rm -rf "$run"
sudo install -d -m 700 "$run"
sudo bash -c 'umask 077; cat >"$1"' land "$auth"
echo ` + quote(strings.Join(keys, " ")) + ` | sudo tee "$ssh_keys" >/dev/null
echo ` + strconv.FormatInt(s.created.Unix(), 10) + ` | sudo tee "$created" >/dev/null
sudo tee "$self_delete" >/dev/null <<'LAND'
` + c.selfDeleteScript() + `LAND
sudo tee "$rearm" >/dev/null <<'LAND'
` + c.rearmScript() + `LAND
sudo chmod 600 "$self_delete" "$rearm"
sudo bash "$rearm"
sudo systemd-run --quiet --collect --unit land-self-delete --on-calendar '*:*:00' /bin/bash "$self_delete"
`, "Authorization: Bearer " + s.token + "\n"
}

// selfDeleteScript is what a box's timer runs as root every minute: once the box's deadline has passed, it deletes the
// SSH key crabbox registered for its lease and then its own server, which it finds through Hetzner's metadata service.
// A held box someone is connected to (a box command running, a tunnel open) stays into its next paid hour instead: its
// deadline moves an hour on. A spare's deadline never moves; the sweep reads it from the server's label.
func (c boxCode) selfDeleteScript() string {
	return "#!/bin/bash\nset -eu\n" + c.runFiles() + `[ -s "$deadline" ] && [ "$(date +%s)" -ge "$(cat "$deadline")" ] || exit 0
if [ ! -e "$spare" ] && ss -Htnp state established 2>/dev/null | grep -q sshd; then
  echo $(( $(cat "$deadline") + ` + seconds(paidHour) + ` )) >"$deadline"
  exit 0
fi
id=$(curl -fsS --max-time 10 ` + c.metadata + `/instance-id)
for key in $(cat "$ssh_keys"); do
  curl -fsS --max-time 30 -o /dev/null -X DELETE -H @"$auth" ` + hetznerAPI + `/ssh_keys/$key || true
done
curl -fsS --max-time 30 -o /dev/null -X DELETE -H @"$auth" ` + hetznerAPI + `/servers/$id
`
}

// rearmScript is what sets a held box's deadline, as root, when the box is made, taken over, and after each box
// command, by the paid-hour rule; it leaves a spare's deadline alone.
func (c boxCode) rearmScript() string {
	return "#!/bin/bash\nset -eu\n" + c.runFiles() + `[ ! -e "$spare" ] || exit 0
made=$(cat "$created") now=$(date +%s)
due=$(( made + ((now - made) / ` + seconds(paidHour) + ` + 1) * ` + seconds(paidHour) + ` - ` + seconds(paidLead) + ` ))
while [ $due -lt $(( now + ` + seconds(heldFloor) + ` )) ]; do due=$(( due + ` + seconds(paidHour) + ` )); done
echo $due >"$deadline"
`
}

// stopSelfDelete stops a box's self-delete timer and unloads it and its service, so that a new one can take the name.
const stopSelfDelete = `sudo systemctl stop land-self-delete.timer land-self-delete.service 2>/dev/null || true
sudo systemctl reset-failed land-self-delete.timer land-self-delete.service 2>/dev/null || true`

// arm is the script that makes the box a spare until deadline.
func (c boxCode) arm(deadline time.Time) string {
	return c.runFiles() + armAt(strconv.FormatInt(deadline.Unix(), 10))
}

// armAt is the shell, after runFiles, that makes the box a spare until unix, a shell word for a time in Unix seconds:
// two commands, so that under set -e a box that lost its self-delete fails at the first, and without it the second
// fails too.
func armAt(unix string) string {
	return `echo ` + unix + ` | sudo tee "$deadline" >/dev/null
sudo touch "$spare"`
}

// disarm is the script that makes a box the worktree took, which may still be a spare, a held box again, its deadline
// the held one, and stops its warm-up, every process in its unit.
func (c boxCode) disarm() string {
	return c.runFiles() + `sudo rm -f "$spare" && sudo bash "$rearm" && { sudo systemctl stop land-warm 2>/dev/null || true; } &&
! systemctl is-active --quiet land-warm`
}

// warmStart is the script that, run on a box once crabbox has copied the green commit there, makes it a spare until
// its argument, a time in Unix seconds, and starts its warm-up in a systemd unit, as the box's user in a login shell:
// the box script, with env and prepare, around warm, leaving the exit code in .git/land-warm (warmWait). Both are in the
// script crabbox uploads, not its arguments, so crabbox doesn't print them. A claim stops the unit, every process in
// it (disarm). Any step that fails stops it, and with it the release, which then stops the box: one whose self-delete
// a reboot wiped holds no deadline. systemd leaves the command line as it is, for bash to expand its ${…} and $$ in the
// box's environment.
func (c boxCode) warmStart(env map[string]string, prepare, warm string) string {
	return "set -e\n" + c.runFiles() + "box=" + quote(c.script(env, prepare)) + "\nwarm=" + quote(warm) + "\n" + armAt(`"$1"`) + `
cd -P .
sudo systemctl stop land-warm 2>/dev/null || true
rm -f .git/land-warm
sudo systemd-run --quiet --collect --expand-environment=no --unit land-warm --uid "$(id -u)" --gid "$(id -g)" \
  --working-directory "$PWD" \
  bash -lc '` + stage.Bash + ` "$1" land ` + stage.Bash + ` "$2" land; echo $? >.git/land-warm' land "$box" "$warm"`
}

// warmWait is the script that waits for the box's warm-up to end and prints its exit code, nothing for one stopped.
func (c boxCode) warmWait() string {
	return `while systemctl is-active --quiet land-warm; do sleep 10; done
cat ` + quote(c.build) + `/.git/land-warm 2>/dev/null || true`
}

// unarm is the shell that removes the box's self-delete, so that no snapshot carries it, nor the box deletes itself
// meanwhile.
func (c boxCode) unarm() string {
	return stopSelfDelete + "\n" + c.runFiles() + `sudo rm -rf "$run"`
}

// strip is the script that readies a warm spare to be imaged: without its self-delete, which restore gives back, its
// disk synced.
func (c boxCode) strip() string { return c.unarm() + "; sync" }

// restore is the script that gives the spare strip readied its self-delete back, s, its deadline deadline, and what it
// reads on stdin.
func (c boxCode) restore(s selfDelete, deadline time.Time) (script, stdin string) {
	install, stdin := c.install(s)
	return c.runFiles() + install + armAt(strconv.FormatInt(deadline.Unix(), 10)), stdin
}

// prepareImage is the script that readies a box built for a snapshot: no self-delete, nothing a new box shouldn't
// inherit (the builder's lease expiry and authorized keys), and cloud-init to run again on each new box's first boot,
// with its own key and SSH host keys.
func (c boxCode) prepareImage() string {
	return c.unarm() + `
sudo rm -f /etc/crabbox/lease-expiry /root/.ssh/authorized_keys
rm -f ~/.ssh/authorized_keys
sudo cloud-init clean --logs
sync`
}

// pack is the script that writes rel, a path under the build root, to stdout in a tar archive, as name.
func (c boxCode) pack(rel string) (script, name string) {
	remote := path.Join(c.build, rel)
	return "tar -C " + quote(path.Dir(remote)) + " -cf - " + quote(path.Base(remote)), path.Base(remote)
}
