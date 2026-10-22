package forge

import (
	"context"
	"encoding/json"
	"io"
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
)

// api is a fake GitHub REST API: it records each request as "METHOD path?query body", refuses one without the token,
// and answers the others from routes, by "METHOD path", with a status and a body; a missing route is a 404.
type api struct {
	token  string
	routes map[string]func(w http.ResponseWriter)

	mu       sync.Mutex
	requests []string
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.requests = append(a.requests, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
	a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+a.token || r.Header.Get("X-GitHub-Api-Version") == "" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	if route, ok := a.routes[r.Method+" "+r.URL.Path]; ok {
		route(w)
		return
	}
	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
}

func reply(status int, body string, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// adapter serves a and returns the GitHub adapter FromEnv makes for repository o/r with the token, as in Actions,
// with each rate-limit wait recorded in waits instead of slept.
func adapter(t *testing.T, a *api) (*gitHub, *[]time.Duration) {
	t.Helper()
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	t.Setenv("GH_TOKEN", a.token)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("GITHUB_API_URL", server.URL)
	f, err := FromEnv(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := f.(*gitHub)
	waits := &[]time.Duration{}
	g.wait = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return g, waits
}

// Marks come from the list endpoint, newest first as GitHub lists them, only those in the context land, each with its
// creator, time and its own error state.
func TestMarksReadTheListEndpoint(t *testing.T) {
	statuses := `[
		{"context":"ci/other","state":"success","creator":{"login":"bot"},"created_at":"2026-01-01T00:00:03Z"},
		{"context":"land","state":"error","description":"No verdict","target_url":"https://x/1","creator":{"login":"b"},"created_at":"2026-01-01T00:00:02Z"},
		{"context":"land","state":"pending","creator":{"login":"a"},"created_at":"2026-01-01T00:00:01Z"}]`
	a := &api{token: "t", routes: map[string]func(http.ResponseWriter){"GET /repos/o/r/commits/abc/statuses": reply(200, statuses)}}
	g, _ := adapter(t, a)
	marks, err := g.Marks(t.Context(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	want := []Mark{
		{State: Error, Description: "No verdict", TargetURL: "https://x/1", Creator: "b", CreatedAt: time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)},
		{State: Pending, Creator: "a", CreatedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)},
	}
	same := func(a, b Mark) bool {
		return a.State == b.State && a.Description == b.Description && a.TargetURL == b.TargetURL && a.Creator == b.Creator && a.CreatedAt.Equal(b.CreatedAt)
	}
	if !slices.EqualFunc(marks, want, same) {
		t.Errorf("marks = %+v, want %+v", marks, want)
	}
	if !slices.Equal(a.requests, []string{"GET /repos/o/r/commits/abc/statuses?per_page=100"}) {
		t.Errorf("requests = %q", a.requests)
	}
}

// Each other operation sends the request GitHub's REST API documents, and reads its answer.
func TestTheOperationsSpeakGitHubsAPI(t *testing.T) {
	long := strings.Repeat("é", 200)
	a := &api{token: "t", routes: map[string]func(http.ResponseWriter){
		"POST /repos/o/r/statuses/abc":         reply(201, `{"id":1}`),
		"POST /repos/o/r/commits/abc/comments": reply(201, `{"html_url":"https://github.com/o/r/commit/abc#commitcomment-1"}`),
		"GET /repos/o/r/actions/workflows/ci.yml/runs": reply(200, `{"total_count":2,"workflow_runs":[`+
			`{"id":9,"status":"in_progress","conclusion":null},{"id":7,"status":"completed","conclusion":"failure"}]}`),
		"POST /repos/o/r/actions/runs/7/rerun": reply(201, `{}`),
		"GET /repos/o/r/issues/1":              reply(200, `{"state":"open"}`),
		"GET /repos/o/r/issues/2":              reply(200, `{"state":"closed"}`),
		"GET /repos/o/r/issues/3":              reply(200, `{"state":"open","pull_request":{"url":"x"}}`),
		"GET /repos/o/r/issues/5":              reply(500, `{"message":"Server Error"}`),
	}}
	g, _ := adapter(t, a)
	ctx := t.Context()
	if err := g.SetMark(ctx, "abc", Mark{State: Success, Description: long, TargetURL: "https://x/2"}); err != nil {
		t.Fatal(err)
	}
	var posted map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(a.requests[0], "POST /repos/o/r/statuses/abc ")), &posted); err != nil {
		t.Fatal(err)
	}
	if posted["state"] != "success" || posted["context"] != "land" || posted["target_url"] != "https://x/2" || len([]rune(posted["description"])) != 140 {
		t.Errorf("SetMark posted %q; want success in context land, its description cut to GitHub's 140 characters", posted)
	}
	if url, err := g.Comment(ctx, "abc", "it broke"); err != nil || url != "https://github.com/o/r/commit/abc#commitcomment-1" ||
		a.requests[1] != `POST /repos/o/r/commits/abc/comments {"body":"it broke"}` {
		t.Errorf("Comment = %q, %v after %q", url, err, a.requests[1])
	}
	runs, err := g.Runs(ctx, "abc")
	if err != nil || !slices.Equal(runs, []Run{{ID: 9, Status: "in_progress"}, {ID: 7, Status: "completed", Conclusion: "failure"}}) ||
		a.requests[2] != "GET /repos/o/r/actions/workflows/ci.yml/runs?event=push&head_sha=abc&per_page=100" {
		t.Errorf("Runs = %+v, %v after %q", runs, err, a.requests[2])
	}
	if err := g.Rerun(ctx, 7); err != nil || a.requests[3] != "POST /repos/o/r/actions/runs/7/rerun" {
		t.Errorf("Rerun: %v after %q", err, a.requests[3])
	}
	for n, want := range map[int]bool{1: true, 2: false, 3: false, 4: false} {
		if open, err := g.IssueOpen(ctx, n); err != nil || open != want {
			t.Errorf("IssueOpen(%d) = %t, %v; want %t", n, open, err, want)
		}
	}
	if open, err := g.IssueOpen(ctx, 5); err == nil || !strings.Contains(err.Error(), "500 Server Error") {
		t.Errorf("IssueOpen on a server error = %t, %v", open, err)
	}
}

// A repository without the CI workflow has no runs: GitHub's 404 for it is no error, so a landing there isn't failed
// after its mark.
func TestAMissingWorkflowHasNoRuns(t *testing.T) {
	g, _ := adapter(t, &api{token: "t", routes: map[string]func(http.ResponseWriter){}})
	if runs, err := g.Runs(t.Context(), "abc"); err != nil || len(runs) != 0 {
		t.Errorf("Runs without the workflow = %+v, %v; want none and no error", runs, err)
	}
}

// A request GitHub's rate limit refuses waits as long as GitHub asks, by retry-after, else until x-ratelimit-reset,
// and is sent again; a refusal that is no rate limit isn't.
func TestRateLimitsAreWaitedOut(t *testing.T) {
	reset := time.Now().Add(30 * time.Second).Unix()
	var calls int
	a := &api{token: "t", routes: map[string]func(http.ResponseWriter){
		"POST /repos/o/r/statuses/abc": func(w http.ResponseWriter) {
			calls++
			switch calls {
			case 1:
				reply(429, `{"message":"slow down"}`, "Retry-After", "7")(w)
			case 2:
				reply(403, `{"message":"API rate limit exceeded"}`, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(reset, 10))(w)
			default:
				reply(201, `{}`)(w)
			}
		},
		"POST /repos/o/r/statuses/def": reply(403, `{"message":"Resource not accessible by integration"}`),
	}}
	g, waits := adapter(t, a)
	if err := g.SetMark(t.Context(), "abc", Mark{State: Success}); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 2 || (*waits)[0] != 7*time.Second || (*waits)[1] < 29*time.Second || (*waits)[1] > 32*time.Second || calls != 3 {
		t.Errorf("waited %v over %d requests; want 7s, then until the reset 30s away, then a third request", *waits, calls)
	}
	if err := g.SetMark(t.Context(), "def", Mark{State: Success}); err == nil || len(*waits) != 2 {
		t.Errorf("a refusal that is no rate limit: %v, after waits %v", err, *waits)
	}
}

// Outside Actions, the token comes from gh and the repository from origin's URL, whose name may hold a dot.
func TestFromEnvFindsTheTokenAndRepositoryOnTheMac(t *testing.T) {
	a := &api{token: "from-gh", routes: map[string]func(http.ResponseWriter){"POST /repos/acme/web.site/actions/runs/1/rerun": reply(201, `{}`)}}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	bin, repo := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n[ \"$*\" = 'auth token' ] && echo from-gh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull) // no url.*.insteadOf of the machine's rewrites origin
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/web.site.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_API_URL", server.URL)
	f, err := FromEnv(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Rerun(t.Context(), 1); err != nil {
		t.Errorf("Rerun: %v after %q", err, a.requests)
	}
	t.Setenv("PATH", t.TempDir()) // no gh
	if _, err := FromEnv(t.Context(), repo); err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("with no token anywhere: %v", err)
	}
}
