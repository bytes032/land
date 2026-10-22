// Package verdict is the only module that interprets marks: a commit's mark, whether it is verified, the green commit
// (the newest verified one), and CI's gate.
package verdict

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bytes032/land/internal/config"
	"github.com/bytes032/land/internal/forge"
	"github.com/bytes032/land/internal/proc"
	"github.com/bytes032/land/internal/provenance"
)

// Verdict is what a commit's marks mean.
type Verdict struct {
	Mark *forge.Mark // the newest mark, any account; nil when the commit has none
	// Verified is whether the commit's own check passed: its mark is success and it has no Unchecked trailer.
	Verified bool
}

// Of is sha's verdict; dir is a checkout holding sha (for its trailers).
func Of(ctx context.Context, f forge.Forge, dir, sha string) (Verdict, error) {
	marks, err := f.Marks(ctx, sha)
	if err != nil {
		return Verdict{}, fmt.Errorf("the marks of %s: %w", sha, err)
	}
	if len(marks) == 0 {
		return Verdict{}, nil
	}
	v := Verdict{Mark: &marks[0]}
	if v.Mark.State == forge.Success {
		unchecked, err := provenance.Trailer(ctx, dir, sha, provenance.Unchecked)
		if err != nil {
			return Verdict{}, fmt.Errorf("the trailers of %s: %w", sha, err)
		}
		v.Verified = unchecked == ""
	}
	return v, nil
}

// recent is how many of the trunk's newest commits are searched for a green one.
const recent = 100

// GreenCommit is the green commit: the newest verified commit among the 100 newest first-parent commits of ref, so
// never an unchecked landing, which no box checked. When none is verified it returns ref's commit with ok false.
func GreenCommit(ctx context.Context, f forge.Forge, dir, ref string) (sha string, ok bool, err error) {
	out, err := git(ctx, dir, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", recent), ref, "--")
	if err != nil {
		return "", false, err
	}
	commits := strings.Fields(out)
	if len(commits) == 0 {
		return "", false, fmt.Errorf("%s names no commit", ref)
	}
	for _, c := range commits {
		v, err := Of(ctx, f, dir, c)
		if err != nil {
			return "", false, err
		}
		if v.Verified {
			return c, true, nil
		}
	}
	return commits[0], false, nil
}

// Production seams, replaced in tests.
var (
	newForge = forge.FromEnv
	now      = time.Now
	sleep    = func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// GreenMain is `land green`: it prints the green commit of origin's trunk, or the trunk's tip when none is green.
func GreenMain(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: land green")
	}
	root, cfg, err := repository(ctx)
	if err != nil {
		return err
	}
	ref := "refs/remotes/origin/" + cfg.Trunk
	if _, err := git(ctx, root, "fetch", "--quiet", "origin", "+refs/heads/"+cfg.Trunk+":"+ref); err != nil {
		return err
	}
	f, err := newForge(ctx, root)
	if err != nil {
		return err
	}
	sha, ok, err := GreenCommit(ctx, f, root, ref)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "no green commit among %s's %d newest; its tip:\n", cfg.Trunk, recent)
	}
	fmt.Println(sha)
	return nil
}

// poll is how often the gate reads the commit's marks.
const poll = 15 * time.Second

// GateMain is `land gate`, CI's one job, in GitHub Actions: it copies the mark of GITHUB_SHA into the run, polling
// every 15 s up to the configured timeout. success passes and writes verified=true|false to GITHUB_OUTPUT; failure
// fails with the mark's description; error, or no final mark by the timeout, follows the unverified policy: fail
// fails, skip passes with verified=false.
func GateMain(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: land gate")
	}
	sha, output := os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_OUTPUT")
	if sha == "" || output == "" {
		return errors.New("land gate runs in GitHub Actions: GITHUB_SHA and GITHUB_OUTPUT must be set")
	}
	root, cfg, err := repository(ctx)
	if err != nil {
		return err
	}
	f, err := newForge(ctx, root)
	if err != nil {
		return err
	}
	verified, err := gate(ctx, f, root, sha, cfg.Gate)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "verified=%t\n", verified); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// gate waits for sha's final mark and returns whether the run passes verified; an error fails the run.
func gate(ctx context.Context, f forge.Forge, dir, sha string, policy config.Gate) (bool, error) {
	deadline := now().Add(policy.Timeout.Duration)
	for {
		v, err := Of(ctx, f, dir, sha)
		switch {
		case err != nil: // read again at the next poll, so one API error doesn't decide the run
			fmt.Fprintln(os.Stderr, err)
		case v.Mark == nil:
			fmt.Printf("%s has no mark yet\n", sha)
		case v.Mark.State == forge.Success:
			fmt.Printf("%s is marked success (verified: %t): %s\n", sha, v.Verified, v.Mark.Description)
			return v.Verified, nil
		case v.Mark.State == forge.Failure:
			return false, fmt.Errorf("%s is marked failure: %s", sha, v.Mark.Description)
		case v.Mark.State == forge.Error:
			return unverified(policy, fmt.Sprintf("%s is marked error: %s", sha, v.Mark.Description))
		default:
			fmt.Printf("%s is marked %s: %s\n", sha, v.Mark.State, v.Mark.Description)
		}
		if !now().Before(deadline) {
			return unverified(policy, fmt.Sprintf("%s has no final mark after %s; `land recheck %s` sets one", sha, policy.Timeout.Duration, sha))
		}
		if err := sleep(ctx, poll); err != nil {
			return false, err
		}
	}
}

// unverified applies the repository's policy to a commit with no verdict; any policy but skip fails closed.
func unverified(policy config.Gate, why string) (bool, error) {
	if policy.Unverified == "skip" {
		fmt.Println(why + "; unverified: skip passes it unverified")
		return false, nil
	}
	return false, errors.New(why)
}

// repository is the root of the checkout land runs in, and its configuration.
func repository(ctx context.Context) (string, *config.Config, error) {
	out, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, err
	}
	root := strings.TrimSpace(out)
	cfg, err := config.Load(root)
	return root, cfg, err
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	var stderr strings.Builder
	out, err := proc.Output(ctx, proc.Spec{Argv: append([]string{"git"}, args...), Dir: dir, Stderr: &stderr})
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
