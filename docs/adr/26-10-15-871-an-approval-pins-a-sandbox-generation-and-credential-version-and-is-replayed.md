# 26-10-15-871 — An approval pins a sandbox generation and credential version, and is replayed

- **Status**: Accepted
- **Date**: 2026-10-15
- **Relates to**: [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  whose approvals and use IDs this binds to one run and one credential version;
  [ADR 0149](0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md),
  whose trusts get the same generation pin and idempotent approval;
  [ADR 0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md),
  which still gates every mint: nothing is issued without a verdict on record.

## Context

An approver answers one agent's ask, and the answer then travels through
several stages that update independently: the request row, the minted grant
(or host trust), and the pool's locally held state — the broker listing, the
proxy's positive value cache, and the in-memory activations. Three holes follow
from the stages not being one decision:

- **Rebuilds and replacements are invisible to the authorization.** A grant
  and a trust name a sandbox ID; a rebuild is a new `Sandbox.Generation`, not a
  new ID, and the mTLS certificate is reused per ID. A credential has no
  version at all. So an approval made to one container run, or against one
  credential, keeps authorizing the next run and the replacement value. A late
  approval — the person answers after the rebuild — revives an authorization
  the asking run can no longer be the counterpart of.
- **The approval is not idempotent.** A second approve is a 409, and use IDs
  are minted afresh on every attempt. A response lost on the network, a
  duplicate click, or a server restart leaves the caller unable to replay the
  original outcome and risks a second set of use IDs.
- **Early revocation is only eventually consistent.** The pool re-reads live
  grants when an agent asks and re-resolves cached values on its refresh
  interval, but nothing tells the pool precisely which use died: with judging
  off, a cached value can keep serving until its hard expiry.

Static grants — preconfigured with `secret grant create`, given at sandbox
create, or minted by harness configure — are a different act: a standing
authorization a person (or a project) set ahead of any ask. They must keep
working across rebuilds and value rotation, as they do today.

## Decision

**An approval of an agent's ask is one immutable decision pinned to five
things — the sandbox run (generation), the credential version, the use set,
the host scope, and the TTL. The decision is replayed, never remade. Live
reads re-check the pins; the pool re-checks them locally before every value
leaves it; and a use that dies is retired individually through a durable
outbox, never as a side effect of its siblings.**

### 1. What the approval binds

Protocol credential requests and host trust requests record the sandbox's
`generation` when the ask is made. Approving:

- refuses unless the sandbox still exists as data-plus-runtime (not
  `archived`/`deleted`) and its generation is still the one the ask named — a
  late approval for an old run is a 409, not a revival;
- mints the grant or trust with that generation, and a credential grant
  additionally pins the secret's current `value_version` (stored on the grant
  as `secret_version`);
- binds the use set, hosts, and TTL it actually grants on the same rows, as
  today, and mints the use IDs exactly once, inside the approval transaction.

Zero on either pin means "not pinned" and keeps the old behavior wholesale:
every pre-existing row (columns backfilled to zero) and every static grant —
`CreateSecretGrant`, sandbox-create grants, harness/project-scoped grants,
harness configure grants. Only an approval of an agent's ask is pinned.

### 2. Credential versions move on deliberate replacement, not on renewal

`Secret.value_version` starts at 1 and is incremented only by a person's
explicit value replacement (the `UpdateSecret` path that carries a new value,
including a harness configure rewriting a credential). It is **not**
incremented by an OAuth/exchange auto-refresh or by answering a refresh
request: rotation inside the granted lifetime is normal delivery, and TTLs and
the value's own staleness already bound it. A pinned grant records that version
as its `secret_version`; once the secret's moves, the grant stops being live.
An unpinned grant records zero and is unaffected.

### 3. Every live read re-checks the pins

The server is the authority. Every read that can release or report a use
filters pinned rows against the sandbox's current state and generation and the
secret's current version: `FindLiveGrant`, the agent-credential listing,
`ApprovedUse`/`ApprovedCredentialUse`, the credential request poll, and every
live host-trust read (the sandbox's, the pool's, and the trust poll). Mismatch
reads as revoked — the request stays approved history, the grant/trust hands
out nothing. TTL expiry stays a hard time comparison on the same reads and on
the proxy cache entry, so an expired use needs no event.

### 4. Approval is replayed, use IDs are minted once

A second approve of an already-approved request does not mint or conflict: it
is checked against the recorded outcome — secret, scope, hosts, use
descriptions, granted seconds — and a matching replay returns the recorded
request with its original grant and the original use IDs. A replay that names
a different answer is a 409 that says so. A request lost in the approval
race (`ErrGenerationConflict`) is re-read and takes the same path. Denials and
refresh requests are unchanged. The pool's request-status polls already return
the recorded grant, so the agent side learns the same IDs whichever call won.

### 5. Early retirement is an outbox the pool applies and acknowledges

The events that end a pinned authorization before its TTL — a deliberate
credential replacement, a sandbox generation change (rebuild, upgrade,
repair), a sandbox archive/delete, and a host trust revocation — enqueue one
`credential_retirements` row per thing the pool has to drop, **in the same
transaction as the event**, addressed to the sandbox's pool. A row is either
kind `use`, whose target is one approved use ID (one row per pinned use), or
kind `trust`, whose target is the revoked trust ID (one row drops the pin and
its uses). TTL expiry enqueues nothing. An ordinary grant *revocation* rides
the live reads rather than the outbox: the next `get` re-lists uses and the
proxy re-resolves its value, so it needs no prompt beyond those; the outbox
covers the events a re-read cannot catch promptly. Only pinned rows (a
`sandbox_generation > 0` grant, a pinned trust) enqueue — a standing grant has
no pool activation to retire.

The pool's proxy unit polls `GET /api/pools/{poolId}/credential-retirements`
(un-acked rows, oldest first), applies each item locally, and acknowledges the
IDs it applied with `POST …/credential-retirements`. Applying a `use` item
retires its in-memory activations and evicts that use's positive proxy-cache
entries (`Server.InvalidateUse`); a `trust` item removes the pin. The
acknowledgement is idempotent and scoped to the pool, so a restart or a lost
response just re-lists and re-applies — every drop is safe to repeat. The
rows are retained after ack rather than deleted, so the list is "what the pool
has not yet applied," not a cursor it can lose.

**One target's failed retirement is that target's debt alone.** A local apply
that fails (essentially a proxy `ApplyConfig` failure on a trust republish) is
held in a per-pool backoff file and retried on a timer; the poller still
applies and acknowledges everything else, so no failure widens or narrows
another target's authorization, and a restart re-drives the un-acked backlog.
An item of an unknown kind from a newer server is acknowledged rather than
retried forever. On the server, rows are enqueued independently in the event's
transaction, so one failed enqueue can never silence another event.

### 6. The pool gates on the current generation locally, per value

The server populates the sandbox `generation` on the pool's sandbox-create
call. The pool agent records the current generation per sandbox in the file
channel its proxy unit reads, writing it on create/rebuild and removing it on
archive/delete. The broker stamps an activation with the generation that file
names when it mints it. When the file does not name the sandbox yet — a
sandbox already running through the upgrade — the stamp is zero and gates
nothing, so it is never judged by a run record the channel could not have; a
zero-pinned activation is the continuity bridge, as an unpinned grant is on
the server. Before a value leaves the proxy, a positively stamped activation
must still equal the file's current generation for its sandbox: a record that
moved (a rebuild) or stopped naming the sandbox (an archive) refuses it. That
is the immediate block; the server-side pins are the backstop a lagging or
missing file cannot weaken, and the outbox is what evicts already-cached values.

## Alternatives rejected

- **Pin only at the pool, with per-generation mTLS certificates.** Strongest
  identity, but it reissues and redistributes certificate material through
  every provider's runtime intake before any of the authorization holes close,
  and the decision to answer an ask would still leave no server-side record of
  the run and version it answered. The application-layer pins here deliver the
  semantics on channels that already exist; per-generation certificate
  rejection is worthwhile defense in depth but is not built here. This change
  does start populating the create-request `generation` field it would use.
- **Version every grant, static ones included.** Rotating a credential would
  silently revoke standing authorizations a person configured, the opposite of
  what pre-approval means — and a harness reconfigure would strand its own
  harness. Pinning is a property of an approval of an ask, not of a grant.
- **Deterministic use IDs derived from the request.** Idempotent without a
  read, but the agent sees the request ID, so deterministic IDs would be
  guessable before approval and spendable by the agent that guessed. Random IDs
  minted once and replayed keep IDs unforgeable.
- **Invalidate the pool's cache synchronously from the approval/revocation
  call.** The pool is a separate host that may be offline; coupling the
  decision to its cache is exactly the staged-update failure being fixed. The
  outbox makes the decision durable and the cache convergence retryable.
- **Bump the credential version on every value write, including refreshes.**
  OAuth tokens refresh constantly by design; approvals would die on routine
  rotation while granting nothing extra. Replacement is a person's act;
  renewal is the credential's.

## Consequences

- A sandbox image upgrade or any rebuild ends the approvals the prior run
  held; the agent asks again in the new run. Static grants are the continuity
  mechanism and are deliberately unaffected.
- Approval UIs and API callers can safely retry an approve; the second call is
  a replay.
- The pool gains an internal per-pool generations file and a retirement
  backoff state file; neither is part of the sandbox-facing agent credentials
  protocol.
- Old unpinned grants and trusts serve out their existing lifetimes; new
  approvals are pinned from the upgrade onward.
