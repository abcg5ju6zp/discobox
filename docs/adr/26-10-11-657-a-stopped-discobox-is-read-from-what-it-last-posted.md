# 26-10-11-657 — A stopped discobox is read from what it last posted

- **Status**: Proposed
- **Date**: 2026-10-11
- **Relates to**: [ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  the channel this extends, and whose §3 it supersedes in part;
  [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §12, the pool agent's auto-start latch;
  [ADR 0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md), the
  idle stop a read must not undo;
  [ADR 0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md),
  whose §2 it supersedes in part;
  [ADR 0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md),
  export.

## Context

The pool agent decides, per route, whether a request starts a stopped discobox
(ADR 0017 §12). Listing a discobox's execs, services or tools, or reading one
exec or service, starts it. So does anything that polls those lists: the
console's open workspace, an orchestrator looking over its workers. An idle
stop (ADR 0108) is undone by the next look at the box it stopped.

Five later reads were registered the other way — a terminal's screen, a wait on
it, the exec event trail, the harness hooks — and refuse a stopped box with a
409 (ADR 0137 §2, ADR 0130). That keeps the box down, but answers nothing.

The control plane already keeps the last status each box reported
(`runtime.agentStatus`, ADR 0030), which carries terminal sessions, ports and
meta. It does not carry the exec list, the service list, the tools, or a
screen. It also could not: the status is on the sandbox row, so every listing
of sandboxes returns it, and it is rewritten every 15 seconds.

## Decision

**A discobox posts an inventory — what its list reads answer — and the control
plane answers those reads from the last one whenever the discobox is not
running. A read never starts a discobox.**

### 1. The inventory

The sandbox agent serves `GET
/api/projects/{projectId}/sandboxes/{sandboxId}/inventory`, holding exactly
what four of its own routes would answer at that moment:

- `execs`: the exec list;
- `services`: the service list;
- `tools`: the tool list;
- `screens`: by exec id, the visible screen of every exec that has one, with
  no scrollback.

A part the box cannot answer is absent, not empty: a box whose services are
unavailable posts no `services`. It also carries `observedAt`, on the
sandbox's clock, and `digest`, a hash of the four parts and nothing else.

The route is gated by a scope of its own, `inventory:read`. The token the pool
agent polls with is minted with `status:read` and `inventory:read`, both
literals in the handler; this supersedes ADR 0030 §3's "exactly
`["status:read"]`". The endpoint still accepts no caller-supplied scope.

### 2. It is posted only when it changes

The pool agent asks for the inventory on the standing poll, naming the digest
it last delivered (`?known=`). A sandbox agent whose inventory still has that
digest answers `unchanged` and no parts.

The inventory rides the status batch (ADR 0030): an entry carries the
inventory when it changed and only its digest when it did not. The control
plane stores a changed one, and for an unchanged one moves the stored
inventory's observation time forward. The batch's answer names the sandboxes
whose digest it does not hold, and the pool agent sends those in full on its
next tick. A pool agent records a digest as delivered only on an answer that
does not name the sandbox.

Configure sandboxes are not asked: they are polled every second
(ADR 26-10-09-867) and nobody reads one that has stopped.

### 3. Where it is kept

One row per sandbox in a table of its own, `sandbox_inventories`, never on the
sandbox row and never in a sandbox listing. The document is sealed with the
server's sealer, as secrets are, because it holds terminal content and the
environment exec records carry. The row is deleted with its sandbox.

### 4. Which reads it answers

| Read | Running | Not running |
| --- | --- | --- |
| exec list, exec get, service list, service get, tools, terminal screen | proxied, live | answered from the inventory |
| exec logs, exec events, service logs, exec resources (snapshot, history, stream) | proxied, live | starts the box, as before |
| exec wait, exec event trail, harness hooks | proxied, live | refused with 409, as before |

- **The control plane decides from the row.** A sandbox whose `runtimeState`
  is not `running` is answered from the inventory without reaching its pool,
  so a stopped box on an unreachable pool is still read.
- **The pool agent refuses rather than starts.** The six reads in the first
  row are registered `requireRunning`, whose 409 now carries a header naming
  the state. A box that stops between the control plane's check and the proxy
  is refused that way, and the control plane then answers from the inventory.
- **A stored answer has the live answer's shape**, plus `observedAt`: when the
  box posted it. A live answer has no `observedAt`. That one field is how a
  client tells the two apart and shows how old a stored answer is.
- **Records are served as posted.** An exec the box last reported `running` is
  answered `running`; `observedAt` is what says the box may no longer be.
- **A stored screen has no scrollback**, whatever `?scrollback=` asks.
- **No inventory, no answer.** A box that has never posted one — it never ran,
  it last ran on a sandbox agent older than this, or it was imported — is
  refused with 409 saying so. So is a part the inventory lacks.

This supersedes ADR 0137 §2's "the pool serves a screen … only while the
discobox runs" for the screen; the wait is unchanged.

### 5. What does not change

- **Any action or mutation starts the box**: attach, exec create, input,
  start, delete; service start, stop, restart; a meta write.
- **Unbounded reads keep starting it.** Logs, events and resource samples have
  no last value worth posting; a client that asks for a stopped box's
  transcript is using the box.
- **Exec wait keeps refusing.** An orchestrator's poll must not wake a worker,
  and a wait has no stored answer.
- **Export carries none of it** (ADR 0123). An imported discobox has no
  inventory until it first runs.

## Alternatives rejected

**More fields on the status report.** One channel, no new route or table.
Rejected: the status is stored on the sandbox row, so the exec list, the tool
catalog and every screen would be returned by every sandbox listing and
rewritten every 15 seconds whether or not they changed.

**Taking the inventory when the box stops.** Cheaper than a standing poll, and
exactly the state a stopped box is read for. Rejected: an idle stop is the
sandbox powering itself off (ADR 0108), and the sandbox agent may not call out
(ADR 0030), so nothing can post at that moment. The pool would have to read a
stopped box's files, per runtime. The standing poll's answer is at most one
interval old, and a box that idled out changed little in its last 15 seconds.

**Not storing screens.** Keeps terminal content out of the server's database;
a stopped box's screen would keep refusing. Rejected by the user: the last
screen is what a lead reads a stopped worker for. The cost is accepted and
bounded — the visible screen only, sealed at rest, deleted with the sandbox.

**Starting the box for a read with no inventory.** Nothing would look
different for boxes that predate this. Rejected: it keeps a path by which a
poll wakes a box, which is the behavior being removed.

**Answering empty lists when there is no inventory.** Never an error. Rejected:
"nothing there" and "not known" would look the same to every client that does
not check a marker.

**Marking the pool's refusal by its message.** No new header. Rejected: the
control plane would match prose that exists to be read by people.

## Consequences

- Opening a stopped discobox's workspace in the console, or listing its
  services or tools, no longer starts it. Attaching to one of its terminals
  does.
- Terminal content and exec environment are at rest in the server's database,
  sealed. Before this the server held only terminal titles.
- A stored answer can be up to one poll interval older than the stop it
  describes.
- A busy terminal changes its screen between most polls, so a working box
  posts its whole inventory every 15 seconds. Splitting the screens from the
  lists, each with its own digest, is the next step if that proves a cost.
- A sandbox agent older than this has no inventory route; its box is read as
  one with no inventory once it stops. A pool agent older than this still
  starts a box for these reads when the control plane's row says `running`
  and the box is not.
