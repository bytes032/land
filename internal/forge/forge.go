// Package forge is land's one seam to GitHub: marks (commit statuses in the fixed context "land"), commit comments,
// the commit's CI runs, reruns and issues. GitHub is one adapter; Memory is the other, for tests.
package forge

import (
	"context"
	"time"
)

// Context is the commit status context of every mark.
const Context = "land"

// Workflow is the workflow file whose push run is "the commit's CI run": land's own, holding the gate job and whatever
// the repository wants to run after it. land reruns it after setting a final mark, so the repository's other workflows
// (image builds, say) are never rerun by land.
const Workflow = "land.yml"

// State is a mark's state.
type State string

// Mark states.
const (
	Pending State = "pending"
	Success State = "success"
	Failure State = "failure"
	Error   State = "error"
)

// Mark is one commit status in the "land" context.
type Mark struct {
	State       State
	Description string
	TargetURL   string
	Creator     string // the account that set it
	CreatedAt   time.Time
}

// Run is one push run of the CI workflow.
type Run struct {
	ID         int64
	Status     string // queued, in_progress or completed
	Conclusion string // set once completed: success, failure, cancelled, …
}

// Forge is what land needs from GitHub.
type Forge interface {
	// SetMark sets a mark on the commit sha.
	SetMark(ctx context.Context, sha string, m Mark) error
	// Marks are the commit's marks, newest first.
	Marks(ctx context.Context, sha string) ([]Mark, error)
	// Comment posts a commit comment and returns its URL.
	Comment(ctx context.Context, sha, body string) (string, error)
	// Runs are the commit's push runs of Workflow, newest first.
	Runs(ctx context.Context, sha string) ([]Run, error)
	// Rerun reruns a completed run.
	Rerun(ctx context.Context, id int64) error
	// IssueOpen reports whether issue n exists and is open.
	IssueOpen(ctx context.Context, n int) (bool, error)
}
