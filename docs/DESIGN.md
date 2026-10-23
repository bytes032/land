# land: design

land lands an agent's work on a repository's trunk: one commit at a time, checked on the agent's own test box at the exact tree it pushes, pushed with no lock and no pull request, and marked with a commit status that CI copies. Every repository that does this had rebuilt the same machinery by hand, and fixes made in one never reached the others; land is that machinery in one versioned package. Terms are defined in [`GLOSSARY.md`](../GLOSSARY.md); the rules agents follow are in [`internal/docs/LANDING.md`](../internal/docs/LANDING.md).

## The protocol (fixed; a repository picks only the push's order)

- **One commit per landing,** squashed and rebased with a merge that writes no worktree files; the trunk is fetched into the worktree's own ref.
- **The check at the push runs at the exact tree pushed.** The box compares its tree with the commit's before running any check.
- **Fast-forward only, never forced, no lock.** `land pre-push` refuses deleting the trunk or moving it backward.
- **Check, then push, or push, then check** (`push:`): `after-check`, the default, checks the commit before the push; `first` pushes it at once, regenerated, marked `pending`, then checks and marks it as after a lost race. Push first trades a red commit on the trunk now and then for nobody waiting on the check to see the commit; new work still branches from the green commit.
- **A lost race pushes first and checks after:** the rebased commit is pushed with the mark `pending`, checked, then marked. Derived-file conflicts take the trunk's side and are regenerated; any other conflict stops land.
- **One CI run per commit, never cancelled.** The commit's CI run is the push run of land's own workflow, `.github/workflows/land.yml`, which holds the gate and has no `concurrency:` group (a group cancels waiting runs); the repository's other workflows are its own, and land never reruns them. When land or `recheck` replaces a `pending`, missing or `error` mark with a final one, it reruns that run if it already completed.
- **No reruns of a failing check, no reverts:** fix forward.
- **Every landing carries `Agent-Session` and `Agent`,** plus `Unchecked` on an unchecked landing. land writes them; the author never does.

## Stages: the only extension seam

A repository extends land only by attaching commands to fixed stages in `.land.yaml`. The core owns where each runs, when, what it gets and what a failure means; it never names a repository's commands, paths or tools.

| Stage | Host | Runs when | Gets | A failure means |
|---|---|---|---|---|
| `setup` | a fresh Ubuntu box | building a snapshot | — | no snapshot |
| `prepare` | the box, before every command | always | — | `error`; repeated at the same commit: `failure` |
| `regenerate` | the box, during a landing | per entry and kind | — | land stops; nothing pushed or marked |
| `check` | the box, during a landing | the change classifier selects it | — | `failure` |
| `warm` | the box, after release | at the green commit, else the trunk's tip | — | the box isn't warm; no new snapshot |
| `review` | the Mac, before land | no finished review for the change's key | the base, an output directory, `land review`'s arguments | no record; land refuses |

Every command runs under `bash -eo pipefail -c` at the repository root; on the Mac the running binary's directory comes first on `PATH`, so `land …` inside a stage is the same pinned version. Around every box command the core's script enters the build root, sets `box.env`, turns apt's daily timers off, removes files the sync no longer lists, restores file times, runs `prepare`, runs the command, records the tree and file times, and moves a held box's deadline on. A failed step that keeps the command's tree right stops it; restored file times only keep builds incremental, so failing to restore them is a warning and the command still runs. Each record commits the tree as the box repository's `HEAD`, a commit of land's own with no parent, so a command that reads `HEAD` finds one and `git status` finds nothing to commit. Tool installs belong in `prepare`, usually the repository's version manager.

```yaml
version: 0.1.0
trunk: main
push: after-check                           # after-check | first
message: {require: ['^Closes #\d+$']}       # each pattern matched line by line
gate: {timeout: 30m, unverified: fail}      # unverified: fail | skip
box: {type: cpx42, env: {}}
stages:
  setup: <command>
  prepare: <command>
  check: {lint: {run: <command>}, test: {run: <command>, ignore: [.md]}}
  regenerate: [{kind: lockfile, path: <path>, run: <command>}, {kind: derived, path: <path>, run: <command>}]
  warm: <command>
  review: land autoreview "$@" --max-priority P1
```

The repository's name is `<owner>.<name>` from its `origin` URL; it names the state directory, the build root on the box and the snapshot label. The mark's commit status context is always `land`.

## Modules

Each is a deep module behind a small interface. A seam is real only with two adapters; Forge is the only one today.

- **Landing flow** (`internal/landing`): message and trailer checks, clean start, `LANDING.md` drift check, base, review check, squash and rebase with regeneration, check, push and mark, lost race, `recheck`, unchecked landings.
- **Change classifier** (`internal/classify`): from changed and conflicted paths, the checks to run, the regenerations (lockfiles first), the conflict policy, the review-key exclusions. Paths are prefixes and suffixes, never globs. A check with `paths` runs only when a changed path starts with one; one with `ignore` is skipped when every changed path ends with one of its suffixes. `lockfile` entries rerun when changed, stop land on conflict and stay in the review key; `derived` entries rerun when changed or conflicted, take the trunk's side and leave the key.
- **Check** (`internal/check`): the box half runs the selected checks at the commit's tree and writes `.land/result.json`; the Mac half classifies it: the tree matches and every check passed → `success`; the tree matches and one failed → `failure` with its name, command and log tail; anything else → `error` naming the stage. No repository output format reaches the core.
- **Verdict** (`internal/verdict`): the only interpreter of marks. A commit's mark is its newest `land` status from any account; it is verified when that is `success` and it has no `Unchecked:` trailer; the green commit is the newest verified commit among the trunk's 100 newest, else the tip. `land gate` polls it in CI and outputs `verified`.
- **Forge** (`internal/forge`): set and list marks, comment, list and rerun the commit's CI runs, check an issue is open. GitHub over plain HTTP (token from `GH_TOKEN`, else `gh auth token`; `github.token` in Actions), and an in-memory adapter for tests.
- **Boxes** (`internal/boxes`): see below.
- **Review ledger** (`internal/review`): a finished review is recorded under the key `git patch-id --verbatim` of `git diff-tree -p -U0 --no-renames` base..HEAD minus derived files, so a clean rebase keeps it and git configuration can't change it. Exit 0 from the `review` command means it finished, findings or not.
- **Bundled reviewer** (`internal/autoreview`): autoreview (openclaw/agent-skills, MIT) pinned per land release, run in branch mode; a change that edits an existing binary is reviewed as a temporary commit without it. It exits 0 when the helper exited 0 or 1 and wrote its report.
- **Provenance** (`internal/provenance`): writes and reads the trailers. omp (≥ 18.12.4) is the only identity source; land fails closed on an unknown session layout.

## Boxes

One crabbox box per worktree, on Hetzner, in crabbox's direct mode, in a project every repository can share.

- **A box is a folder.** Each box gets `<state>/boxes/<id>/`, made and recorded (as pending) before anything exists on Hetzner. Every crabbox call for the box runs with `XDG_STATE_HOME=<folder>/state` and `TMPDIR=<folder>/tmp`, so whatever crabbox keeps for it lives there. The server is created with `--slug land-<id>`, so a half-made box is findable by its label before land knows its lease.
- **A new box's image must have what land's box code uses.** Before setup does anything else on a new box, it checks for every command land's own scripts call (bash, perl with Time::HiRes, git, flock, ss, curl, tar, systemd 254's `systemd-run`, cloud-init, …), sudo without a password, a `/run/land` whose scripts bash runs, and Hetzner's metadata service; an image lacking any is refused, naming all it lacks, and the box is deleted. `go test -tags boximage ./internal/boxes/`, run before every release, runs land's box code on real boxes from crabbox's `os: ubuntu:24.04` and `ubuntu:26.04`.
- **Ending a box** is always the same: delete its server through Hetzner's API (by ID, or by slug for a pending box), delete its SSH keys by the fingerprints of the public keys in its folder, delete the folder. land never calls `crabbox stop`, `crabbox cp` or `crabbox list`: results come back with tar over the SSH command crabbox prints, and land lists servers itself, across every page.
- **The reaper,** run by every box command, ends boxes whose server is gone or powered off, boxes of removed worktrees, pending boxes whose creator died (their folder lock is free) and folders no record names.
- **No box outlives the hour it was last used.** Hetzner bills a server's lifetime rounded up to whole hours. A released box is a spare: it warms at the green commit and deletes itself 5 minutes before its next paid hour unless a claim takes it. A held box does the same: every box command moves its deadline to 5 minutes before the first of its paid hours to end at least 15 minutes later, and the box deletes itself then, unless something is connected to it (a command running, a tunnel open), which keeps it another hour. The box deletes itself with a token and a timer held in memory (`/run/land`); no snapshot carries them.
- **Snapshots:** new boxes boot the repository's newest available snapshot; `land boxes snapshot` builds one at the green commit (`--head`: at the worktree's HEAD commit, with its `.land.yaml`, for a repository's first snapshot, before its adoption lands); older ones go only once a newer one is available; a warm spare replaces a snapshot older than 4 hours.
- **Sweep:** `land boxes sweep`, hourly in CI, deletes land's servers (slug `land-…`) that are past their spare deadline, older than 25 hours, or powered off. Other tools' servers are never touched.
- **crabbox versions:** land refuses crabbox outside [0.78.1, 0.80.0). The range widens only after `go test -tags crabboxreal ./internal/boxes/` passes on the new version; that test runs the real crabbox under a macOS sandbox and fails if any of land's calls writes outside the box's folder.

## Facts the design rests on

- **Hetzner** bills each server's lifetime from its creation rounded up to whole hours, and a powered-off server bills like a running one. Observed: a server deleted 50 minutes after creation billed one hour.
- **crabbox's sync** deletes only files its previous sync delivered (its manifest lives in the workspace, so it rides along in a snapshot). Files a box got otherwise survive, hence the box script's prune.
- **crabbox writes outside its state root** for `cp` (journals and locks under the user's config and cache directories) and `stop` (an SSH session through the user's global `known_hosts`), never for `warmup`, `run` or `ssh`. It keeps an SSH control directory per lease under `/tmp` and a master connection open for 10 minutes after each use.
- **crabbox list** asks Hetzner for one page of 100 servers.

## Accepted shortcuts and risks

- A mark is the pusher's word: any account's newest `land` status counts, until a signed receipt can name the tested tree.
- Every box holds the project's Hetzner token for its self-delete; Hetzner tokens can't be scoped to one server.
- crabbox's per-lease `/tmp` control directory stays after a box ends (empty once its master exits; gone at reboot).
- An orphan SSH key can remain if land's state folder is deleted by hand before a crashed create is cleaned; keys cost nothing.
- Checks are chosen by path rules only; failure comments carry log tails, not test names; `--unchecked` searches the trunk's 100 newest commits.
