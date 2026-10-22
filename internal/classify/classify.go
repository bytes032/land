// Package classify is the change classifier: from the changed and conflicted paths, the checks to run, the
// regenerations in order, the conflict policy per path, and the review-key exclusions. Paths are prefixes and suffixes,
// never globs: a check's paths are plain prefixes, and a regenerate entry's path names a file or a directory as git's
// pathspecs do, so that the classifier and the review key, which git computes, agree on what is derived. Ticket #7.
package classify

import (
	"slices"
	"strings"

	"github.com/bytes032/land/internal/config"
)

// Policy is what land does with a conflicted path.
type Policy int

// Conflict policies.
const (
	Stop      Policy = iota // land stops
	TakeTrunk               // a derived file: take the trunk's side, then regenerate
)

// Checks are the checks that run for a change touching changed, in their configured order.
func Checks(stages config.Stages, changed []string) []config.Check {
	var run []config.Check
	for _, c := range stages.Check {
		if len(c.Paths) > 0 && !slices.ContainsFunc(changed, func(path string) bool { return prefixed(path, c.Paths) }) {
			continue
		}
		if len(c.Ignore) > 0 && len(changed) > 0 && !slices.ContainsFunc(changed, func(path string) bool {
			return !slices.ContainsFunc(c.Ignore, func(suffix string) bool { return strings.HasSuffix(path, suffix) })
		}) {
			continue
		}
		run = append(run, c)
	}
	return run
}

// Regenerations are the entries to rerun for changed and conflicted paths: lockfiles first, then derived files.
func Regenerations(stages config.Stages, changed, conflicted []string) []config.Regenerate {
	var lockfiles, derived []config.Regenerate
	for _, r := range stages.Regenerate {
		hit := func(path string) bool { return under(path, r.Path) }
		switch {
		case r.Kind == config.Lockfile && slices.ContainsFunc(changed, hit):
			lockfiles = append(lockfiles, r)
		case r.Kind == config.Derived && (slices.ContainsFunc(changed, hit) || slices.ContainsFunc(conflicted, hit)):
			derived = append(derived, r)
		}
	}
	return append(lockfiles, derived...)
}

// OnConflict is the policy for one conflicted path.
func OnConflict(stages config.Stages, path string) Policy {
	for _, r := range stages.Regenerate {
		if under(path, r.Path) {
			if r.Kind == config.Derived {
				return TakeTrunk
			}
			return Stop
		}
	}
	return Stop
}

// ReviewExclusions are the paths the review key leaves out: derived files only.
func ReviewExclusions(stages config.Stages) []string {
	var paths []string
	for _, r := range stages.Regenerate {
		if r.Kind == config.Derived {
			paths = append(paths, r.Path)
		}
	}
	return paths
}

// under reports whether path is entry, a regenerate entry's path, or lies in it, as git's literal pathspec matches:
// "gen" names the file gen and everything in the directory gen/; "gen/" names only what is in the directory gen/.
func under(path, entry string) bool {
	if strings.HasSuffix(entry, "/") {
		return strings.HasPrefix(path, entry)
	}
	return path == entry || strings.HasPrefix(path, entry+"/")
}

// prefixed reports whether path starts with one of prefixes, a check's paths.
func prefixed(path string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(path, prefix) })
}
