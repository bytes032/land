# land

The language of landing an agent's work on a repository's trunk: one checked commit at a time, with no lock and no pull request.

## Landing

**Landing**:
Putting one agent's finished work on the trunk as a single commit, checked at the exact tree that is pushed.
_Avoid_: merge, PR, ship

**Trunk**:
The one branch every landing moves forward, never backward.
_Avoid_: main, master (those are a repository's names for it)

**Lost race**:
A landing whose push is refused because the trunk moved while it was being checked.

**Unchecked landing**:
A landing pushed without its own check, because the failure it hit was already on the trunk or is a known flaky test.
_Avoid_: skip, force

## Checking

**Box**:
A remote test machine claimed by one worktree, where every build and check of that worktree runs.
_Avoid_: runner, VM, sandbox

**Check at the push**:
The run of a repository's checks on the box, at the tree of the commit being landed.
_Avoid_: CI check, verification

**Stage**:
A fixed point in a landing or a box's life (setup, prepare, regenerate, check, warm, review) at which a repository's commands run, with a fixed meaning when they fail.
_Avoid_: hook, step, phase

**Spare**:
A released box kept warm for the rest of the hour already paid for it, then deleted by the box itself.

## Verdicts

**Mark**:
The commit status a landing sets on its commit: success, failure, error or pending.
_Avoid_: status, check result

**Failure**:
A mark meaning a check ran at the commit's tree and found something wrong, or the repository's own setup failed twice at that commit.

**Error**:
A mark meaning no verdict was reached, because the box or its setup failed before or around the check.
_Avoid_: infra failure, flake

**Verified**:
A commit whose own check at the push passed; an unchecked landing is never verified.

**Green commit**:
The newest trunk commit whose own check passed, where new work branches and boxes warm; an unchecked landing never is one.
_Avoid_: last passing CI run
