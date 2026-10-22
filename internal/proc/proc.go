// Package proc starts, stops and describes the tools' child processes. Every child leads its own
// session and so its own process group, and stopping a child stops the whole group, such as the
// binary a `go run` child started.
package proc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultGrace = 10 * time.Second
	// killWait bounds the wait for the group to vanish after SIGKILL.
	killWait = 5 * time.Second
	poll     = 20 * time.Millisecond
)

// Spec is a child to start. A nil Env inherits the tool's environment; a nil Stdin, Stdout or
// Stderr is the null device.
type Spec struct {
	Argv   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Grace is how long Stop waits after SIGTERM before SIGKILL, and how long a grandchild may hold
	// the leader's output pipes once the leader exited; zero means 10 s.
	Grace time.Duration
}

// Child is a started child, the leader of its own process group.
type Child struct {
	cmd      *exec.Cmd
	grace    time.Duration
	done     chan struct{}
	exit     Exit
	ioErr    error
	stopOnce sync.Once
	stopped  chan struct{}
	stopErr  error
}

// Start starts spec as the leader of a new session. Cancelling ctx stops the child as Stop does; a
// child started under a context that never ends outlives the tool unless the tool stops it.
func Start(ctx context.Context, spec Spec) (*Child, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("proc: empty command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	grace := cmp.Or(spec.Grace, defaultGrace)
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A grandchild still holding the output pipes a grace after the leader exited loses them, and
	// Wait returns exec.ErrWaitDelay rather than waiting for the grandchild.
	cmd.WaitDelay = grace
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Child{cmd: cmd, grace: grace, done: make(chan struct{}), stopped: make(chan struct{})}
	go c.reap()
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = c.Stop()
			case <-c.stopped:
			}
		}()
	}
	return c, nil
}

func (c *Child) reap() {
	err := c.cmd.Wait()
	c.exit = exitOf(c.cmd.ProcessState)
	if exitErr := (*exec.ExitError)(nil); !errors.As(err, &exitErr) {
		c.ioErr = err
	}
	close(c.done)
}

// Done is closed once the leader has exited and its output is copied, or the grace after it exited
// has passed. It says nothing about the rest of the group, which may live on until Stop.
func (c *Child) Done() <-chan struct{} { return c.done }

// Wait waits for the leader, not its group, and returns how it ended. Its error reports only a
// failure to copy the leader's input or output, exec.ErrWaitDelay when a grandchild held it.
func (c *Child) Wait() (Exit, error) {
	<-c.done
	return c.exit, c.ioErr
}

// Pid is the leader's process ID, which is also the group's ID.
func (c *Child) Pid() int { return c.cmd.Process.Pid }

// Stop stops the child's process group, even after its leader exited: SIGTERM, then SIGKILL after
// the grace period, then a bounded wait until kill(-pgid, 0) finds nothing. It returns nil only once
// the group is gone, so after an error a member may still run; later calls return the first result.
func (c *Child) Stop() error {
	c.stopOnce.Do(func() {
		c.stopErr = c.terminate()
		close(c.stopped)
	})
	return c.stopErr
}

func (c *Child) terminate() error {
	group := -c.Pid()
	termErr := signalGroup(group, syscall.SIGTERM)
	if gone(group, c.grace) {
		return nil
	}
	killErr := signalGroup(group, syscall.SIGKILL)
	if gone(group, killWait) {
		return nil
	}
	return errors.Join(termErr, killErr, fmt.Errorf("process group %d outlived SIGKILL", -group))
}

// signalGroup signals every process in the group; a group with none left is no error.
func signalGroup(group int, signal syscall.Signal) error {
	if err := syscall.Kill(group, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%s process group %d: %w", signal, -group, err)
	}
	return nil
}

// gone polls until kill(group, 0) finds no process, or wait elapses.
func gone(group int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for !errors.Is(syscall.Kill(group, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

// Run starts spec, waits for its leader, then stops its group, so nothing the child started
// outlives it. An exit other than 0 is an error wrapping the Exit. A run ctx cancelled is an error
// wrapping ctx's cause, and the Exit only when it isn't 0: a child that exits 0 on the
// cancellation's SIGTERM didn't finish.
func Run(ctx context.Context, spec Spec) error {
	c, err := Start(ctx, spec)
	if err != nil {
		return err
	}
	exit, waitErr := c.Wait()
	cancelled := ctx.Err() != nil // before Stop, whose own wait may outlast a deadline
	if err := errors.Join(waitErr, c.Stop()); err != nil {
		return fmt.Errorf("%s: %w", spec.Argv[0], err)
	}
	switch {
	case cancelled && exit == (Exit{}):
		return fmt.Errorf("%s: %w", spec.Argv[0], context.Cause(ctx))
	case exit == (Exit{}):
		return nil
	case cancelled:
		return fmt.Errorf("%s: %w: %w", spec.Argv[0], exit, context.Cause(ctx))
	default:
		return fmt.Errorf("%s: %w", spec.Argv[0], exit)
	}
}

// Output runs spec as Run does and returns its stdout, which spec must leave unset.
func Output(ctx context.Context, spec Spec) (string, error) {
	if spec.Stdout != nil {
		return "", errors.New("proc: Output collects stdout itself")
	}
	var stdout strings.Builder
	spec.Stdout = &stdout
	err := Run(ctx, spec)
	return stdout.String(), err
}

// Exit is how a leader ended: its exit code, or the signal that killed it. It prints as
// os.ProcessState does, and a non-zero Exit is the error Run returns.
type Exit struct {
	Code   int            // the exit code; -1 when a signal ended the leader
	Signal syscall.Signal // the signal that ended the leader, or 0
}

func exitOf(state *os.ProcessState) Exit {
	if state == nil {
		return Exit{Code: -1}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return Exit{Code: -1, Signal: status.Signal()}
	}
	return Exit{Code: state.ExitCode()}
}

func (e Exit) String() string {
	if e.Signal != 0 {
		return "signal: " + e.Signal.String()
	}
	return "exit status " + strconv.Itoa(e.Code)
}

func (e Exit) Error() string { return e.String() }
