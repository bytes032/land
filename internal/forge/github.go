package forge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/proc"
)

// attempts bounds the tries of one request that GitHub's rate limit refuses.
const attempts = 5

// gitHub is the Forge over GitHub's REST API, for one repository.
type gitHub struct {
	api, repo, token string // repo is owner/name
	client           *http.Client
	// wait sleeps out a rate limit, or returns ctx's error first.
	wait func(ctx context.Context, d time.Duration) error
}

// FromEnv returns the GitHub adapter for the repository checked out at dir: owner and name from GITHUB_REPOSITORY in
// Actions, else its origin remote; the token from GH_TOKEN, else `gh auth token`; the API at GITHUB_API_URL, else
// GitHub's.
func FromEnv(ctx context.Context, dir string) (Forge, error) {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		out, err := proc.Output(ctx, proc.Spec{Argv: []string{"gh", "auth", "token"}, Dir: dir})
		if token = strings.TrimSpace(out); token == "" {
			return nil, fmt.Errorf("no GitHub token: set GH_TOKEN, or log in with gh auth login: %w", cmp.Or(err, errors.New("gh auth token printed none")))
		}
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		name, err := config.RepoName(ctx, dir)
		if err != nil {
			return nil, err
		}
		// GitHub's owner names hold no dot, so the first one splits <owner>.<name> back.
		owner, name, _ := strings.Cut(name, ".")
		repo = owner + "/" + name
	}
	return &gitHub{
		api:    strings.TrimSuffix(cmp.Or(os.Getenv("GITHUB_API_URL"), "https://api.github.com"), "/"),
		repo:   repo,
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
		wait:   sleep,
	}, nil
}

// SetMark posts a commit status in the context land. GitHub refuses a description over 140 characters, so a longer
// one is cut.
func (g *gitHub) SetMark(ctx context.Context, sha string, m Mark) error {
	if r := []rune(m.Description); len(r) > 140 {
		m.Description = string(r[:139]) + "…"
	}
	body := struct {
		State       State  `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description,omitempty"`
		TargetURL   string `json:"target_url,omitempty"`
	}{m.State, Context, m.Description, m.TargetURL}
	return g.call(ctx, http.MethodPost, "statuses/"+url.PathEscape(sha), body, nil)
}

// Marks reads the commit's statuses with the list endpoint, which GitHub returns newest first and, unlike the combined
// status, with each one's creator and its own error state, and keeps those in the context land.
// shortcut: reads the newest 100 statuses only; follow the Link header's next page if a commit ever carries more.
func (g *gitHub) Marks(ctx context.Context, sha string) ([]Mark, error) {
	var statuses []struct {
		Context     string    `json:"context"`
		State       State     `json:"state"`
		Description string    `json:"description"`
		TargetURL   string    `json:"target_url"`
		CreatedAt   time.Time `json:"created_at"`
		Creator     struct {
			Login string `json:"login"`
		} `json:"creator"`
	}
	if err := g.call(ctx, http.MethodGet, "commits/"+url.PathEscape(sha)+"/statuses?per_page=100", nil, &statuses); err != nil {
		return nil, err
	}
	var marks []Mark
	for _, s := range statuses {
		if s.Context == Context {
			marks = append(marks, Mark{State: s.State, Description: s.Description, TargetURL: s.TargetURL, Creator: s.Creator.Login, CreatedAt: s.CreatedAt})
		}
	}
	return marks, nil
}

// Comment posts a commit comment and returns its URL.
func (g *gitHub) Comment(ctx context.Context, sha, body string) (string, error) {
	var comment struct {
		URL string `json:"html_url"`
	}
	err := g.call(ctx, http.MethodPost, "commits/"+url.PathEscape(sha)+"/comments", map[string]string{"body": body}, &comment)
	return comment.URL, err
}

// Runs lists the commit's push runs of Workflow, which GitHub returns newest first. A repository without the workflow
// has no runs: GitHub answers 404 for it.
func (g *gitHub) Runs(ctx context.Context, sha string) ([]Run, error) {
	var page struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	query := url.Values{"head_sha": {sha}, "event": {"push"}, "per_page": {"100"}}
	if err := g.call(ctx, http.MethodGet, "actions/workflows/"+Workflow+"/runs?"+query.Encode(), nil, &page); err != nil {
		if refused := (*apiError)(nil); errors.As(err, &refused) && refused.code == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	runs := make([]Run, len(page.Runs))
	for i, r := range page.Runs {
		runs[i] = Run{ID: r.ID, Status: r.Status, Conclusion: r.Conclusion}
	}
	return runs, nil
}

// Rerun reruns a completed run.
func (g *gitHub) Rerun(ctx context.Context, id int64) error {
	return g.call(ctx, http.MethodPost, "actions/runs/"+strconv.FormatInt(id, 10)+"/rerun", nil, nil)
}

// IssueOpen reports whether issue n exists and is open; a pull request, which the issues endpoint returns too, isn't
// an issue.
func (g *gitHub) IssueOpen(ctx context.Context, n int) (bool, error) {
	var issue struct {
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	err := g.call(ctx, http.MethodGet, "issues/"+strconv.Itoa(n), nil, &issue)
	var refused *apiError
	if errors.As(err, &refused) && (refused.code == http.StatusNotFound || refused.code == http.StatusGone) {
		return false, nil
	}
	return err == nil && issue.State == "open" && issue.PullRequest == nil, err
}

// apiError is a response GitHub refused a request with.
type apiError struct {
	method, path string
	code         int
	message      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub %s %s: %d %s", e.method, e.path, e.code, e.message)
}

// call sends method to path, under the repository's API, with in as its JSON body when not nil, and decodes the
// response into out when not nil. A request GitHub's rate limit refuses waits as long as GitHub says, its retry-after,
// else until x-ratelimit-reset once x-ratelimit-remaining is 0, and is sent again.
func (g *gitHub) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, g.api+"/repos/"+g.repo+"/"+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if wait, ok := rateLimited(resp, time.Now()); ok && attempt < attempts {
			fmt.Fprintf(os.Stderr, "land: GitHub's rate limit refused %s %s: waiting %s\n", method, path, wait)
			if err := g.wait(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			var refusal struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &refusal)
			return &apiError{method, path, resp.StatusCode, cmp.Or(refusal.Message, string(bytes.TrimSpace(data)))}
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		return json.Unmarshal(data, out)
	}
}

// rateLimited is how long a response GitHub's rate limit refused asks to wait before the request is sent again: its
// retry-after, else until its x-ratelimit-reset when x-ratelimit-remaining is 0. Any other response isn't one.
func rateLimited(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(max(seconds, 0)) * time.Second, true
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0, false
	}
	// A second past the reset, so a clock a little behind GitHub's doesn't send the request early.
	return max(time.Unix(reset, 0).Sub(now), 0) + time.Second, true
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
