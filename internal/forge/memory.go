package forge

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Memory is an in-memory Forge for tests. Its zero value is ready; Creator names the account its marks are set by.
type Memory struct {
	Creator string

	mu       sync.Mutex
	clock    time.Time
	marks    map[string][]Mark // oldest first
	comments map[string][]string
	runs     map[string][]Run // oldest first
	reruns   []int64
	issues   map[int]bool // open or closed
	nextRun  int64
}

func (m *Memory) init() {
	if m.marks == nil {
		m.marks, m.comments, m.runs, m.issues = map[string][]Mark{}, map[string][]string{}, map[string][]Run{}, map[int]bool{}
		m.clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
}

// SetMark records m, stamped one second after the previous mark, so newer marks always sort first.
func (m *Memory) SetMark(_ context.Context, sha string, mark Mark) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.clock = m.clock.Add(time.Second)
	mark.CreatedAt = m.clock
	if mark.Creator == "" {
		mark.Creator = m.Creator
	}
	m.marks[sha] = append(m.marks[sha], mark)
	return nil
}

// Marks returns sha's marks, newest first.
func (m *Memory) Marks(_ context.Context, sha string) ([]Mark, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	marks := slices.Clone(m.marks[sha])
	slices.Reverse(marks)
	return marks, nil
}

// Comment records a commit comment.
func (m *Memory) Comment(_ context.Context, sha, body string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.comments[sha] = append(m.comments[sha], body)
	return fmt.Sprintf("memory://comments/%s/%d", sha, len(m.comments[sha])), nil
}

// Comments returns sha's comments, oldest first.
func (m *Memory) Comments(sha string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return slices.Clone(m.comments[sha])
}

// AddRun records a push run of the CI workflow for sha and returns its id.
func (m *Memory) AddRun(sha, status, conclusion string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.nextRun++
	m.runs[sha] = append(m.runs[sha], Run{ID: m.nextRun, Status: status, Conclusion: conclusion})
	return m.nextRun
}

// Runs returns sha's runs, newest first.
func (m *Memory) Runs(_ context.Context, sha string) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	runs := slices.Clone(m.runs[sha])
	slices.Reverse(runs)
	return runs, nil
}

// Rerun records the rerun and sets the run back to queued; it refuses a run that hasn't completed, as GitHub does.
func (m *Memory) Rerun(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for sha, runs := range m.runs {
		for i := range runs {
			if runs[i].ID == id {
				if runs[i].Status != "completed" {
					return fmt.Errorf("run %d is %s, not completed", id, runs[i].Status)
				}
				m.runs[sha][i] = Run{ID: id, Status: "queued"}
				m.reruns = append(m.reruns, id)
				return nil
			}
		}
	}
	return fmt.Errorf("no run %d", id)
}

// Reruns returns the ids of rerun runs, in order.
func (m *Memory) Reruns() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.reruns)
}

// SetIssue records issue n as open or closed.
func (m *Memory) SetIssue(n int, open bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.issues[n] = open
}

// IssueOpen reports whether issue n exists and is open.
func (m *Memory) IssueOpen(_ context.Context, n int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return m.issues[n], nil
}
