package boxes

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testBox is a box of the box code's own tests, on this machine, all of it in a directory of its own: its build root
// and self-delete (code), crabbox's workspace for its lease, linked to the build root by setup, its home, its
// temporary files, and the stand-ins' systemd units and apt timers; with a metadata service of its own, which names its
// server. Its scripts run with env, whose PATH, bin, holds only bash, coreutils and the commands the box code needs
// (onPath), and bash adds each command it finds nowhere to missing.
type testBox struct {
	code                                boxCode
	lease, workspace, home, script, bin string
	env                                 []string
	missing                             string
}

func newTestBox(t *testing.T, server int64) *testBox {
	t.Helper()
	dir := realDir(t)
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instance-id" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, server)
	}))
	t.Cleanup(metadata.Close)
	lease := fmt.Sprintf("cbx_%012x", server)
	b := &testBox{
		code:  boxCode{build: filepath.Join(dir, "work", "acme.api"), run: filepath.Join(dir, "run", "land"), metadata: metadata.URL},
		lease: lease, workspace: filepath.Join(dir, "work", "crabbox", lease, "api"), home: filepath.Join(dir, "home"),
		script: filepath.Join(dir, "script"), bin: onPath(t, dir), missing: filepath.Join(dir, "missing"),
	}
	for _, sub := range []string{"home", "tmp", "units"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "timers"), "off\n")
	b.env = []string{"PATH=" + b.bin, "HOME=" + b.home, "TMPDIR=" + filepath.Join(dir, "tmp"),
		"UNITS=" + filepath.Join(dir, "units"), "TIMERS=" + filepath.Join(dir, "timers"), "CALLS=" + filepath.Join(dir, "calls"),
		"MISSING=" + b.missing,
		`BASH_FUNC_command_not_found_handle%%=() { printf '%s\n' "$1" >>"$MISSING"; echo "$1: command not found" >&2; return 127; }`}
	return b
}

// coreutils are GNU coreutils' commands, which every image has.
const coreutils = `[ b2sum base32 base64 basename basenc cat chcon chgrp chmod chown chroot cksum comm cp csplit cut date dd
df dir dircolors dirname du echo env expand expr factor false fmt fold groups head hostid id install join kill link ln
logname ls md5sum mkdir mkfifo mknod mktemp mv nice nl nohup nproc numfmt od paste pathchk pinky pr printenv printf ptx pwd
readlink realpath rm rmdir runcon seq sha1sum sha224sum sha256sum sha384sum sha512sum shred shuf sleep sort split stat
stdbuf stty sum sync tac tail tee test timeout touch tr true truncate tsort tty uname unexpand uniq unlink users vdir wc who
whoami yes`

// onPath makes dir's bin/, a box's PATH, and returns it: bash and coreutils, as this machine has them, and each command
// the box code needs, tools and checkedTools: this machine's own, or a stand-in, run by bash, for one a Mac lacks or
// mustn't run. sudo's runs its command as bash does, so that a command bash finds nowhere is missing there too.
func onPath(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	stand := maps.Clone(standIns)
	stand["sudo"] = "[ \"$1\" != -n ] || shift\n\"$@\""
	for _, name := range slices.Concat([]string{"bash"}, strings.Fields(coreutils), tools, checkedTools) {
		if path.IsAbs(name) {
			continue // needs checks it by its path
		}
		file := filepath.Join(bin, name)
		if script, ok := stand[name]; ok {
			if err := os.WriteFile(file, []byte("#!"+bash+"\n"+recordCall+script+"\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in
				t.Fatal(err)
			}
			continue
		}
		real, err := exec.LookPath(name)
		switch {
		case err == nil:
			if err := os.Symlink(real, file); err != nil {
				t.Fatal(err)
			}
		case slices.Contains(tools, name) || slices.Contains(checkedTools, name):
			t.Fatalf("this machine has no %s, which the box code needs", name)
		}
	}
	return bin
}

// bash runs `bash ARGS...` on the box, in dir with stdin, and returns what it printed; it fails the test unless bash
// exits 0 having found every command it ran.
func (b *testBox) bash(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(b.bin, "bash"), args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, b.env, strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash %.200q: %v\n%s", args, err, stderr.String())
	}
	if missing, err := os.ReadFile(b.missing); err == nil {
		t.Fatalf("bash %.200q called %q, which needs doesn't check a box's image for\n%s", args, missing, stderr.String())
	}
	return string(out)
}

// ssh runs script as crabbox's SSH command does, with stdin, and returns what it printed.
func (b *testBox) ssh(t *testing.T, script, stdin string) string {
	t.Helper()
	return b.bash(t, b.home, stdin, "-c", script)
}

// run runs script as `crabbox run --script-stdin -- ARGS...` does, in the lease's workspace.
func (b *testBox) run(t *testing.T, script string, args ...string) {
	t.Helper()
	write(t, b.script, script)
	b.bash(t, b.workspace, "", append([]string{b.script}, args...)...)
}

// tick runs the box's self-delete, as its timer does every minute.
func (b *testBox) tick(t *testing.T) {
	t.Helper()
	b.bash(t, b.home, "", filepath.Join(b.code.run, "self-delete"))
}

// Every script of land's box code runs with only bash, coreutils and the commands needs checks a box's image for on
// PATH, and calls nothing else, which an image could lack without setup refusing it: a box goes through them all,
// setup, a box command, a release as a spare and a takeover, a warm-up and the wait for it, a fetch, a refresh's strip
// and restore of the self-delete, which then deletes the box, and the readying of a snapshot's box.
func TestTheBoxCodeCallsOnlyWhatItNeeds(t *testing.T) {
	f := newFake(t)
	f.write(t, "servers", `{"id":42,"name":"crabbox-box","status":"running","labels":{"crabbox":"true"}}`+"\n")
	f.write(t, "keys", `{"id":7,"name":"crabbox-cbx-00000000002a"}`+"\n")
	b := newTestBox(t, 42)
	s := selfDelete{token: testToken, created: time.Now().Add(-50 * time.Minute), keys: []int64{7}}
	env, hour := map[string]string{"GREETING": "it's here"}, time.Now().Add(time.Hour)
	script, stdin := b.code.setup(b.lease, b.workspace, s)
	if lacks := b.ssh(t, script, stdin); lacks != "" {
		t.Fatalf("setup found the box lacking %q", lacks)
	}
	b.run(t, b.code.script(env, "true"), "true")
	b.bash(t, b.home, "", "-c", b.code.arm(hour))
	b.bash(t, b.home, "", "-c", b.code.disarm())
	b.run(t, b.code.warmStart(env, "true", "true"), strconv.FormatInt(hour.Unix(), 10))
	if code := b.ssh(t, b.code.warmWait(), ""); code != "0\n" {
		t.Fatalf("the warm-up's exit code is %q, want 0", code)
	}
	pack, _ := b.code.pack(".git/land-warm")
	b.ssh(t, pack, "")
	b.ssh(t, b.code.strip(), "")
	script, stdin = b.code.restore(s, time.Now().Add(-time.Second))
	b.ssh(t, script, stdin)
	b.tick(t)
	b.ssh(t, b.code.prepareImage(), "")
	if changes, _ := f.api.changed(); !slices.Equal(changes, []string{"DELETE /ssh_keys/7", "DELETE /servers/42"}) {
		t.Fatalf("the self-delete called %q, want the box's key and server deleted", changes)
	}
}

// Two boxes, each in a directory of its own, share nothing: setting one up, arming it as a spare past its deadline,
// then setting the other up and running a command there, leaves the first armed, so that its self-delete deletes its
// server, which its own metadata service names, and its key, and the other's none; and each box's build root holds
// only what its own command made.
func TestTwoBoxesShareNothing(t *testing.T) {
	f := newFake(t)
	f.write(t, "servers", `{"id":41,"name":"crabbox-a","status":"running","labels":{"crabbox":"true"}}`+"\n"+
		`{"id":42,"name":"crabbox-b","status":"running","labels":{"crabbox":"true"}}`+"\n")
	f.write(t, "keys", `{"id":7,"name":"crabbox-cbx-000000000029"}`+"\n"+`{"id":8,"name":"crabbox-cbx-00000000002a"}`+"\n")
	a, b := newTestBox(t, 41), newTestBox(t, 42)
	created := time.Now().Add(-50 * time.Minute)
	for _, box := range []*testBox{a, b} {
		key := int64(7)
		if box == b {
			key = 8
		}
		script, stdin := box.code.setup(box.lease, box.workspace, selfDelete{token: testToken, created: created, keys: []int64{key}})
		box.ssh(t, script, stdin)
		box.run(t, box.code.script(nil, ""), "touch", "made-by-"+box.lease)
		if box == a {
			box.bash(t, box.home, "", "-c", box.code.arm(time.Now().Add(-time.Second)))
		}
	}
	a.tick(t)
	b.tick(t)
	if changes, _ := f.api.changed(); !slices.Equal(changes, []string{"DELETE /ssh_keys/7", "DELETE /servers/41"}) {
		t.Fatalf("the boxes' self-deletes called %q, want the spare's key and server deleted, and nothing of the other's", changes)
	}
	if _, err := os.Stat(filepath.Join(b.code.run, "spare")); err == nil {
		t.Fatal("the held box is a spare")
	}
	for _, box := range []*testBox{a, b} {
		entries, err := os.ReadDir(box.code.build)
		if err != nil {
			t.Fatal(err)
		}
		var made []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "made-by-") {
				made = append(made, e.Name())
			}
		}
		if !slices.Equal(made, []string{"made-by-" + box.lease}) {
			t.Errorf("%s's build root holds %q, want only what its own command made", box.lease, made)
		}
	}
}
