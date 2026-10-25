//go:build boximage

package boxes

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// land's box code runs on each Ubuntu crabbox's `os` names, on real Hetzner boxes: a box from plain Ubuntu, created
// and readied by land's own code (create, setup with its needs check), runs a box command through the box script,
// which rearms its deadline; at a deadline passed, the self-delete keeps the box while something is connected to it,
// and deletes it, server and SSH key, once nothing is. Each box is a cpx22 in the project of HCLOUD_TOKEN, the only
// secret the test reads, for a few minutes; the test deletes all it made, failing or not, and confirms with Hetzner's
// API that nothing is left. Its HOME is its own, so the boxes' state is too. Run it, with crabbox, with
//
//	go test -tags boximage -count=1 -v -timeout 30m -run TestLandsBoxCodeRunsOnUbuntu ./internal/boxes/
func TestLandsBoxCodeRunsOnUbuntu(t *testing.T) {
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Fatal("HCLOUD_TOKEN, the Hetzner project's API token, isn't set")
	}
	t.Setenv("HOME", realDir(t))
	for _, version := range []string{"24.04", "26.04"} {
		t.Run("ubuntu:"+version, func(t *testing.T) { onUbuntu(t, version) })
	}
}

// onUbuntu is the box's life on Ubuntu version: in a worktree of acme/api, whose .crabbox.yaml names it.
func onUbuntu(t *testing.T, version string) {
	ctx := t.Context()
	root, cfg := worktree(t)
	write(t, filepath.Join(root, ".crabbox.yaml"), "os: ubuntu:"+version+"\n")
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.name=land", "-c", "user.email=land@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "init")
	p, err := newPool(ctx, root, cfg, hetznerBox)
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	p.stdout = &stdout
	id, unlock, err := p.newBox()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var c claim
	var keys []int64
	t.Cleanup(func() { endAll(t, p, claim{Box: id, Server: c.Server}, keys) })

	c, err = p.create(ctx, id, "cpx22", true) // as a snapshot's build: from crabbox's plain Ubuntu, which os: names
	if err != nil {
		t.Fatalf("creating a box from ubuntu:%s: %v", version, err)
	}
	var s struct {
		Server struct {
			server
			Image struct {
				OSVersion string `json:"os_version"`
			} `json:"image"`
		} `json:"server"`
	}
	if err := p.hz.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", c.Server), nil, &s); err != nil {
		t.Fatal(err)
	}
	if s.Server.Image.OSVersion != version {
		t.Fatalf("crabbox's os: ubuntu:%s booted Ubuntu %q", version, s.Server.Image.OSVersion)
	}
	if keys, err = p.hz.keys(ctx, s.Server.server); err != nil || len(keys) == 0 {
		t.Fatalf("the lease's SSH keys: %v, %v; want crabbox's", keys, err)
	}

	b := &box{p, c}
	if err := b.Run(ctx, nil, "bash", "-c", `. /etc/os-release && echo "$VERSION_ID $GREETING"`); err != nil {
		t.Fatalf("a box command: %v", err)
	}
	if want := version + " it's here\n"; stdout.String() != want {
		t.Fatalf("the box command printed %q, want %q", &stdout, want)
	}
	ssh, err := p.ssh(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.code.run
	out, err := p.onBox(ctx, ssh, `sudo cat `+dir+`/created `+dir+`/deadline; date +%s`, "")
	if err != nil {
		t.Fatal(err)
	}
	var created, deadline, now int64
	if _, err := fmt.Sscan(out, &created, &deadline, &now); err != nil ||
		(deadline+300-created)%3600 != 0 || deadline < now+900 || deadline > now+900+3600 {
		t.Fatalf("after a box command, created, deadline and now are %q, %v; want the first deadline, 5 minutes before "+
			"the end of a paid hour, at least 15 minutes from now", out, err)
	}

	// Its deadline passed, a box something is connected to, this script's own session, stays another hour.
	out, err = p.onBox(ctx, ssh, `set -eu
passed=$(( $(date +%s) - 1 ))
echo $passed | sudo tee `+dir+`/deadline >/dev/null
for i in $(seq 150); do [ "$(sudo cat `+dir+`/deadline)" = $passed ] || break; sleep 1; done
echo $(( $(sudo cat `+dir+`/deadline) - passed ))`, "")
	if err != nil || strings.TrimSpace(out) != "3600" {
		t.Fatalf("a box in use, its deadline passed, moved it %q s on, %v; want 3600", out, err)
	}

	// Its user's sessions ended, crabbox's connection and this script's own among them, and only then its deadline
	// passed again, the box is idle. Passing the deadline first would race the self-delete's minute tick: a tick
	// while this script is still connected rightly keeps the box another hour.
	if _, err := p.onBox(ctx, ssh, `set -eu
user=$(id -un)
sudo systemd-run --quiet --on-active=3 /bin/bash -c "loginctl terminate-user $user && sleep 5 &&
  echo \$(( \$(date +%s) - 1 )) >`+dir+`/deadline"`, ""); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); ; {
		if _, err := p.hz.server(ctx, c.Server); notFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 3*time.Minute {
			t.Fatal("an idle box past its deadline didn't delete itself in 3 minutes")
		}
		time.Sleep(10 * time.Second)
	}
	for _, k := range keys {
		if err := p.hz.do(ctx, http.MethodGet, "/ssh_keys/"+strconv.FormatInt(k, 10), nil, nil); !notFound(err) {
			t.Errorf("the box deleted itself, but not its SSH key %d: %v", k, err)
		}
	}
}

// endAll ends the box c, as land does, and fails the test unless Hetzner then has neither its server, by ID or slug,
// nor its lease's SSH keys.
func endAll(t *testing.T, p *pool, c claim, keys []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := p.drop(ctx, c); err != nil {
		t.Errorf("ending the box: %v", err)
	}
	if ids, err := p.hz.slugged(ctx, "land-"+c.Box); err != nil || len(ids) > 0 {
		t.Errorf("servers land-%s left: %v, %v", c.Box, ids, err)
	}
	if c.Server != 0 {
		if _, err := p.hz.server(ctx, c.Server); !notFound(err) {
			t.Errorf("server %d left: %v", c.Server, err)
		}
	}
	for _, k := range keys {
		if err := p.hz.do(ctx, http.MethodGet, "/ssh_keys/"+strconv.FormatInt(k, 10), nil, nil); !notFound(err) {
			t.Errorf("SSH key %d left: %v", k, err)
		}
	}
	t.Logf("box %s: server %d and SSH keys %v gone", c.Box, c.Server, keys)
}
