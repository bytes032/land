# Landing

How work lands on this repository's trunk. `land` below is the repository's land shim, which runs the land version
`.land.yaml` pins. The repository's own commands (its checks, setup, generators and reviewer) live in `.land.yaml`;
this file names none of them. land writes this file (`land docs`) and refuses to land while it differs from the pinned
version's, so don't edit it: a version bump brings the new one.

A landing is one commit on the trunk, checked on your box at the exact tree that is pushed, pushed with no lock,
fast-forward only, and marked. The mark is the commit status `land`: `success`, `failure`, `error` or `pending`. CI
checks nothing; it copies the mark.

## Branch

One worktree per task. Branch from the green commit, the newest trunk commit whose own check passed (an unchecked
landing never is one): `land green` prints it, or the trunk's tip when none of its 100 newest commits is green.

## Build and test on the box

Your box runs every build and test; your Mac only edits files and runs git. With `HCLOUD_TOKEN` in your environment:

- `land boxes run -- CMD` runs CMD at the box's build root and exits with its exit code. The first box command claims
  the worktree's box. Each command syncs the worktree, uncommitted changes included, removes files the worktree no
  longer has, restores file times and runs the repository's `prepare` stage before CMD; a failed `prepare` stops it. If
  the file times can't be restored, the command still runs, after a warning that the build may redo work.
- `land boxes status` names your box's lease and prints the crabbox commands that forward a port of the box to your
  Mac or copy a file back from it. Copy them from there and fill in PORT or PATH. They run crabbox with your box's own
  state (`XDG_STATE_HOME`), without which crabbox doesn't know the box, and with `CRABBOX_MAX_TTL=24h`, without which
  crabbox cuts the box's 24-hour lease to its own 4-hour default and the box shuts down under you. They look like this:
  ```sh
  XDG_STATE_HOME='<box folder>/state' CRABBOX_MAX_TTL=24h crabbox tunnel --provider hetzner --id LEASE --local-port PORT PORT
  XDG_STATE_HOME='<box folder>/state' CRABBOX_MAX_TTL=24h crabbox cp --provider hetzner --id LEASE SANDBOX:/work/<owner>.<name>/PATH PATH
  ```
- `land boxes claim --type TYPE` asks for another server type, only while the worktree holds no box or one of that
  type; `land boxes release` gives the box up. A released box stays as a spare, warm at the green commit, for the rest
  of the hour already paid for, then deletes itself; a later claim may take it, with at least an hour of the box's
  24-hour lease left.
- A box you hold but stop using deletes itself the same way: 5 minutes before a paid hour in which it ran no box
  command, unless something is connected to it then (a command still running, a tunnel open), which keeps it another
  hour. Your next box command after that claims a spare or a new box, with cold build caches.
- New boxes boot the repository's newest snapshot. With none, the claim stops and names `land boxes snapshot`, which
  builds one at the green commit, with that commit's `.land.yaml`, whatever your worktree holds. `--head` builds it at
  your HEAD commit instead, with its `.land.yaml`: for the repository's first snapshot, before its adoption lands.
- land checks that a new box's image (the snapshot, or, for `land boxes snapshot`, the Ubuntu that `os:` in
  `.crabbox.yaml` names) has what land's box code needs, and refuses one that lacks any: it names all it lacks and
  deletes the box. Each land release is tested on crabbox's Ubuntu 24.04 and 26.04.
- At Hetzner's server limit, a claim says once who holds the repository's boxes, then retries every 30 s for up to 3 h.

## Ready to land

Commit your work: land lands commits, and refuses uncommitted changes and untracked files.

**The review.** `land review [ARGS…]` runs the repository's reviewer over your committed change, from where it left
the trunk to HEAD, and records it once the reviewer finishes; it prints where the report is. Pass it your task: with
the bundled reviewer, `land review --prompt-file FILE`, FILE inside your worktree (remove it before you land, which
refuses untracked files). A review that finished with findings counts as finished. The record is keyed by the change
itself, so a clean rebase keeps it and any edit to the change needs a new one; derived files are left out of the key.
`land review check` says whether the change has one.

**The findings rule.** For each finding at or above the threshold, verify it against the code and task; fix it if
likely or rare but dangerous (data loss, security, money), with a regression test; refute it with evidence if wrong;
decline it if real but unlikely, recoverable and costlier to fix than the risk, with a one-line reason (a finding that
only adds restrictions with no concrete harm is declined the same way); record refutations and declines in the commit
message's `Review:` paragraph. A fix needs a new review; a refutation or decline doesn't; never rerun a review for a
nicer verdict.

## Land

Write the commit message to a file, then run `land --message FILE`.

- The message holds every line `message.require` in `.land.yaml` asks for, each matched line by line, and your
  `Review:` paragraph. Write no `Agent-Session:`, `Agent:` or `Unchecked:` line: land adds them as the last paragraph,
  from your omp session (omp 18.12.4 or later), and refuses a message that has them or a session it can't read.
- land refuses a change with no finished review, and a `LANDING.md` that isn't this version's.
- It fetches the trunk into a ref of your worktree's own, squashes your work into one commit and rebases it onto the
  trunk with a merge that writes no file into your worktree, so a conflict never leaves it half-merged.
- From here on, land follows the landing commit's own `.land.yaml`, which the trunk may have changed since you
  branched. Lockfiles your change changes, and derived files it changes or that conflicted, are regenerated on the box
  and amended into the commit.
- The check at the push runs on your box at the commit's exact tree: the checks your change selects (a check with
  `paths` runs only when a changed path starts with one; a check with `ignore` is skipped when every changed path ends
  with one). The box stops before any check when its tree isn't the commit's. Its result and each check's log come
  back to `.land/` in your worktree.
- When it passes, land pushes the commit and marks it `success`, and your worktree ends at it. A failure, or a box that
  stopped before a verdict, pushes nothing and leaves your worktree at the squashed commit, before any regeneration:
  fix a failure, or after the box just land again, which needs no new review.
- **The order,** `push:` in `.land.yaml`: `after-check`, the default, pushes only once the check passed, as above.
  `first` pushes the regenerated commit at once, marked `pending`, then checks it and sets the final mark, as after a
  lost race: the trunk is red now and then, but nobody waits on the check to see your commit, and new work still
  branches from the green commit.
- **A lost race:** the trunk moved while you landed, so the push is refused. land rebases onto it and pushes again at
  once, marked `pending`, then checks the pushed commit and sets the final mark. When derived files conflicted, they
  are regenerated and, under `after-check`, checked before the push instead. Any other conflict stops land (see
  Conflicts).
- **The marks:** `success`, the check passed. `failure`, a check failed at this commit: a commit comment, linked from
  the mark, lists each failed check's name, command, exit and the end of its log. `error`, no verdict: the box, the
  connection, Hetzner, crabbox or `prepare` failed, and the mark names where; it never blames the commit. `pending`,
  the check after the push is running.

## CI

One CI run per commit, never cancelled, and it checks nothing: the gate, `land gate`, waits up to `gate.timeout` for
the commit's mark. It lives in its own workflow, `.github/workflows/land.yml`, run on every push to the trunk, with no
`concurrency:` group: runs of different commits go in parallel, and a group would cancel a waiting run, leaving that
commit without a verdict. `success` passes; `failure` fails; `error`, or no final mark by then, fails under
`gate.unverified: fail` and passes under `skip`. The gate outputs `verified`, true only for `success` on a commit that
isn't an unchecked landing; a deploy reads it in a job after the gate in `land.yml`, and serializes itself if it must.
When land or `land recheck` sets a final mark after the commit's `land.yml` run completed, it reruns that run, so CI
agrees with the mark; land never reruns the repository's other workflows.

## Stay with the commit

A landing isn't done until its mark is final. While it is `pending`, missing or `error` (your session or box died, or
the box failed), run `land recheck SHA`, from any worktree: it checks the commit in a worktree of its own, with that
commit's `.land.yaml`, and sets the final mark. It refuses `success` and `failure`: nothing is rechecked until it
passes. A `prepare` error repeated at the same commit becomes `failure`, since the commit broke the repository's setup;
every other repeated error stays `error`.

## A failure your change didn't cause

When a check fails the same way at the trunk without your change, don't fix it inside your landing:

- `land --message FILE --unchecked`: the trunk is red already. land names its first red commit since the green commit
  in your commit's `Unchecked:` trailer and marks your commit `failure`, inheriting that failure; it refuses when no
  commit since the green one is marked `failure`.
- `land --message FILE --unchecked '#N'`: a flaky test that open issue #N tracks. Your commit gets `Unchecked: #N` and
  is marked `success`; land refuses an issue that isn't open.

Neither runs the check, so neither is ever verified or deployed; both still need a finished review.

## A red trunk

A red trunk is fixed forward from its first red commit, the oldest marked `failure` since the green commit; an
unchecked landing's `Unchecked:` trailer names it. Its commit comment lists what failed, and its `Agent` and
`Agent-Session` trailers name the agent that landed it and its root omp session, whose transcript omp's archive finds
by that id. That agent, or a new one given its context, fixes it in a new landing: no reverts.

## Conflicts

Derived files never need a hand merge: land takes the trunk's side and regenerates them. Any other conflict, a
lockfile's included, stops land with nothing pushed and your worktree as it was. Merge the trunk into your branch,
resolve (regenerate a lockfile on your box rather than merge it by hand), run `land review check` and review again only
if it fails, then land again.

## Parallel work

Agents land side by side with no lock and no queue: each worktree fetches the trunk into its own ref and holds its own
box, and a lost race only rebases and pushes again. Never force-push the trunk: the pre-push hook, `land pre-push`,
refuses deleting it or moving it backward.
