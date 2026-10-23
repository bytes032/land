package boxes

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytes032/land/internal/config"
)

// fakeCrabbox is crabbox 0.78.1, or the version in version, in direct Hetzner mode as land uses it, in the directory
// $FAKE_CRABBOX: each call's arguments are a line of calls, and a line of envs with its XDG_STATE_HOME and TMPDIR after
// a tab each; the project's servers are the lines of servers, one JSON object each, created at $FAKE_CREATED or now,
// and its SSH keys the lines of keys, which the fake API reads too. warmup adds its type and location,
// CRABBOX_HETZNER_LOCATION, to attempts; it refuses as many boxes at the project's limit as refuse holds, a type
// dedicated-limit lists at the project's dedicated-core limit, a type a line of sold-out names, alone or with the
// location, as Hetzner refuses one it has none of to sell there, and an image the API deleted, a line of
// deleted-images; it adds the image it starts from to images, keeps the lease's claim, key pair and a temporary file
// where crabbox does, in the state and temporary directories, registers the key and labels the server with the slug it
// is given, as crabbox does, and adds the lease and slug to leases. ssh prints a command that runs the remote command
// here, so a box's workspaces are under work/; run adds the commit its checkout holds to heads and runs its command in
// the lease's workspace. run fails while the file unreachable exists, and ssh while sshfail does. It refuses calls not
// made for direct Hetzner with 24 h leases, or not in a box's folder, and stop, cp and list, which newFake fails the
// test for.
const fakeCrabbox = `#!/bin/sh
state=$FAKE_CRABBOX
if [ "$1" = --version ]; then cat "$state/version" 2>/dev/null || echo 0.78.1; exit; fi
[ "$CRABBOX_PROVIDER/$CRABBOX_MAX_TTL/$CRABBOX_SYNC_GIT_SEED" = hetzner/24h/false ] || { echo "fake crabbox: wrong environment" >&2; exit 2; }
printf '%s\n' "$*" >>"$state/calls"
case $1 in stop | cp | list)
  printf '%s\n' "$*" >>"$state/forbidden"
  echo "fake crabbox: land never runs crabbox $1" >&2
  exit 2 ;;
esac
case $XDG_STATE_HOME in */boxes/????????????/state) ;; *) echo "fake crabbox: XDG_STATE_HOME isn't a box's" >&2; exit 2 ;; esac
[ "$TMPDIR" = "${XDG_STATE_HOME%/state}/tmp" ] || { echo "fake crabbox: TMPDIR isn't the box's" >&2; exit 2; }
printf '%s\t%s\t%s\n' "$*" "$XDG_STATE_HOME" "$TMPDIR" >>"$state/envs"
case $1 in
warmup)
  while [ $# -gt 0 ]; do
    case $1 in --slug) slug=$2; shift 2 ;; --type) type=$2; shift 2 ;; *) shift ;; esac
  done
  printf '%s %s\n' "$type" "$CRABBOX_HETZNER_LOCATION" >>"$state/attempts"
  refuse=$(cat "$state/refuse" 2>/dev/null || echo 0)
  if [ "$refuse" -gt 0 ]; then
    echo $((refuse - 1)) >"$state/refuse"
    echo 'crabbox: hetzner POST /servers: http 403: {"error":{"code":"resource_limit_exceeded","message":"server limit reached"}}' >&2
    exit 1
  fi
  if grep -qx "$type" "$state/dedicated-limit" 2>/dev/null; then
    echo "$type: hetzner POST /servers: http 403: {\"error\":{\"code\":\"dedicated_core_limit\",\"details\":{},\"message\":\"dedicated core limit reached\"}}" >&2
    exit 1
  fi
  if grep -qxE "$type( $CRABBOX_HETZNER_LOCATION)?" "$state/sold-out" 2>/dev/null; then
    echo "$type: hetzner POST /servers: http 412: {\"error\":{\"code\":\"resource_unavailable\",\"details\":{},\"message\":\"error during placement\"}}" >&2
    exit 1
  fi
  if grep -qx "${CRABBOX_HETZNER_IMAGE:-none}" "$state/deleted-images" 2>/dev/null; then
    echo 'crabbox: hetzner POST /servers: http 404: {"error":{"code":"not_found","message":"image not found"}}' >&2
    exit 1
  fi
  lease=$(printf 'cbx_%012x' $$) key=crabbox-cbx-$(printf %012x $$)
  keys=$XDG_STATE_HOME/crabbox/testboxes/$lease
  mkdir -p "$keys" "$XDG_STATE_HOME/crabbox/claims" && echo "{}" >"$XDG_STATE_HOME/crabbox/claims/$lease.json" &&
    touch "$TMPDIR/crabbox-$lease" && ssh-keygen -q -t ed25519 -N '' -C "crabbox $lease" -f "$keys/id_ed25519" &&
    fingerprint=$(ssh-keygen -l -E md5 -f "$keys/id_ed25519.pub") || exit 1
  fingerprint=${fingerprint#* MD5:} && fingerprint=${fingerprint%% *}
  echo "{\"id\":$$,\"name\":\"$key\",\"fingerprint\":\"$fingerprint\"}" >>"$state/keys"
  echo "{\"id\":$$,\"name\":\"crabbox-$slug\",\"status\":\"running\",\"created\":\"${FAKE_CREATED:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}\",\"labels\":{\"crabbox\":\"true\",\"created_by\":\"crabbox\",\"lease\":\"$lease\",\"slug\":\"$slug\",\"provider_key\":\"$key\"}}" >>"$state/servers"
  printf '%s\n' "${CRABBOX_HETZNER_IMAGE:-plain Ubuntu}" >>"$state/images"
  printf '%s %s\n' "$lease" "$slug" >>"$state/leases"
  echo "leased $lease slug=$slug provider=hetzner server=$$ type=$type ip=192.0.2.1 idle_timeout=24h0m0s expires=-"
  echo "ready ssh=crabbox@192.0.2.1:2222 network=public workroot=$state/work"
  echo "warmup complete total=1s" ;;
ssh)
  [ ! -e "$state/sshfail" ] || { echo "crabbox: ssh: lease unreachable" >&2; exit 1; }
  echo "'sh' '-c'" ;;
run)
  lease=$3
  git rev-parse HEAD >>"$state/heads" 2>/dev/null || echo none >>"$state/heads"
  script=
  while [ "$1" != -- ]; do [ "$1" != --script-stdin ] || script=$state/script.$$; shift; done
  shift
  [ -z "$script" ] || cat >"$script"
  [ ! -e "$state/unreachable" ] || { echo "ssh: connect to host 192.0.2.1 port 2222: Connection refused" >&2; exit 255; }
  cd "$state/work/$lease/api" || exit 1
  [ -z "$script" ] || exec bash "$script" "$@"
  exec "$@" ;;
*)
  exit 2 ;;
esac
`

// standIns are fakeCrabbox and what a box runs that a Mac lacks or mustn't run: sudo runs its command, but with -n
// fails while the fake crabbox's sudo-asks exists, as a sudo that wants a password does; systemctl's apt timers are on
// while $TIMERS holds "on", and its disable turns them off, while the other units are files in $UNITS, which stop
// removes; systemd-run, systemd 255's, makes a timer such a file, and runs any other unit's command at once, in its
// working directory, refusing a command line with ${ or $$ that systemd would expand, unless told not to; flock locks
// nothing, ss finds no connection, and cloud-init does nothing.
var standIns = map[string]string{
	"crabbox": fakeCrabbox,
	"sudo": `if [ "$1" = -n ]; then
  [ ! -e "$FAKE_CRABBOX/sudo-asks" ] || { echo 'sudo: a password is required' >&2; exit 1; }
  shift
fi
exec "$@"`,
	"systemctl": `[ "$1" != --quiet ] || shift
case $1/$2 in
is-enabled/apt-daily* | is-active/apt-daily*) [ "$(cat "$TIMERS")" = on ] ;;
is-active/*) [ -e "$UNITS/$2" ] ;;
disable/*) echo off >"$TIMERS" ;;
stop/*) shift; for unit; do rm -f "$UNITS/$unit"; done ;;
*) exit 2 ;;
esac`,
	"systemd-run": `unit= dir=. timer= expand=yes
while [ $# -gt 0 ]; do
  case $1 in
  --unit) unit=$2; shift 2 ;;
  --working-directory) dir=$2; shift 2 ;;
  --on-calendar) timer=$2; shift 2 ;;
  --uid | --gid) shift 2 ;;
  --expand-environment=no) expand=no; shift ;;
  --help) echo '     --expand-environment=BOOL'; exit 0 ;;
  --*) shift ;;
  *) break ;;
  esac
done
if [ $expand = yes ]; then
  for arg; do
    case $arg in *'${'* | *'$$'*) echo 'systemd-run: systemd would expand ${…} or $$ in the command line' >&2; exit 1 ;; esac
  done
fi
if [ -n "$timer" ]; then echo "$*" >"$UNITS/$unit.timer"; exit 0; fi
# A login shell on a Mac would put the system's sudo before the stand-ins.
[ "$1 $2" != "bash -lc" ] || { shift 2; set -- bash -c "$@"; }
cd "$dir" && exec "$@"`,
	"flock":      ``,
	"ss":         ``,
	"cloud-init": ``,
}

// fake is a test's crabbox, Hetzner project and boxes.
type fake struct {
	dir string
	api *fakeHetzner
}

// newFake puts the stand-ins first on PATH, each recording its calls in $CALLS, serves a fake Hetzner API over the
// fake crabbox's project with one available snapshot of acme.api, image 1, and the server types fakeTypes, makes the
// boxes' build roots and self-delete directories of the test's own, and gives the test a home of its own, so the
// claims are its own. It fails the test if land runs crabbox stop, cp or list.
func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{dir: realDir(t)}
	f.api = &fakeHetzner{servers: filepath.Join(f.dir, "servers"), keys: filepath.Join(f.dir, "keys"), made: time.Now(), images: []image{
		{ID: 1, Status: "available", Created: time.Now().Add(-time.Hour), Labels: map[string]string{snapshotLabel: "acme.api", "commit": "old"}},
	}, types: slices.Clone(fakeTypes)}
	api := httptest.NewServer(f.api)
	t.Cleanup(api.Close)
	t.Cleanup(func() {
		if calls := f.lines(t, "forbidden", ""); len(calls) > 0 {
			t.Errorf("land ran crabbox %q, which leaves the box's state outside its folder", calls)
		}
	})
	bin := realDir(t)
	record := `printf '%s\n' "$(basename "$0") $*" >>"$CALLS"` + "\n"
	for name, script := range standIns {
		if name != "crabbox" {
			script = record + script
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+strings.TrimPrefix(script, "#!/bin/sh\n")+"\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		"PATH":            bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_CRABBOX":    f.dir,
		"CALLS":           filepath.Join(f.dir, "stand-in-calls"),
		"TIMERS":          filepath.Join(f.dir, "timers"),
		"UNITS":           realDir(t),
		"HOME":            realDir(t),
		"XDG_CONFIG_HOME": "",
		"TMPDIR":          realDir(t), // where the box script's lock lives
		"HCLOUD_TOKEN":    testToken,
	} {
		t.Setenv(name, value)
	}
	f.write(t, "timers", "off\n")
	vars := []*string{&boxRoot, &runDir, &hetznerAPI, &metadataURL}
	old := make([]string, len(vars))
	for i, v := range vars {
		old[i] = *v
	}
	t.Cleanup(func() {
		for i, v := range vars {
			*v = old[i]
		}
	})
	boxRoot, runDir, hetznerAPI, metadataURL = realDir(t), filepath.Join(realDir(t), "land"), api.URL, api.URL+"/hetzner/v1/metadata"
	return f
}

// realDir is a new temporary directory by a path without links, as the box script's `cd -P` sees it.
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *fake) write(t *testing.T, name, content string) {
	t.Helper()
	write(t, filepath.Join(f.dir, name), content)
}

// lines are the lines of the fake's file name that start with prefix.
func (f *fake) lines(t *testing.T, name, prefix string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

// calls are crabbox's calls, one line each, that start with prefix.
func (f *fake) calls(t *testing.T, prefix string) []string { return f.lines(t, "calls", prefix) }

// servers are the project's servers, by lease.
func (f *fake) servers(t *testing.T) map[string]server {
	t.Helper()
	servers, err := f.api.load()
	if err != nil {
		t.Fatal(err)
	}
	byLease := map[string]server{}
	for _, s := range servers {
		byLease[s.Labels["lease"]] = s
	}
	return byLease
}

// setServer sets the status of lease's server, or removes the server when status is "".
func (f *fake) setServer(t *testing.T, lease, status string) {
	t.Helper()
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	servers, err := f.api.load()
	if err != nil {
		t.Fatal(err)
	}
	for i := range servers {
		if servers[i].Labels["lease"] == lease {
			servers[i].Status = status
		}
	}
	if err := f.api.save(slices.DeleteFunc(servers, func(s server) bool { return s.Status == "" })); err != nil {
		t.Fatal(err)
	}
}

// keys are the project's SSH keys, by name.
func (f *fake) keys(t *testing.T) map[string]fakeKey {
	t.Helper()
	keys, err := f.api.loadKeys()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]fakeKey{}
	for _, k := range keys {
		byName[k.Name] = k
	}
	return byName
}

const testToken = "test-token-0123456789abcdef"

// fakeHetzner is Hetzner's API over the fake crabbox's project, whose servers and SSH keys it reads from and writes to
// its files of them, over images, the snapshots, and over types, the server types: a listing of servers honours a
// label selector of key=value terms, and pages of up to 50; a new snapshot stays creating for as many reads as creating
// says, created at made, then turns available, or unavailable, failed, when failing is set; a deleted image is a line
// of the fake crabbox's deleted-images, which no box boots any more. Every call that changes something is a line of
// changes, "METHOD /path", and each read finding a new snapshot available is "available /images/ID". Every deletion
// fails while deleteFails is set. imaged runs as a snapshot starts; the metadata service names instance, unless
// noMetadata is set, as off Hetzner.
type fakeHetzner struct {
	mu          sync.Mutex
	servers     string
	keys        string
	images      []image
	types       []fakeType
	creating    int
	failing     bool
	deleteFails bool
	made        time.Time
	changes     []string
	imaged      func()
	instance    int64
	noMetadata  bool
}

// fakeType is a server type of the fake API's, priced price an hour in Europe and a third more in sin, and sold now
// in the locations in.
type fakeType struct {
	name, arch, cpu string
	cores           int
	memory, disk    float64
	price           float64
	deprecated      bool
	in              []string
}

// fakeLocations are the fake API's, of which Hetzner recommends hel1.
var fakeLocations = []string{"fsn1", "nbg1", "hel1", "sin"}

// fakeTypes are the fake API's server types, as Hetzner's, in no order of price: the tests' cpx42 and ccx63, and,
// as big as a cpx42 or bigger, dearer x86 ones; a cheaper one whose disk is small, cx43; one Hetzner sells nowhere now,
// cx53; an Arm one, cax41; and cpx51, which the fake has deprecated.
var fakeTypes = []fakeType{
	{"ccx63", "x86", "dedicated", 48, 192, 960, 1.9365, false, fakeLocations},
	{"ccx43", "x86", "dedicated", 16, 64, 360, 0.6259, false, fakeLocations},
	{"ccx33", "x86", "dedicated", 8, 32, 240, 0.3134, false, fakeLocations},
	{"cpx62", "x86", "shared", 16, 32, 640, 0.2942, false, fakeLocations},
	{"cpx42", "x86", "shared", 8, 16, 320, 0.1576, false, fakeLocations},
	{"cpx51", "x86", "shared", 16, 32, 360, 0.1605, true, fakeLocations},
	{"cax41", "arm", "shared", 16, 32, 320, 0.0932, false, fakeLocations},
	{"cx53", "x86", "shared", 16, 32, 320, 0.0673, false, nil},
	{"cx43", "x86", "shared", 8, 16, 160, 0.0355, false, fakeLocations},
}

// sell has Hetzner sell the type name now in the locations in, and nowhere else.
func (f *fakeHetzner) sell(name string, in ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.types {
		if f.types[i].name == name {
			f.types[i].in = in
		}
	}
}

// fakeKey is an SSH key of the fake project's, with its fingerprint, which Hetzner computes.
type fakeKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

func (f *fakeHetzner) loadKeys() ([]fakeKey, error) {
	data, err := os.ReadFile(f.keys)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var keys []fakeKey
	for line := range strings.Lines(string(data)) {
		var k fakeKey
		if err := json.Unmarshal([]byte(line), &k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (f *fakeHetzner) saveKeys(keys []fakeKey) error {
	var lines bytes.Buffer
	for _, k := range keys {
		data, err := json.Marshal(k)
		if err != nil {
			return err
		}
		lines.Write(append(data, '\n'))
	}
	return os.WriteFile(f.keys, lines.Bytes(), 0o600)
}

// failDeletes makes every deletion fail, as an outage of Hetzner's API does, until it is called with false.
func (f *fakeHetzner) failDeletes(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteFails = fail
}

func (f *fakeHetzner) load() ([]server, error) {
	data, err := os.ReadFile(f.servers)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var servers []server
	for line := range strings.Lines(string(data)) {
		var s server
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, err
		}
		servers = append(servers, s)
	}
	return servers, nil
}

func (f *fakeHetzner) save(servers []server) error {
	var lines bytes.Buffer
	for _, s := range servers {
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		lines.Write(append(data, '\n'))
	}
	return os.WriteFile(f.servers, lines.Bytes(), 0o600)
}

func (f *fakeHetzner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/hetzner/v1/metadata/instance-id" {
		if f.noMetadata {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, f.instance)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"unable to authenticate"}}`)
		return
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	missing := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"error":{"code":"not_found","message":"not found"}}`)
	}
	servers, err := f.load()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	keys, err := f.loadKeys()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	call, query := r.Method+" "+r.URL.Path, r.URL.Query()
	page := map[string]any{"pagination": map[string]any{"next_page": nil}}
	i := slices.IndexFunc(servers, func(s server) bool { return s.ID == id })
	j := slices.IndexFunc(f.images, func(m image) bool { return m.ID == id })
	k := slices.IndexFunc(keys, func(k fakeKey) bool { return k.ID == id })
	switch {
	case r.Method == http.MethodDelete && f.deleteFails:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"error":{"code":"unavailable","message":"service unavailable"}}`)
	case call == "GET /servers":
		selected := slices.DeleteFunc(servers, func(s server) bool {
			for term := range strings.SplitSeq(query.Get("label_selector"), ",") {
				if key, value, _ := strings.Cut(term, "="); term != "" && s.Labels[key] != value {
					return true
				}
			}
			return false
		})
		perPage, _ := strconv.Atoi(query.Get("per_page"))
		perPage = min(cmp.Or(perPage, 25), 50)
		n, _ := strconv.Atoi(query.Get("page"))
		n = max(n, 1)
		end := min(n*perPage, len(selected))
		if end < len(selected) {
			page = map[string]any{"pagination": map[string]any{"next_page": n + 1}}
		}
		reply(map[string]any{"servers": selected[min((n-1)*perPage, end):end], "meta": page})
	case r.Method == http.MethodGet && parts[0] == "servers" && i >= 0:
		reply(map[string]any{"server": servers[i]})
	case r.Method == http.MethodPut && parts[0] == "servers" && i >= 0:
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		servers[i].Labels = body.Labels
		_ = f.save(servers)
		f.changes = append(f.changes, call)
	case r.Method == http.MethodDelete && parts[0] == "servers" && i >= 0:
		_ = f.save(slices.Delete(servers, i, i+1))
		f.changes = append(f.changes, call)
	case call == "GET /ssh_keys":
		reply(map[string]any{"ssh_keys": slices.DeleteFunc(keys, func(k fakeKey) bool {
			return query.Has("name") && k.Name != query.Get("name") || query.Has("fingerprint") && k.Fingerprint != query.Get("fingerprint")
		}), "meta": page})
	case r.Method == http.MethodDelete && parts[0] == "ssh_keys" && k >= 0:
		_ = f.saveKeys(slices.Delete(keys, k, k+1))
		f.changes = append(f.changes, call)
	case call == "GET /images":
		reply(map[string]any{"images": f.images, "meta": page})
	case call == "GET /server_types":
		var types []map[string]any
		for _, t := range f.types {
			var prices, locations []map[string]any
			for _, l := range fakeLocations {
				price := t.price
				if l == "sin" {
					price = price * 4 / 3
				}
				prices = append(prices, map[string]any{"location": l, "price_hourly": map[string]string{"gross": strconv.FormatFloat(price, 'f', 16, 64)}})
				locations = append(locations, map[string]any{"name": l, "available": slices.Contains(t.in, l), "recommended": l == "hel1"})
			}
			var deprecation any
			if t.deprecated {
				deprecation = map[string]string{"announced": "2026-01-01T00:00:00Z", "unavailable_after": "2026-04-01T00:00:00Z"}
			}
			types = append(types, map[string]any{"name": t.name, "architecture": t.arch, "cpu_type": t.cpu, "cores": t.cores,
				"memory": t.memory, "disk": t.disk, "deprecation": deprecation, "prices": prices, "locations": locations})
		}
		reply(map[string]any{"server_types": types, "meta": page})
	case r.Method == http.MethodGet && parts[0] == "images" && j >= 0:
		if f.images[j].Status == "creating" {
			switch {
			case f.creating > 0:
				f.creating--
			case f.failing:
				f.images[j].Status = "unavailable"
			default:
				f.images[j].Status = "available"
				f.changes = append(f.changes, "available "+r.URL.Path)
			}
		}
		reply(map[string]any{"image": f.images[j]})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/create_image") && i >= 0:
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.imaged != nil {
			f.imaged()
		}
		made := image{ID: int64(100 + len(f.images)), Status: "available", Created: f.made, Labels: body.Labels}
		f.changes = append(f.changes, call)
		if f.creating > 0 {
			made.Status = "creating"
		} else {
			f.changes = append(f.changes, "available /images/"+strconv.FormatInt(made.ID, 10))
		}
		f.images = append(f.images, made)
		reply(map[string]any{"image": made})
	case r.Method == http.MethodDelete && parts[0] == "images" && j >= 0:
		f.delete(j)
		f.changes = append(f.changes, call)
	default:
		missing()
	}
}

// delete deletes snapshot j, which no box boots any more.
func (f *fakeHetzner) delete(j int) {
	deleted, err := os.OpenFile(filepath.Join(filepath.Dir(f.servers), "deleted-images"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintln(deleted, f.images[j].ID)
		_ = deleted.Close()
	}
	f.images = slices.Delete(f.images, j, j+1)
}

// replace makes snapshot the project's one snapshot and deletes the others, as a spare's refresh does.
func (f *fakeHetzner) replace(snapshot image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.images) > 0 {
		f.delete(0)
	}
	f.images = []image{snapshot}
}

// changed is what changed in the project so far, and its snapshots.
func (f *fakeHetzner) changed() ([]string, []image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.changes), slices.Clone(f.images)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testConfig is a .land.yaml that config.Load takes, and the configuration it holds.
const testConfig = `version: 0.1.0
trunk: main
gate: {timeout: 30m, unverified: fail}
box: {type: cpx42, env: {GREETING: "it's here"}}
stages:
  check: {test: {run: 'true'}}
`

// worktree is a new worktree of acme/api, whose crabbox name is api, with testConfig.
func worktree(t *testing.T) (root string, cfg *config.Config) {
	t.Helper()
	root = realDir(t)
	git(t, root, "init", "-q")
	git(t, root, "remote", "add", "origin", "https://github.com/acme/api.git")
	write(t, filepath.Join(root, config.File), testConfig)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

// testPool is the pool of a new worktree, whose own output goes to stdout and the rest to stderr.
func testPool(t *testing.T) (p *pool, stdout, stderr *bytes.Buffer) {
	t.Helper()
	root, cfg := worktree(t)
	return poolAt(t, root, cfg)
}

func poolAt(t *testing.T, root string, cfg *config.Config) (p *pool, stdout, stderr *bytes.Buffer) {
	t.Helper()
	p, err := newPool(t.Context(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	p.stdout, p.stderr = stdout, stderr
	p.sleep = func(context.Context, time.Duration) error { return errors.New("slept") }
	p.background = func(string, ...string) error { return errors.New("no background in a test") }
	return p, stdout, stderr
}

func claimed(t *testing.T, p *pool, typ string) *box {
	t.Helper()
	b, err := p.claim(t.Context(), typ)
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, p.stderr)
	}
	return b
}

// claims are the repository's boxes as holder: lease, a spare's holder "spare".
func (p *pool) claims(t *testing.T) map[string]string {
	t.Helper()
	held := map[string]string{}
	if err := p.withState(func(s *state) error {
		for _, c := range s.Claims {
			held[cmp.Or(c.Worktree, "spare")] = c.Lease
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return held
}

// boxes are the folders of the repository's boxes, by lease, as their claims name them.
func (p *pool) boxes(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Claims []struct {
			Box   string `json:"box"`
			Lease string `json:"lease"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	folders := map[string]string{}
	for _, c := range s.Claims {
		folders[c.Lease] = filepath.Join(p.dir, c.Box)
	}
	return folders
}

// folders are the directories in the repository's boxes/, the boxes' folders.
func (p *pool) folders(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(p.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var folders []string
	for _, e := range entries {
		if e.IsDir() {
			folders = append(folders, filepath.Join(p.dir, e.Name()))
		}
	}
	return folders
}

// A worktree's first claim creates its box, a 24 h lease from the repository's snapshot whose workspace links to the
// build root, its slug land-<ID>, the ID naming its folder; later claims get the same box, and another worktree gets a
// box of its own. The worktree's repository ignores the bundles crabbox saves of failed commands, in .crabbox/.
func TestAWorktreeKeepsItsBoxAndOthersGetTheirOwn(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	if link, err := os.Readlink(filepath.Join(f.dir, "work", b.c.Lease, "api")); err != nil || link != p.build {
		t.Fatalf("the lease's workspace links to %q, %v; want the build root %s", link, err, p.build)
	}
	if again := claimed(t, p, "cpx42"); again.c.Lease != b.c.Lease {
		t.Fatalf("the second claim got %+v, want %+v", again.c, b.c)
	}
	other, _, _ := testPool(t)
	if theirs := claimed(t, other, "cpx42"); theirs.c.Lease == b.c.Lease {
		t.Fatalf("another worktree got this worktree's box %s", b.c.Lease)
	}
	warmups := f.calls(t, "warmup")
	if want := "warmup --slug " + b.c.Slug + " --type cpx42 --ttl 24h --idle-timeout 24h"; len(warmups) != 2 || warmups[0] != want ||
		b.c.Slug != "land-"+filepath.Base(p.boxes(t)[b.c.Lease]) {
		t.Fatalf("crabbox warmups %q of %s; want two, the first %q, the slug land-<the box's folder>", warmups, p.boxes(t)[b.c.Lease], want)
	}
	if images := f.lines(t, "images", ""); !slices.Equal(images, []string{"1", "1"}) {
		t.Fatalf("the boxes started from %q, want the snapshot, image 1, each", images)
	}
	if held := p.claims(t); len(held) != 2 || held[p.root] != b.c.Lease {
		t.Fatalf("claims %v", held)
	}
	exclude := filepath.Join(p.root, ".git/info/exclude")
	if data, err := os.ReadFile(exclude); err != nil || strings.Count(string(data), "/.crabbox/\n") != 1 {
		t.Fatalf("after two claims, %s holds %q, %v; want /.crabbox/ once", exclude, data, err)
	}
	if git(t, p.root, "check-ignore", ".crabbox/captures/run.tar.gz") != ".crabbox/captures/run.tar.gz" {
		t.Fatal("the worktree doesn't ignore crabbox's failure bundles")
	}
}

// A new box whose image lacks what land's box code needs stops setup before it does anything else on the box, naming
// all it lacks at once, and the box goes with all it made; a box lacking nothing is readied as ever.
func TestABoxWhoseImageLacksWhatLandNeedsIsRefused(t *testing.T) {
	f := newFake(t)
	f.write(t, "sudo-asks", "")
	f.api.mu.Lock()
	f.api.noMetadata = true
	f.api.mu.Unlock()
	p, _, stderr := testPool(t)
	_, err := p.claim(t.Context(), "cpx42")
	want := "this box's image lacks sudo without a password, Hetzner's metadata service; land's box code needs them (see .crabbox.yaml os)"
	if err == nil || !strings.HasPrefix(err.Error(), "readying cbx_") || !strings.Contains(err.Error(), ": "+want) {
		t.Fatalf("a claim of a box lacking two needs: %v; want readying <lease>: %s\n%s", err, want, stderr)
	}
	for _, made := range []string{p.build, filepath.Join(runDir, "auth")} {
		if _, err := os.Stat(made); err == nil {
			t.Errorf("setup made %s before checking the box's image", made)
		}
	}
	if servers, keys := f.servers(t), f.keys(t); len(servers) != 0 || len(keys) != 0 || len(p.claims(t)) != 0 || len(p.folders(t)) != 0 {
		t.Fatalf("after the refusal: servers %v, keys %v, claims %v, folders %q; want none", servers, keys, p.claims(t), p.folders(t))
	}
	if err := os.Remove(filepath.Join(f.dir, "sudo-asks")); err != nil {
		t.Fatal(err)
	}
	f.api.mu.Lock()
	f.api.noMetadata = false
	f.api.mu.Unlock()
	b := claimed(t, p, "cpx42")
	if link, err := os.Readlink(filepath.Join(f.dir, "work", b.c.Lease, "api")); err != nil || link != p.build {
		t.Fatalf("the lease's workspace links to %q, %v; want the build root %s", link, err, p.build)
	}
	if _, err := os.Stat(filepath.Join(runDir, "auth")); err != nil {
		t.Fatalf("a box lacking nothing has no self-delete: %v", err)
	}
}

// --type works only on a worktree with no box or a box at least as big, in cores and memory, so a worktree never
// holds two boxes: its ccx63 serves a claim of a cpx42, its cpx42 never one of a ccx63. A claim asking for no type, as
// the landing's does, takes whatever box the worktree holds; a spare of a smaller type is no box of the type asked for.
func TestTypeOnlyOnAWorktreeWithNoBoxOrOneAsBig(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	small := claimed(t, p, "")
	if _, err := p.claim(t.Context(), "ccx63"); err == nil || !strings.Contains(err.Error(), "land boxes release") {
		t.Fatalf("a ccx63 for a worktree holding a cpx42: %v", err)
	}
	other, _, _ := testPool(t)
	big := claimed(t, other, "ccx63")
	if big.c.Type != "ccx63" {
		t.Fatalf("a worktree with no box got a %s, want the ccx63 it asked for", big.c.Type)
	}
	for _, typ := range []string{"", "cpx42"} {
		if b := claimed(t, other, typ); b.c.Lease != big.c.Lease {
			t.Fatalf("a claim asking for %q got %+v, not the worktree's ccx63 %s", typ, b.c, big.c.Lease)
		}
	}
	if err := p.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b := claimed(t, p, "ccx63"); b.c.Type != "ccx63" || b.c.Lease == small.c.Lease {
		t.Fatalf("after its release, the worktree got %+v", b.c)
	}
	if warmups := f.calls(t, "warmup"); len(warmups) != 3 {
		t.Fatalf("crabbox warmups %q, want three", warmups)
	}
}

// Before claiming, each box that should be gone is deleted, its server, its SSH key and its folder, and its claim
// dropped: a removed worktree's, one whose server is gone, and one crabbox powered off at the end of its lease.
func TestClaimsOfGoneWorktreesAndGoneServersAreDropped(t *testing.T) {
	f := newFake(t)
	removed, _, _ := testPool(t)
	gone, _, _ := testPool(t)
	off, _, _ := testPool(t)
	kept, _, _ := testPool(t)
	leases := map[*pool]string{}
	for _, p := range []*pool{removed, gone, off, kept} {
		leases[p] = claimed(t, p, "cpx42").c.Lease
	}
	folders := kept.boxes(t)
	if err := os.RemoveAll(removed.root); err != nil {
		t.Fatal(err)
	}
	f.setServer(t, leases[gone], "")
	f.setServer(t, leases[off], "off")
	p, _, stderr := testPool(t)
	mine := claimed(t, p, "cpx42")
	want := map[string]string{kept.root: leases[kept], p.root: mine.c.Lease}
	if held := p.claims(t); fmt.Sprint(held) != fmt.Sprint(want) {
		t.Fatalf("claims %v, want %v\n%s", held, want, stderr)
	}
	servers, keys := f.servers(t), f.keys(t)
	for q, lease := range leases {
		_, up := servers[lease]
		_, key := keys["crabbox-"+strings.ReplaceAll(lease, "_", "-")]
		_, err := os.Stat(folders[lease])
		if left := q == kept; up != left || key != left || (err == nil) != left {
			t.Errorf("%s: server up %t, key %t, folder %t; want each %t\n%s", lease, up, key, err == nil, left, stderr)
		}
	}
}

// At Hetzner's limit, a claim asks again every 30 s and gets a box once Hetzner stops refusing, having said once what
// Hetzner said and who holds the repository's boxes.
func TestAClaimAtHetznersLimitGetsABoxOnceItStopsRefusing(t *testing.T) {
	f := newFake(t)
	holder, _, _ := testPool(t)
	held := claimed(t, holder, "cpx42")
	f.write(t, "refuse", "2\n")
	p, _, stderr := testPool(t)
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	b := claimed(t, p, "cpx42")
	if want := []time.Duration{retryEvery, retryEvery}; !slices.Equal(slept, want) || b.c.Lease == held.c.Lease {
		t.Fatalf("slept %v and got %s; want %v and a new box", slept, b.c.Lease, want)
	}
	out := stderr.String()
	if strings.Count(out, "resource_limit_exceeded") != 1 || strings.Count(out, held.c.Lease+" ("+held.c.Slug+", a cpx42) held by "+holder.root) != 1 {
		t.Fatalf("want Hetzner's refusal and the holders said once:\n%s", out)
	}
}

// A claim gives up once Hetzner has refused it for 3 h, naming the refusal. (Each sleep here takes an hour.)
func TestAClaimAtHetznersLimitGivesUpAfterThreeHours(t *testing.T) {
	f := newFake(t)
	f.write(t, "refuse", "1000\n")
	p, _, _ := testPool(t)
	now := time.Now()
	var slept []time.Duration
	p.now = func() time.Time { return now }
	p.sleep = func(_ context.Context, d time.Duration) error {
		now, slept = now.Add(time.Hour), append(slept, d)
		return nil
	}
	_, err := p.claim(t.Context(), "cpx42")
	if want := []time.Duration{retryEvery, retryEvery, retryEvery}; err == nil || !strings.Contains(err.Error(), "resource_limit_exceeded") || !slices.Equal(slept, want) {
		t.Fatalf("after sleeps %v: %v; want %v, then the refusal", slept, err, want)
	}
	if warmups := f.calls(t, "warmup"); len(warmups) != 4 {
		t.Fatalf("crabbox warmups %q; want one at the start and one after each hour", warmups)
	}
	if held, folders := p.claims(t), p.folders(t); len(held) != 0 || len(folders) != 0 {
		t.Fatalf("claims %v and folders %q after giving up", held, folders)
	}
}

// Claims are under the lock: worktrees claiming at once each get a box of their own and lose no claim, and two
// claims of one worktree at once create one box.
func TestClaimsAtOnceUnderTheLock(t *testing.T) {
	f := newFake(t)
	var pools []*pool
	for range 4 {
		p, _, _ := testPool(t)
		twin := *p // the same worktree, in another land command
		twin.stdout, twin.stderr = &bytes.Buffer{}, &bytes.Buffer{}
		pools = append(pools, p, &twin)
	}
	var wg sync.WaitGroup
	for _, p := range pools {
		wg.Go(func() {
			if _, err := p.claim(context.Background(), "cpx42"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	held := pools[0].claims(t)
	leases := map[string]bool{}
	for _, lease := range held {
		leases[lease] = true
	}
	if len(held) != 4 || len(leases) != 4 || len(f.calls(t, "warmup")) != 4 {
		t.Fatalf("claims %v after warmups %q; want four worktrees with a box each", held, f.calls(t, "warmup"))
	}
}

// status names each box's lease and when a released box, a spare, deletes itself; and prints, ready to paste,
// crabbox's own tunnel and cp commands for the worktree's box, with its folder's state and land's 24 h lease ceiling.
func TestStatusNamesTheLeaseAndASparesDeadline(t *testing.T) {
	newFake(t)
	p, stdout, _ := testPool(t)
	b := claimed(t, p, "cpx42")
	if err := p.status(t.Context()); err != nil {
		t.Fatal(err)
	}
	env := "XDG_STATE_HOME=" + quote(filepath.Join(p.boxes(t)[b.c.Lease], "state")) + " CRABBOX_MAX_TTL=24h "
	if want := b.c.Lease + "\t" + b.c.Slug + "\tcpx42\trunning\tthis worktree\n" +
		env + "crabbox tunnel --provider hetzner --id " + b.c.Lease + " --local-port PORT PORT\n" +
		env + "crabbox cp --provider hetzner --id " + b.c.Lease + " SANDBOX:" + p.build + "/PATH PATH\n"; stdout.String() != want {
		t.Fatalf("status printed %q, want %q", stdout, want)
	}
	if err := Release(t.Context(), p.root, p.cfg); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := p.status(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := "this worktree holds no box; `land boxes run -- CMD` claims one\n" + b.c.Lease + "\t" + b.c.Slug +
		"\tcpx42\trunning\ta spare until "; !strings.HasPrefix(stdout.String(), want) {
		t.Fatalf("status after release printed %q, want %q…", stdout, want)
	}
}

// `land boxes run` runs the command on the worktree's box and exits with its exit code; a failed prepare is its
// stage's error instead; --type is refused on a worktree holding a smaller type.
func TestLandBoxesRun(t *testing.T) {
	newFake(t)
	root, _ := worktree(t)
	t.Chdir(root)
	ctx := t.Context()
	var exit interface{ ExitCode() int }
	if err := Main(ctx, []string{"run", "--", "sh", "-c", "[ \"$GREETING\" = \"it's here\" ] && exit 5"}); !errors.As(err, &exit) || exit.ExitCode() != 5 {
		t.Fatalf("a command exiting 5 gave %v", err)
	}
	if err := Main(ctx, []string{"run", "--type", "ccx63", "--", "true"}); err == nil || !strings.Contains(err.Error(), "release") {
		t.Fatalf("--type ccx63 on a worktree holding a cpx42 gave %v", err)
	}
	write(t, filepath.Join(root, config.File), testConfig+"  prepare: exit 3\n")
	var stage *StageError
	if err := Main(ctx, []string{"run", "true"}); !errors.As(err, &stage) || stage.Stage != "prepare" || errors.As(err, &exit) {
		t.Fatalf("a failed prepare gave %v", err)
	}
	if err := Main(ctx, []string{"run"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run without a command gave %v", err)
	}
}
