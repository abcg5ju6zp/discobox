# 26-10-15-943 — A created sandbox's source is one frozen delivery generation

- **Status**: Accepted
- **Date**: 2026-10-15
- **Relates to**:
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)
  (pin and cache delivery this fences),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md)
  (the readiness gate completion opens),
  [0001](0001-sandbox-origin-and-remote-source-push.md)
  (awaiting source and the completion call),
  [26-09-24-005](26-09-24-005-attaching-to-a-discobox-awaiting-its-source-delivers-it.md)
  (two clients delivering one discobox).

## Context

A create from a local directory with uncommitted work, or one carrying extra
repositories, hides snapshots in commits and refs of its own, delivers Git
objects to a pool cache with a pin and a push, and waits for the sandbox to
clone them (ADR 26-10-10-545). Between the accepted request and the sandbox
becoming usable there are windows in which nobody can prove the source that
lands is the source that was accepted:

- a push is interrupted after some objects or refs moved;
- the pool restarts after objects arrived but before the pin or push settled;
- the same create is sent again — a lost response, a restarted client, a
  retried name — and a second snapshot or a moved branch can land in the
  namespace the first request's sandbox clones;
- a client disconnects, and a different client finishes "the" delivery;
- a remote-URL source's branch moves after the create, and a resumed delivery
  that lists the remote again follows it;
- a completion arrives late from an attempt a later create superseded.

The checkout commit is already pinned in the request, and a push is atomic,
but nothing states the delivery *as a whole*: the pool's pin takes whatever
its cache happens to hold, completion only compares commit strings the client
reports, and a repeated create relies on the name-conflict error instead of
returning the first sandbox. A cache hit that never inspects the pinned
objects can release a sandbox whose namespace is short a ref or an object.

## Decision

### 1. The accepted sources are one digest, fixed at create

Every source of a create, ordered by slug, contributes one canonical record:
its slug, its root (a local directory, else its remote URL), its checkout
ref type/name/commit, its workspace mode with the base commit and snapshot
ref, and its destination and working directories. The SHA-256 of that
document is the sandbox's `sourceGeneration`, computed by the client
(`sourcegeneration`) and independently recomputed by the server from the
same request.

- A request that names a generation the server does not recompute is refused
  with 400. The digest covers request fields only; delivery kind and cache
  key are the server's later decisions and never enter it.
- The generation is persisted on the sandbox row (`source_generation`) and
  returned on the sandbox, so any client resuming or taking over reads the
  one it must match. It is immutable for the sandbox's life.
- A no-source create names no generation and takes no part in any fence.
  A request that names sources but no generation is an old client and keeps
  the pre-generation path (§6).

### 2. A create token makes a create one replayable request

The prompt create also carries an opaque client-minted `createToken`. The
server keeps, per project and token, a fingerprint of the request body and
the sandbox it created, in the create transaction.

- A repeated create with the same token and an identical body — including
  the generation — returns the first sandbox with its current state and
  progress. No second sandbox is created and the name-conflict check is
  skipped.
- The same token with a different body is 409. The caller's retries must be
  retries, not an edit.
- An unseen token takes the ordinary path, including the name-conflict
  check. A request without a token behaves exactly as before.

The client records the exact request under its token in its state directory
before sending (`creates/<sha256(server|project|generation)>.json`, private,
atomic, seven-day TTL). A restarted client, or one whose response was lost,
finds the record by server, project and generation and re-sends the recorded
body and token. The record is removed once the sandbox is observed ready,
failed or deleted; delivered-but-still-booting stays recorded. A new name
after a name conflict is a new body and a new token.

### 3. A cache namespace is fenced to the generation it first accepted

Beside a namespace's ownership marker the pool records, at its first pin that
carries a generation, the generation and the complete ref-to-object set that
pin named: the source's branch at its pinned commit, any dirty-workspace
snapshot, and the remote's branches and tags as the delivery knew them.

- A later pin carrying another generation is refused (`ErrGenerationMismatch`,
  409). A same-generation pin is idempotent and is how a failed push resumes:
  the objects the pool already confirmed are pinned again, and the client
  pushes only what is still missing.
- A delivery push carries the same generation header and is checked before
  receive-pack runs: every ref it updates is a recorded ref at its recorded
  object. A push with a generation header into an unrecorded namespace is
  refused; the pin is what records a generation. A later `discobox push`
  carries no header and keeps today's lease rules — the fence protects the
  delivery, it does not freeze the branch the sandbox works on afterwards.
- A namespace with no generation record is pre-ADR and keeps the old path:
  generation fencing never rewrites an existing namespace, and a pool
  restart loses nothing because the record is a file beside the refs, written
  before any ref is.

### 4. Completion is fenced, and the pool verifies the whole generation

`complete-source-push` echoes the sandbox's generation. A sandbox holding one
refuses, with 409, a completion that omits it or names another: a late
completion from an old client or a superseded attempt cannot open a sandbox
frozen to different sources. Before `sourceDeliveredAt` is set, the control
plane asks the hosting pool, once per cache-delivered source through the
sandbox's leased pool client, to verify the namespace (`.../<slug>.git/verify`,
read-only):

1. every recorded ref resolves to its recorded object;
2. every dirty-workspace snapshot's parent is the pinned checkout;
3. every object the namespace's refs reach is present — one
   `rev-list --objects` walk checked with a batched `cat-file --batch-check`.

Anything missing is a 409 naming the refs, snapshots or objects, the sandbox
stays awaiting its source, and a resumed client re-pins and re-pushes. This
is a connectivity check, never a rehash: incoming packs, indexed strictly,
stay the only content-hash boundary. A same-generation completion of an
already completed sandbox is idempotent; a different-generation one is void.
Push-only sources of pre-cache sandboxes keep the commit-comparison check.

### 5. The runtime releases only a verified namespace

The pool runtime's landing check releases a generation-fenced namespace only
when verify passes, not merely when it holds a branch. A cache hit is
therefore checked for the pinned commit, snapshot parentage and object
connectivity like any other delivery. A pre-generation namespace falls back
to "holds a branch". The sandbox agent already checks out the pinned commit
and refuses a snapshot whose parent is not the base; the pool gate closes
the window before the clone.

### 6. Takeover must still match the frozen generation

Any client may finish a delivery, but for a local source only while the local
repository still is the generation: the pinned commit exists, a branch
checkout's local HEAD has not moved off it, and the snapshot ref is still
present. A mismatch is a refusal naming the source — pushing would move the
frozen namespace at different objects. Remote-URL sources need no local state
to match and are exempt. A resumed remote delivery re-pins and fetches by the
recorded commit hash without a fresh `ls-remote`, so the remote moving after
the create cannot redirect what the sandbox clones.

Pure remote sources, clean local repositories, no-source sandboxes and fully
cached pools keep their existing fast paths: fencing is added around them, not
a new route through them.

## Alternatives rejected

- **Content-address the delivery by the pack or the snapshot object alone.**
  A pack differs run to run — negotiation decides its bytes — and one missing
  non-snapshot ref or remote tag would still freeze silently. The generation
  names every ref and its object; the pool proves object presence by
  connectivity.
- **Let the sandbox be the verifier.** It is the consumer, but a sandbox may
  already have cloned a short namespace by the time it reports, and a failed
  clone surfaces as an opaque provisioning failure. The pool verifies before
  the readiness gate opens, with the namespaces and the object store in hand.
- **Re-fetch from the remote to double-check a remote source at completion.**
  That makes a later remote move change the frozen source and hands the pool a
  credential problem ADR 26-10-10-545 rejected. The recorded commit, delivered
  by a client, is the authority.
- **Make the create token the identity and skip the digest.** A token only
  proves the same body was replayed; it says nothing about whether what
  reached the pool matches what was accepted, and it cannot fence the pool or
  a takeover. The digest is the fence; the token is the replay.
- **Lock or delete the namespace on any interruption.** A delta push and an
  atomic ref update leave the cache with useful, verified objects. Idempotent
  re-pinning resumes from them instead of restarting the transfer.

## Consequences

- A sandbox that exists is returned, with its progress, for every retry of the
  accepted create; a lost response, a client restart or a same-name retry no
  longer creates ambiguity or a second delivery.
- The pool can prove a namespace is exactly the frozen generation before the
  sandbox clones it, and completion cannot open a sandbox on anything less.
- Push interruptions resume from the objects already confirmed; pool restarts
  lose the durable record of nothing.
- The client keeps create records for seven days and must hold a local
  source's frozen commits to finish its delivery; a directory that had no
  repository remains deliverable only by the run that built it (ADR 0045).
- Sandboxes, namespaces and clients from before this keep their behavior
  whole: an absent generation is the old path at every stage.
