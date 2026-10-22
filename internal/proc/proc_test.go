package proc

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startScript starts script under sh and returns the child and the first line the script prints.
func startScript(ctx context.Context, t *testing.T, script string, grace time.Duration) (*Child, string) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child, err := Start(ctx, Spec{Argv: []string{"sh", "-c", script}, Stdout: write, Grace: grace})
	_ = write.Close() // the child holds its own copy
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Stop() })
	line, err := bufio.NewReader(read).ReadString('\n')
	_ = read.Close()
	if err != nil {
		t.Fatal(err)
	}
	return child, strings.TrimSpace(line)
}

func alive(t *testing.T, pid string) bool {
	t.Helper()
	n, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatal(err)
	}
	return !errors.Is(syscall.Kill(n, 0), syscall.ESRCH)
}

func TestStopLeavesNoGrandchild(t *testing.T) {
	for name, script := range map[string]string{
		"leader running":      "sleep 60 >/dev/null & echo $!; wait",
		"leader exited first": "sleep 60 >/dev/null & echo $!",
	} {
		t.Run(name, func(t *testing.T) {
			child, grandchild := startScript(context.Background(), t, script, 0)
			if name == "leader exited first" {
				if exit, err := child.Wait(); err != nil || exit != (Exit{}) {
					t.Fatal(exit, err)
				}
			}
			if !alive(t, grandchild) {
				t.Fatal("the grandchild is gone before Stop")
			}
			if err := child.Stop(); err != nil {
				t.Fatal(err)
			}
			if alive(t, grandchild) {
				t.Fatal("the grandchild outlived Stop")
			}
		})
	}
}

func TestStopKillsAChildIgnoringSIGTERMAfterTheGrace(t *testing.T) {
	const grace = 300 * time.Millisecond
	child, _ := startScript(context.Background(), t, "trap '' TERM; echo ready; while :; do sleep 1; done", grace)
	start := time.Now()
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("stopped after %v, before the %v grace", elapsed, grace)
	}
	if exit, err := child.Wait(); err != nil || exit.String() != "signal: killed" {
		t.Fatal(exit, err)
	}
}

func TestCancellingTheContextStopsTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	child, grandchild := startScript(ctx, t, "sleep 60 >/dev/null & echo $!; wait", 0)
	cancel()
	select {
	case <-child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the leader outlived its context")
	}
	if err := child.Stop(); err != nil { // the cancellation's own Stop, once it finished
		t.Fatal(err)
	}
	if alive(t, grandchild) {
		t.Fatal("the grandchild outlived its context")
	}
	err := Run(ctx, Spec{Argv: []string{"true"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("started under a cancelled context: %v", err)
	}
}

func TestRunReportsTheExitAsProcessStateDoes(t *testing.T) {
	err := Run(context.Background(), Spec{Argv: []string{"sh", "-c", "exit 3"}})
	var exit Exit
	if !errors.As(err, &exit) || exit.Code != 3 || err.Error() != "sh: exit status 3" {
		t.Fatal(err)
	}
}

// A child that exits 0 on its cancellation's SIGTERM didn't finish: Run reports the cancellation with
// no Exit, so a caller that takes an Exit as the outcome doesn't read it as success.
func TestRunReportsACancelledChildThatExitsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := Run(ctx, Spec{Argv: []string{"sh", "-c", "trap 'exit 0' TERM; sleep 60 & wait"}})
	var exit Exit
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &exit) {
		t.Fatal(err)
	}
}

// A grandchild holding the captured stdout doesn't hold Output: the wait for the output ends a grace
// after the leader exits, the group is stopped, and what the leader printed comes back.
func TestOutputReturnsWhileAGrandchildHoldsItsStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // bounds a wait that never ends
	defer cancel()
	start := time.Now()
	out, err := Output(ctx, Spec{Argv: []string{"sh", "-c", "sleep 60 & echo printed"}, Grace: 100 * time.Millisecond})
	if out != "printed\n" || !errors.Is(err, exec.ErrWaitDelay) || time.Since(start) > 5*time.Second {
		t.Fatalf("%q after %v: %v", out, time.Since(start), err)
	}
}
