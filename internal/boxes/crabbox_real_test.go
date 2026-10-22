//go:build crabboxreal

package boxes

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // known_hosts hashes host names with HMAC-SHA1
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytes032/land/internal/proc"
)

// The real crabbox ($LAND_CRABBOX, else crabbox on PATH) keeps a box in the box's folder: run with the folder's state/
// as its state directory (XDG_STATE_HOME) and its tmp/ as its temporary one (TMPDIR), land's calls on a box, warmup,
// run with and without a script on stdin, and ssh, and land's own use of the SSH command ssh prints, fetching a file
// the box made with tar, write nothing outside the folder but what outside allows.
//
// HOME is the test's directory, which holds the folder and the repository, and whose walks judge every path in it.
// Each call runs in a macOS sandbox that denies it any write elsewhere but in /dev, in crabbox's SSH control
// directories in /tmp, which the walks list, and in the known hosts file of the passwd home, where ssh writes whatever
// HOME is: those lines the test reads for the box's. So what other processes write in /tmp counts for nothing, and a
// write anywhere else fails the call, or shows in what it printed. The box's folder is as long as a real one on
// macOS, so that a socket crabbox put in it would hit the 104-byte limit here first. stop, which only releases the
// box, is outside the assertion: the test reports what it writes. crabbox's docker provider stands in for Hetzner:
// free and local, with the same claims, keys, SSH and sync. The test skips when docker isn't reachable, and leaves
// docker as it found it. Run it, one at a time, with
//
//	go test -tags crabboxreal -count=1 -v -run TestCrabboxKeepsABoxInItsFolder ./internal/boxes/
func TestCrabboxKeepsABoxInItsFolder(t *testing.T) {
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-time.Minute)) // a minute left to stop the box
		defer cancel()
	}
	r := newBoxRun(t)
	images := r.images(t)
	t.Cleanup(func() { r.cleanUp(t, images) })
	r.start()

	out := r.crabbox(ctx, t, "warmup", false, "", "warmup", "--slug", "land-"+randomHex(6), "--type", "cpx42",
		"--ttl", leaseTTL, "--idle-timeout", leaseTTL)
	leased := regexp.MustCompile(`(?m)^leased (cbx_[0-9a-f]{12}) .*\bserver=(\S+)`).FindStringSubmatch(out)
	endpoint := regexp.MustCompile(`(?m)^ready .*\bssh=(?:\S+@)?(\S+):(\d+)\b`).FindStringSubmatch(out)
	if leased == nil || endpoint == nil || !readyLine.MatchString(out) {
		t.Fatalf("crabbox warmup printed no leased or no ready line:\n%s", out)
	}
	r.lease, r.sshHost = leased[1], "["+endpoint[1]+"]:"+endpoint[2]
	image, err := r.docker(ctx, "inspect", "--format", "{{.Image}}", leased[2])
	if err != nil {
		t.Fatalf("docker inspect %s: %v: %s", leased[2], err, image)
	}
	r.image = image

	out = r.crabbox(ctx, t, "run --script-stdin", false, "set -eo pipefail\n"+`printf 'made on the box\n' >made-on-the-box
printf 'workspace=%s\n' "$PWD"
"$@"
`, "run", "--id", r.lease, "--reclaim", "--script-stdin", "--", "true")
	workspace := regexp.MustCompile(`(?m)^workspace=(/\S+)$`).FindStringSubmatch(out)
	if workspace == nil {
		t.Fatalf("the box script printed no workspace:\n%s", out)
	}
	if out := r.crabbox(ctx, t, "run", false, "", "run", "--id", r.lease, "--reclaim", "--", "sh", "-c", "echo hi"); !strings.Contains(out, "hi\n") {
		t.Fatalf("crabbox run printed %q; want hi", out)
	}
	out = strings.TrimSpace(r.crabbox(ctx, t, "ssh", false, "", "ssh", "--id", r.lease))
	ssh := out[strings.LastIndexByte(out, '\n')+1:] // the command is crabbox's last line
	r.checkSSH(t, ssh)
	r.call(ctx, t, "tar over the ssh command", false, "", "bash", "-c", `set -o pipefail; `+ssh+` "$1" | tar -xf - -C "$2"`,
		"land", "tar -C "+quote(workspace[1])+" -cf - made-on-the-box", filepath.Dir(r.fetched))
	if data, err := os.ReadFile(r.fetched); err != nil || string(data) != "made on the box\n" {
		t.Fatalf("tar over the SSH command crabbox printed fetched %q, %v", data, err)
	}

	r.crabbox(ctx, t, "stop", true, "", "stop", "--id", r.lease)
	r.stopped = true
	if left := r.containers(ctx, t); len(left) > 0 {
		t.Errorf("crabbox stop left the box's containers %q", left)
	}
	r.report(t)
}

// Outside the box's folder, land's calls may make crabbox write only these, which the folder's deletion leaves:
//   - its SSH control directories in /tmp, whose socket paths must stay short, as crabbox documents: the lease's
//     ControlPersist master's, c and 16 characters, and each run's workspace owner's, cbx-owner- and digits;
//   - the file land fetched, in the repository's .land/;
//   - with the docker provider only, ~/Library/Caches/crabbox/local-container-bootstrap/, a container's bootstrap
//     script, mounted into it: a Hetzner box has none.
//
// A new directory holding nothing else is allowed with them.
const (
	inFolder   = "folder"
	control    = "/tmp/c*"
	fetched    = "land's fetch"
	dockerOnly = "docker only"
	holder     = "holds the allowed"
)

// where is what outside allows p as, "" for nothing.
func (r *boxRun) where(p string) string {
	switch {
	case under(r.box, p):
		return inFolder
	case under(r.tmp, p):
		return control // the sandbox lets the calls write nowhere else in /tmp
	case p == r.fetched:
		return fetched
	case under(r.bootstrap, p):
		return dockerOnly
	}
	return ""
}

// controlDirs are the names of crabbox's SSH control directories in /tmp, as regular expressions that both Go and
// sandbox profiles, which can't count repetitions, read.
var controlDirs = []string{"c" + strings.Repeat("[-_A-Za-z0-9]", 16), "cbx-owner-[0-9]+"}

var controlDir = regexp.MustCompile("^(" + strings.Join(controlDirs, "|") + ")$")

// boxRun is one box's life in the real crabbox: the box's folder, the fresh HOME and the scratch repository crabbox
// runs with, and what each call wrote.
type boxRun struct {
	bin, sandboxExec string // crabbox and sandbox-exec
	dockerHost       string // docker's endpoint
	home             string // the test's directory, HOME, which holds the box's folder and the repository
	box, repo        string
	tmp              string // /tmp, by a path without links
	knownHosts       string // the passwd home's ~/.ssh/known_hosts, ssh's whatever HOME is
	fetched          string // where land's tar puts the file a box command made: in the repository's .land/
	bootstrap        string // the docker provider's bootstrap root
	profile          string // the sandbox
	env              []string
	before, last     tree               // every path before the first call, and after the last
	landEnd          tree               // every path after land's last call
	known            map[string]bool    // knownHosts' lines before the first call
	calls            []*call            // in order
	changes          map[string]*change // by path
	log              bytes.Buffer       // every call and what it printed
	lease, image     string             // the box's lease, and its container's image
	sshHost          string             // the box's SSH endpoint, as known_hosts names it
	control          string             // the lease's SSH control directory in /tmp, from crabbox ssh
	ours             map[string]bool    // the control directories in /tmp the box's processes named, by name
	stopped          bool
}

// tree is every path a walk found.
type tree map[string]entry

// entry is a path's type and, for its comparison, its size and modification time.
type entry struct {
	mode  fs.FileMode
	size  int64
	mtime int64
}

// call is one call in the box's life, land's or, cleanup, only to release the box; the paths new, or files changed,
// since before the first call, that appeared by its end, those such it removed, and the control directories that
// appeared in /tmp only while it ran; the lines that appeared in knownHosts by its end; and those of what it printed
// that tell of a write the sandbox denied.
type call struct {
	name                     string
	cleanup                  bool
	created, removed, during []string
	knownLines, denied       []string
}

// change is a path created, or modified, a file there before the first call, and the calls after which it was there,
// or "during" one, for a control directory a call made and removed; cleanup if a cleanup call made it.
type change struct {
	dir, modified, cleanup bool
	when                   []string
}

// newBoxRun makes HOME, the box's folder in it, as land lays it out on macOS, with its state/ and tmp/, and a
// repository of one commit, whose origin is acme/example-repository and which ignores crabbox's .crabbox/, as land
// has it, with a .land/ for what land fetches.
func newBoxRun(t *testing.T) *boxRun {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the test keeps crabbox's writes to its own with macOS's sandbox")
	}
	bin, err := exec.LookPath(cmp.Or(os.Getenv("LAND_CRABBOX"), "crabbox"))
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		t.Fatalf("no crabbox: %v; set LAND_CRABBOX to its path, or put it on PATH", err)
	}
	sandboxExec, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	host := dockerHost(t)
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := realDir(t)
	r := &boxRun{bin: bin, sandboxExec: sandboxExec, dockerHost: host, home: home, tmp: tmp, changes: map[string]*change{},
		ours:       map[string]bool{},
		box:        filepath.Join(home, "Library", "Application Support", "land", "acme.example-repository", "boxes", "0123456789ab"),
		repo:       filepath.Join(home, "repo"),
		knownHosts: filepath.Join(u.HomeDir, ".ssh", "known_hosts"),
		bootstrap:  filepath.Join(home, "Library", "Caches", "crabbox", "local-container-bootstrap")}
	r.fetched = filepath.Join(r.repo, ".land", "made-on-the-box")
	r.profile = r.sandbox()
	r.env = boxEnv(home, r.box, host)
	for _, dir := range []string{filepath.Join(r.box, "state"), filepath.Join(r.box, "tmp"), filepath.Dir(r.fetched)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.git(t, "init", "-q")
	r.git(t, "remote", "add", "origin", "https://github.com/acme/example-repository.git")
	write(t, filepath.Join(r.repo, "README"), "a scratch repository\n")
	r.git(t, "add", "README")
	r.git(t, "-c", "commit.gpgsign=false", "commit", "-qm", "init")
	exclude := filepath.Join(r.repo, ".git", "info", "exclude")
	data, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	write(t, exclude, string(data)+"/.crabbox/\n")
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, version)
	}
	t.Logf("crabbox %s (%s), docker at %s, the box's folder (%d bytes) %s", bytes.TrimSpace(version), bin, host, len(r.box), r.box)
	return r
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// dockerHost is the endpoint of the user's docker, DOCKER_HOST or the current context's, for the docker crabbox runs,
// whose HOME has no contexts; the test skips unless docker answers there.
func dockerHost(t *testing.T) string {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Skipf("docker isn't reachable: no current docker context: %v", err)
		}
		host = strings.TrimSpace(string(out))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "--host", host, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("docker isn't reachable at %s: %v: %s", host, err, bytes.TrimSpace(out))
	}
	return host
}

// boxEnv is crabbox's environment for the box: the test's, without what reaches the user's crabbox, docker, git or
// XDG directories, with HOME home, docker's endpoint, the box's state/ and tmp/ as crabbox's state and temporary
// directories, and land's crabbox settings, on the docker provider.
func boxEnv(home, box, dockerHost string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "HOME" || name == "TMPDIR" || slices.ContainsFunc([]string{"CRABBOX_", "DOCKER_", "GIT_", "XDG_"},
			func(prefix string) bool { return strings.HasPrefix(name, prefix) })
	})
	return append(env, "HOME="+home, "DOCKER_HOST="+dockerHost, "XDG_STATE_HOME="+filepath.Join(box, "state"),
		"TMPDIR="+filepath.Join(box, "tmp"), "CRABBOX_PROVIDER=docker", "CRABBOX_MAX_TTL="+leaseTTL,
		"CRABBOX_SYNC_GIT_SEED=false")
}

// sandbox is the profile every call runs in: it denies any write but in the test's directory, in /dev, in crabbox's
// control directories in /tmp and in knownHosts, with the file ssh replaces it with.
func (r *boxRun) sandbox() string {
	var controls strings.Builder
	for _, name := range controlDirs {
		fmt.Fprintf(&controls, "\n    (require-not (regex #\"^%s/%s(/|$)\"))", r.tmp, name)
	}
	return fmt.Sprintf(`(version 1)
(allow default)
(deny file-write*
  (require-all
    (require-not (subpath %s))
    (require-not (subpath "/dev"))
    (require-not (regex #"^%s"))%s))
`, strconv.Quote(r.home), regexp.QuoteMeta(r.knownHosts), &controls)
}

func (r *boxRun) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.repo}, args...)...)
	cmd.Env = append(slices.Clone(r.env), "GIT_AUTHOR_NAME=land", "GIT_AUTHOR_EMAIL=land@example.invalid",
		"GIT_COMMITTER_NAME=land", "GIT_COMMITTER_EMAIL=land@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// start notes every path, and knownHosts' lines, before the first call.
func (r *boxRun) start() {
	r.before = r.walk()
	r.last = r.before
	r.known = map[string]bool{}
	for _, line := range r.knownHostsLines() {
		r.known[line] = true
	}
}

// spec is argv in the sandbox, run in the repository as land runs crabbox, with crabbox's environment, and stdin
// unless it is "".
func (r *boxRun) spec(argv []string, stdin string, stdout, stderr io.Writer) proc.Spec {
	s := proc.Spec{Argv: append([]string{r.sandboxExec, "-p", r.profile}, argv...), Dir: r.repo, Env: r.env,
		Stdout: stdout, Stderr: stderr, Grace: time.Minute}
	if stdin != "" {
		s.Stdin = strings.NewReader(stdin)
	}
	return s
}

// crabbox is the call name of crabbox with args.
func (r *boxRun) crabbox(ctx context.Context, t *testing.T, name string, cleanup bool, stdin string, args ...string) string {
	t.Helper()
	return r.call(ctx, t, name, cleanup, stdin, append([]string{r.bin}, args...)...)
}

var (
	// tooLong is how a path over a limit, such as a socket's 104 bytes on macOS, shows in what a call printed.
	tooLong = regexp.MustCompile(`(?i)too long|ENAMETOOLONG|sun_path`)
	// deniedLine is a line of what a call printed that tells of a write the sandbox denied.
	deniedLine = regexp.MustCompile(`(?im)^.*(operation not permitted|failed to add the host).*$`)
)

// call is the call name: argv with stdin, watching /tmp meanwhile, then the walk that records what it changed. It
// returns what argv printed on stdout, and fails the test unless argv exits 0.
func (r *boxRun) call(ctx context.Context, t *testing.T, name string, cleanup bool, stdin string, argv ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	stop := r.watch()
	err := proc.Run(ctx, r.spec(argv, stdin, &stdout, &stderr))
	during := stop()
	fmt.Fprintf(&r.log, "$ %s\n%s%s", strings.Join(argv, " "), &stdout, &stderr)
	printed := stdout.String() + stderr.String()
	if err != nil {
		var hint string
		switch {
		case tooLong.MatchString(printed):
			hint = fmt.Sprintf("\na path-length problem? the box's folder is %d bytes", len(r.box))
		case deniedLine.MatchString(printed):
			hint = "\nthe sandbox denied a write outside the box's folder, " + r.box
		}
		t.Fatalf("%s: %v%s\n%s", strings.Join(argv, " "), err, hint, printed)
	}
	r.record(&call{name: name, cleanup: cleanup, denied: deniedLine.FindAllString(printed, -1)}, during)
	return stdout.String()
}

// watch notes each control directory that appears in /tmp, which the walk after the call before didn't find, until
// stop, which returns their names, each true once a process naming the box's folder too, the box's ssh, named it:
// another crabbox may make its own meanwhile.
func (r *boxRun) watch() (stop func() map[string]bool) {
	seen := map[string]bool{}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		var listed time.Time
		for {
			entries, _ := os.ReadDir(r.tmp)
			for _, e := range entries {
				_, old := r.last[filepath.Join(r.tmp, e.Name())]
				if _, known := seen[e.Name()]; !old && !known && controlDir.MatchString(e.Name()) {
					seen[e.Name()] = false
				}
			}
			if slices.Contains(slices.Collect(maps.Values(seen)), false) && time.Since(listed) > 50*time.Millisecond {
				listed = time.Now()
				out, _ := exec.Command("ps", "-A", "-ww", "-o", "command=").Output()
				for line := range strings.Lines(string(out)) {
					for name := range seen {
						seen[name] = seen[name] || strings.Contains(line, "/tmp/"+name+"/") && strings.Contains(line, r.box)
					}
				}
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() map[string]bool {
		close(done)
		<-finished
		return seen
	}
}

// walk finds every path in HOME, but the repository's objects, and in crabbox's control directories in /tmp. Entries
// it can't read, or that vanish meanwhile, are other processes'.
func (r *boxRun) walk() tree {
	found := tree{}
	for _, top := range []string{r.home, r.tmp} {
		_ = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil
			case p == filepath.Join(r.repo, ".git", "objects"), top == r.tmp && filepath.Dir(p) == r.tmp && !controlDir.MatchString(d.Name()):
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if info, err := d.Info(); err == nil {
				found[p] = entry{mode: info.Mode().Type(), size: info.Size(), mtime: info.ModTime().UnixNano()}
			}
			return nil
		})
	}
	return found
}

// changed is whether p, found as e, is new since before the first call or, but in /tmp, where other processes write
// too, a file whose type, size or time changed.
func (r *boxRun) changed(p string, e entry) bool {
	was, old := r.before[p]
	return !old || !under(r.tmp, p) && !(e.mode.IsDir() && was.mode.IsDir()) && e != was
}

// record walks after c and notes what c changed, the control directories that appeared in /tmp only during it
// (during), and the lines that appeared in knownHosts.
func (r *boxRun) record(c *call, during map[string]bool) {
	now := r.walk()
	for p, e := range now {
		if !r.changed(p, e) {
			continue
		}
		r.note(p, e.mode.IsDir(), c, c.name)
		if prev, ok := r.last[p]; !ok || !e.mode.IsDir() && prev != e {
			c.created = append(c.created, p)
		}
	}
	for p, e := range r.last {
		if _, still := now[p]; !still && r.changed(p, e) {
			c.removed = append(c.removed, p)
		}
	}
	for name, ours := range during {
		r.ours[name] = r.ours[name] || ours
		p := filepath.Join(r.tmp, name)
		if _, after := now[p]; !after {
			r.note(p, true, c, "during "+c.name)
			c.during = append(c.during, p)
		}
	}
	for _, line := range r.knownHostsLines() {
		if !r.known[line] {
			c.knownLines = append(c.knownLines, line)
		}
	}
	r.last = now
	if !c.cleanup {
		r.landEnd = now
	}
	r.calls = append(r.calls, c)
}

// note notes that p, a directory or not, was there after c, or during it: when.
func (r *boxRun) note(p string, dir bool, c *call, when string) {
	ch := r.changes[p]
	if ch == nil {
		_, old := r.before[p]
		ch = &change{dir: dir, modified: old, cleanup: c.cleanup}
		r.changes[p] = ch
	}
	ch.when = append(ch.when, when)
}

// knownHostsLines are knownHosts' lines, none if it has none.
func (r *boxRun) knownHostsLines() []string {
	data, _ := os.ReadFile(r.knownHosts)
	return strings.Split(string(data), "\n")
}

// knows is whether the known_hosts line names host, as itself or hashed.
func knows(line, host string) bool {
	names, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	if hashed, ok := strings.CutPrefix(names, "|1|"); ok {
		salt, sum, _ := strings.Cut(hashed, "|")
		key, err1 := base64.StdEncoding.DecodeString(salt)
		want, err2 := base64.StdEncoding.DecodeString(sum)
		mac := hmac.New(sha1.New, key)
		mac.Write([]byte(host))
		return err1 == nil && err2 == nil && hmac.Equal(mac.Sum(nil), want)
	}
	return slices.Contains(strings.Split(names, ","), host)
}

// checkSSH checks the SSH command crabbox printed, which land runs itself: ssh reads the lease's key and writes its
// known hosts in the box's folder, and its control socket in a control directory in /tmp, if any.
func (r *boxRun) checkSSH(t *testing.T, ssh string) {
	t.Helper()
	out, err := exec.Command("bash", "-c", `printf '%s\0' `+ssh).Output()
	if err != nil {
		t.Fatalf("bash can't split crabbox's SSH command %q: %v", ssh, err)
	}
	words := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var key, known, socket string
	for i := 1; i < len(words); i++ {
		option, value, _ := strings.Cut(words[i], "=")
		if unquoted, err := strconv.Unquote(value); err == nil { // ssh's own quoting of a value with spaces
			value = unquoted
		}
		switch {
		case words[i-1] == "-i":
			key = words[i]
		case words[i-1] == "-o" && option == "UserKnownHostsFile":
			known = value
		case words[i-1] == "-o" && option == "ControlPath":
			socket = value
		}
	}
	if !under(r.box, key) || !under(r.box, known) {
		t.Errorf("crabbox ssh printed %q; want its key and known hosts in the box's folder, %s", ssh, r.box)
	}
	if socket != "" && socket != "none" {
		if dir := filepath.Dir(socket); filepath.Dir(dir) != "/tmp" || !controlDir.MatchString(filepath.Base(dir)) {
			t.Errorf("crabbox ssh printed %q; want its control socket in a control directory in /tmp", ssh)
		} else {
			r.control = filepath.Base(dir)
		}
	}
}

// report logs, call by call, what appeared or went outside the box's folder, and the sandbox denied; every path the
// calls wrote, by what outside allows it as; the control directories in /tmp; and what land's last call and stop
// left in the box's folder, which land deletes with it. It fails on what land's calls wrote outside the folder that
// outside doesn't allow.
func (r *boxRun) report(t *testing.T) {
	t.Helper()
	others := map[string]bool{} // the control directories another crabbox made meanwhile
	for p := range r.changes {
		if top := r.topOfTmp(p); under(r.tmp, p) && top != r.control && !r.ours[top] {
			others[top] = true
			delete(r.changes, p)
		}
	}
	paths := slices.Sorted(maps.Keys(r.changes))
	as := map[string]string{}
	for _, p := range paths {
		as[p] = r.where(p)
	}
	for _, p := range paths {
		if c := r.changes[p]; as[p] == "" && c.dir && !c.modified && slices.ContainsFunc(paths, func(q string) bool {
			return q != p && under(p, q) && r.where(q) != ""
		}) {
			as[p] = holder
		}
	}
	var b, outside strings.Builder
	b.WriteString("call by call, what appeared (+) or went (-) outside the box's folder, appeared there only while the call ran (±), or the sandbox denied (✗):\n")
	counted := map[string]bool{}
	for _, c := range r.calls {
		fmt.Fprintf(&b, "  %s%s\n", c.name, map[bool]string{true: ", cleanup only, outside the assertion"}[c.cleanup])
		n := b.Len()
		for _, list := range []struct {
			sign  string
			paths []string
		}{{"+", c.created}, {"-", c.removed}, {"±", c.during}} {
			slices.Sort(list.paths)
			for _, p := range list.paths {
				if ch := r.changes[p]; ch != nil && as[p] != inFolder {
					fmt.Fprintf(&b, "    %s %s  (%s)\n", list.sign, r.show(p, ch.dir, ch.modified), cmp.Or(as[p], "OUTSIDE"))
				}
			}
		}
		var outsideOf []string
		for _, line := range c.knownLines {
			if !counted[line] && knows(line, r.sshHost) {
				counted[line] = true
				outsideOf = append(outsideOf, fmt.Sprintf("the box's host key, %s, in %s", r.sshHost, r.knownHosts))
			}
		}
		for _, line := range c.denied {
			outsideOf = append(outsideOf, "a denied write: "+strings.TrimSpace(line))
		}
		for _, what := range outsideOf {
			fmt.Fprintf(&b, "    ✗ %s  (OUTSIDE)\n", what)
			if !c.cleanup {
				fmt.Fprintf(&outside, "  %s, by %s\n", what, c.name)
			}
		}
		if b.Len() == n {
			b.WriteString("    nothing\n")
		}
	}
	b.WriteString("every path the calls wrote, created or changed (~), and the calls after which it was there, or during which only:\n")
	var controls []string
	for _, p := range paths {
		c := r.changes[p]
		line := fmt.Sprintf("  %-17s %s  %s\n", cmp.Or(as[p], "OUTSIDE"), r.show(p, c.dir, c.modified), strings.Join(c.when, ", "))
		b.WriteString(line)
		if as[p] == "" && !c.cleanup {
			outside.WriteString(line)
		}
		if filepath.Dir(p) == r.tmp {
			controls = append(controls, filepath.Base(p)+": "+strings.Join(c.when, ", "))
		}
	}
	fmt.Fprintf(&b, "the box's %d control directories in /tmp, the lease's, from crabbox ssh, %s, and those the box's ssh named while a call ran:\n  %s\n",
		len(controls), cmp.Or(r.control, "none"), strings.Join(controls, "\n  "))
	fmt.Fprintf(&b, "  and %d that other crabbox processes made meanwhile, which the test ignores\n", len(others))
	for _, end := range []struct {
		name string
		tree tree
	}{{"land's last call", r.landEnd}, {"stop", r.last}} {
		fmt.Fprintf(&b, "the box's folder after %s, which land deletes with it:\n", end.name)
		for _, p := range slices.Sorted(maps.Keys(end.tree)) {
			if p != r.box && under(r.box, p) {
				fmt.Fprintf(&b, "  %s\n", r.show(p, end.tree[p].mode.IsDir(), false))
			}
		}
	}
	t.Log(b.String())
	if outside.Len() > 0 {
		t.Errorf("land's calls made crabbox write outside the box's folder, where nothing deletes it with the box:\n%s", &outside)
	}
}

// show is p by the test's directory it is in, with a slash after a directory and a ~ before a modified file.
func (r *boxRun) show(p string, dir, modified bool) string {
	name := p
	for _, d := range [][2]string{{r.box, "$BOX"}, {r.repo, "$REPO"}, {r.home, "$HOME"}, {r.tmp, "/tmp"}} {
		if under(d[0], p) {
			name = d[1] + strings.TrimPrefix(p, d[0])
			break
		}
	}
	if dir {
		name += "/"
	}
	if modified {
		name = "~" + name
	}
	return name
}

// topOfTmp is the entry directly in /tmp that holds p.
func (r *boxRun) topOfTmp(p string) string {
	rel, _ := filepath.Rel(r.tmp, p)
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	return top
}

// under is whether p is dir or in it.
func under(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// docker runs docker on crabbox's endpoint and returns what it printed.
func (r *boxRun) docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", r.dockerHost}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// images are docker's images, by ID.
func (r *boxRun) images(t *testing.T) map[string]bool {
	t.Helper()
	out, err := r.docker(t.Context(), "image", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		t.Fatalf("docker image ls: %v: %s", err, out)
	}
	ids := map[string]bool{}
	for _, id := range strings.Fields(out) {
		ids[id] = true
	}
	return ids
}

// containers are the IDs of the box's containers, by crabbox's lease label.
func (r *boxRun) containers(ctx context.Context, t *testing.T) []string {
	t.Helper()
	out, err := r.docker(ctx, "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=lease="+r.lease)
	if err != nil {
		t.Errorf("docker ps: %v: %s", err, out)
	}
	return strings.Fields(out)
}

// cleanUp stops the box unless the test did, removes its containers, failing the test for any, and the image docker
// pulled for it, which wasn't there before (images).
func (r *boxRun) cleanUp(t *testing.T, images map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if r.lease == "" {
		m := regexp.MustCompile(`\blease=(cbx_[0-9a-f]{12})\b`).FindStringSubmatch(r.log.String())
		if m == nil {
			return
		}
		r.lease = m[1]
	}
	if !r.stopped {
		var out bytes.Buffer
		if err := proc.Run(ctx, r.spec([]string{r.bin, "stop", "--id", r.lease}, "", &out, &out)); err != nil {
			t.Errorf("crabbox stop --id %s: %v\n%s", r.lease, err, &out)
		}
	}
	for _, id := range r.containers(ctx, t) {
		if image, err := r.docker(ctx, "inspect", "--format", "{{.Image}}", id); err == nil && r.image == "" {
			r.image = image
		}
		out, err := r.docker(ctx, "rm", "--force", id)
		t.Errorf("removed the box's container %.12s, which crabbox left: %v %s", id, err, out)
	}
	if r.image != "" && !images[r.image] {
		if out, err := r.docker(ctx, "image", "rm", r.image); err != nil {
			t.Errorf("removing the image docker pulled for the box, %s: %v: %s", r.image, err, out)
		} else {
			t.Logf("removed the image docker pulled for the box, %s", r.image)
		}
	}
}
