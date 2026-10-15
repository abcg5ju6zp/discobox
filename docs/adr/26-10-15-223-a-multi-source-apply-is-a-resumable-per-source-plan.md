# 26-10-15-223 — A multi-source apply is a resumable per-source plan

- **Status**: Accepted
- **Date**: 2026-10-15
- **Supersedes**: [0014](0014-disco-apply-pulls-sandbox-commits-via-cherry-pick.md)
  §3 point 4 — on conflict the scratch worktree is aborted and the user is
  handed a manual command — and its Consequences bullet "On conflict,
  `disco apply` hands back a command rather than a live conflict state".
  Amends 0014 §4: the server record gains idempotent confirmation and the
  client gains a durable per-run plan. Everything else in 0014 stands: the
  fetch transport and `refs/discobox/apply/<id>/<slug>` ref, merge-base base
  selection, cherry-pick-never-merge, the sandbox dirty-tree check, all
  sources matched by host, and `AppliedCommits` as an append-only audit trail.

## Context

0014 made one apply attempt all-or-nothing *per source*: the cherry-pick ran
in a disposable worktree, conflict aborted and deleted it, and
`AppliedCommits` was written only after the host fast-forward had landed. That
keeps the server record truthful, but a discobox can carry several sources,
and apply walks them one after another in one process with no memory of the
run. Three failures follow from that:

1. **Part of a multi-source run cannot be resumed.** Source A lands and is
   recorded, source B conflicts, the process exits. The run intended — source
   order, each source's base and tip, its target directory and the local HEAD
   it started from — was held only in local variables and is gone.
2. **Re-running redoes the work.** If the process died, or the
   complete-apply call failed, *after* the fast-forward but *before* the
   record existed, the next apply has no last-applied cursor, derives the
   full range from the merge base again, and cherry-picks commits that are
   already on the branch — conflicts or empty commits over work that genuinely
   landed. The user's own new local commits can then be mistaken for the
   discobox's result, because nothing records which host commits this run
   produced.
3. **A retried confirmation duplicates the audit trail.** complete-apply
   appends unconditionally; a response lost in flight is indistinguishable
   from a request that never arrived, so the safe retry writes the same fact
   twice.

0014 rejected leaving a live `CHERRY_PICK_HEAD` in the user's own checkout
because a multi-commit range can conflict after earlier commits landed,
putting the host branch in a genuinely partial state with no honest way to
record it. That reasoning still holds for the host branch — it is why the
host branch is still never touched until the full range is clean. What it did
not consider was keeping the attempt's *worktree* alive somewhere other than
the host checkout, which gives the user a live conflict to resolve without
giving up the all-or-nothing landing.

## Decision

### 1. Every apply run is a plan file in the CLI state directory

Before any local mutation — after the read-only inspection 0014 already
implies (sandbox metadata, host-directory resolution, sandbox dirty check,
tip fetch, base and range resolution) — apply writes a private JSON plan to
the CLI state directory: `cli/apply/<sandbox-id>/plan.json` under
`cliStateDir()` (`XDG_STATE_HOME/discobox/cli`), written atomically with the
existing `writeStateFile` convention (temp file, `0600`, rename).

The plan records, for the whole run:

- server / project / sandbox identity;
- the selection fingerprint: `--source` slug, every `--dir` override,
  `--allow-dirty`;
- the sources in their application order; per source: slug, resolved host
  repository root and how it was chosen (override or sandbox origin), branch,
  local HEAD at planning time, landing strategy (cherry-pick, or the unborn
  root path), base with its origin, sandbox tip, and the commits in range.

Each source advances through `planned → fetched → landed → confirmed` and
every transition is persisted *before* the next action begins. A conflict is
a retained state (§2), up-to-date is a terminal short circuit. An advisory
lock beside the plan (an OS file lock on a lock file) makes two applies of
the same sandbox on one host mutually exclusive; the lock releases with the
process, so a killed apply cannot deadlock the next one. When every selected
source has reached a verified terminal state the plan file is deleted.

### 2. On conflict the worktree is kept, and the run stops

The cherry-pick still never touches the host branch until the whole range is
clean. What changes is where the attempt lives: a linked worktree under the
state directory for *this* plan, not a `MkdirTemp` path that is torn down on
any failure.

- On conflict the cherry-pick is **not aborted and the worktree is not
  removed**. `CHERRY_PICK_HEAD` and the unmerged index stay exactly where the
  user can work on them, in a normal working tree: edit, `git add`,
  `git cherry-pick --continue`. The host repository — branch, working tree,
  index — is unchanged, as it was under 0014.
- The plan records the worktree path, its scratch branch, and the conflicted
  commit, and the report says so: status `conflict`, with next-step commands
  naming the worktree directory and ending with "re-run `discobox apply`".
- The run **stops** there. Sources later in the order are not processed and
  are reported with a distinct `pending` status. Confirmed predecessors are
  never rolled back: they finished, and recovery is per source.

A later run inspects the retained worktree:

- still in a conflict (`CHERRY_PICK_HEAD` or unmerged entries) → the same
  conflict report, stop again;
- resolved (`cherry-pick --continue` completed: no `CHERRY_PICK_HEAD`, HEAD
  advanced, the full range present) → the §4 stale checks pass, then the host
  branch is fast-forwarded onto it, the worktree and scratch branch are
  removed, confirmation proceeds, and the remaining sources run in order;
- worktree deleted/pruned, or the user ran `cherry-pick --abort` in it → an
  error naming what is gone. Apply never invents a resolution; the user
  either finishes or abandons the worktree by hand.

### 3. Resume verifies what already happened, and only fills the gap

Recovery is a re-run of the same command; there is no resume flag.

- A source at `landed` is verified, not redone: the recorded host tip must
  exist and be reachable from the current host HEAD, and the fetched sandbox
  tip must still equal the recorded one. Then apply does no new fetch and no
  cherry-pick — it re-sends complete-apply and reads the sandbox back to
  confirm the exact tuple is present, and marks the source confirmed.
- With no plan file at all, recognition falls back to content: each commit
  in `base..tip` must have a patch-id equivalent on the host branch
  (`git patch-id`). Only an **all-match** range counts as landed, and the host
  tip is the host commit matching the last sandbox commit. A partial match is
  not landed — that is the case where the user also has their own new
  commits, and guessing would repeat the original bug.
- The up-to-date short circuit and the unplaced-source (no local directory)
  path are unchanged.

### 4. A stale plan is refused; existing flags are the only way forward

Resuming a source that has not landed requires the world the plan named to
still be the world. The refusal makes no Git change.

- **Target replaced** — directory gone, a different repository (common-dir
  identity changed), or a different `--dir` — refuse and ask for
  `--dir slug=path`; a re-run with a matching override supersedes the plan
  and plans against the new target.
- **Branch advanced** — host HEAD is no longer the planned HEAD and the
  recorded host tip has not landed — refuse and mark that source's plan
  superseded in the same run. A plain re-run then plans from the current
  HEAD; already-confirmed ranges are recognized through last-applied,
  already-landed ones through §3, so nothing is applied twice.
- **Host working tree dirty** — refuse unless `--allow-dirty` is given. That
  flag now also means "I accept local uncommitted changes; abandon the stale
  plan and plan fresh". Its sandbox-side meaning (apply despite uncommitted
  work in the discobox) is unchanged; the two dirtinesses stay separately
  worded in output. A fast-forward that Git itself refuses over overlapping
  local files still fails as Git says.
- **Identity or selection mismatch** — different server/project/sandbox,
  `--source`, `--dir` set, or `--allow-dirty` — supersedes the old plan and
  starts a new one.

### 5. complete-apply is idempotent on the four-tuple

The endpoint treats `(slug, commit, hostCommit)` — trimmed and
case-normalized, as the fields already are — as an idempotency key. If an
entry with all three already exists on the sandbox, nothing is appended and
the response is the current sandbox carrying the **original** entry, original
`appliedAt` included. Because the check is against persisted state inside the
existing generation-checked metadata transaction, it holds across a server
restart, a lost response, and concurrent retries alike: duplicate requests
append at most once. The same `slug`+`commit` with a *different*
`hostCommit` is a different fact — the same sandbox work landed on a
different host outcome — and appends normally. Unknown slug, validation, and
the append-only shape of `AppliedCommits` are unchanged; there is no schema
change and no migration.

### 6. The unborn-repository path rides the same plan

The first apply into a repository with no commits (0084) and into a directory
with no repository (0139) uses the same plan, phases, and persistent
worktree via the root strategy. 0139's keep rule is unchanged: while HEAD is
still unborn — including a conflict that never landed — the repository apply
made is removed again; once its branch holds commits it is kept.

## Alternatives rejected

**Cherry-pick directly on the host branch and leave `CHERRY_PICK_HEAD`
there.** 0014 rejected this and still should: a range that conflicts at
commit 4 leaves commits 1–3 permanently on the real branch, which is the
partial state no record can describe honestly. A retained worktree outside
the host checkout provides the live conflict without that cost — it is the
option 0014's own trade did not consider.

**New flags (`--resume`, `--continue`, `--force`, `--discard-plan`).** Resume
is what an ordinary re-run already means once the run is durable; the stale
overrides map onto flags that already exist (`--allow-dirty`, `--dir`), and
a branch that simply moved needs only a plain second run after the refusal
retired the plan. A second vocabulary for "apply now" would be surface for no
new capability.

**Store the resume cursor as a client-side Git ref** — 0014 rejected a
single ref for the baseline; the fuller version does not fit either. Refs
live per target repository and cannot name source order across N
repositories, the selection, the target directory, the strategy, or a
retained worktree. The plan is one object describing one run; the fetch ref
remains disposable transport state, as 0014 had it.

**Idempotency on `(slug, commit)` alone, 409 on a different hostCommit.**
Replaying the same sandbox tip onto a genuinely different host outcome —
another host or path, or re-cherry-picking after host history moved — is a
new fact the audit trail should keep. Exact-tuple dedupe gives the retry
guarantee without deciding two different outcomes are the same event.

**Roll back confirmed predecessors when a later source conflicts.** Their
commits landed and were recorded; undoing them would destroy successful work
to make a failure tidier. The failure is per source, and so is recovery.

**Skip the conflicted source and keep applying later sources.** Later
sources may depend on the state the conflicted one was meant to reach, and
apply cannot judge that. Stopping makes the user's resolution the gate;
nothing later is attempted without it.

**A real dry run with `git merge-tree` to avoid persisting attempt state.**
Same objection as in 0014: it computes one tree merge, not an ordered commit
sequence with per-commit metadata, so it would reimplement cherry-pick to
avoid persisting exactly the state that makes resume possible.

## Consequences

- A new client-side package owns the plan file, its lock, the selection
  fingerprint, and stale-world checks; `gitapply` gains a named persistent
  worktree attempt plus inspect-and-finish resume helpers; the apply command
  becomes the state machine this ADR describes.
- The report gains a `pending` status, reachable only when a multi-source
  run stops at a conflict with sources still in the queue. A single-source
  run can never produce it, and the existing statuses keep their meaning.
  `--allow-dirty`'s help text gains its local-dirt meaning.
- A conflict left for the user holds one linked worktree (and its scratch
  branch) under the state directory until resolved or abandoned. A worktree
  pruned out from under the plan is detected on the next run, not treated as
  a repository corruption.
- The TUI still shells out to the same command and parses the same report
  file; conflict remains a non-zero exit, so no success dialog changes.
- complete-apply's OpenAPI description changes; no API schema field and no
  database migration does. 0014 is never edited — this record supersedes the
  named pieces and `cli/DESIGN.md` / `server/DESIGN.md` describe the live
  system.
