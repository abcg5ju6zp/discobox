# Store Design

`internal/store` owns application persistence. It is the only server package that
should issue resource-level GORM queries or write resource rows directly. The one
exception is the reconcile engine's dirty marks, which `internal/reconcile` writes
itself — inside a store transaction when they commit with intent.

## Boundaries

```mermaid
flowchart LR
    resources["internal/resources/{resource}"] --> store[internal/store]
    service[internal/service] --> store
    services[internal/services] --> store
    auth["internal/auth, auth/sandbox, auth/poolagent"] --> store
    sshd[internal/sshd] --> store
    router[internal/server] -. "store.New(write, read, WithSealer)" .-> store
    store --> db[(GORM write/read handles)]
    store --> model[internal/model]
    store --> secrets[internal/secrets Sealer]
```

- Construct with `New(write, read, ...Option)`; a nil read handle falls back to
  the write handle. `internal/server` wires it, and `cmd/discobox-secret-dump`
  builds its own.
- Keep GORM types and query details inside this package.
- Scope resource queries by project/user/pool IDs as appropriate.
- Re-export the server-owned `apperrors` sentinels through package aliases.

## Transaction Rules

`Store.Transaction(ctx, fn(txStore, txDB))` runs `fn` in one write
transaction. `txStore` is a `Store` bound to the transaction; `txDB` is the raw
handle, passed on to `reconcile.Engine.MarkDirtyTx` so the dirty mark commits with
the intent that caused it.

```mermaid
sequenceDiagram
    participant Service
    participant Store
    participant Engine as reconcile.Engine
    participant DB

    Service->>Store: Transaction(fn(txStore, txDB))
    Store->>DB: txStore writes resource state (generation bump, intent)
    Service->>Engine: MarkDirtyTx(txDB, type, id)
    Engine->>DB: write dirty mark
    DB-->>Store: commit
```

Use a transaction only where two statements must commit together: intent plus
its dirty mark, a create plus its secret assignments, a delete plus its
cascades (`DeleteProject`, `DeleteSecret`, `DeleteSandbox`,
`DeleteHarnessConfig`), a state-report batch. A mutation that is a single
statement does not need one. Nothing is published on commit: a waiting caller
re-reads the rows rather than being told
([ADR 0081](../../../docs/adr/0081-project-events-are-not-persisted-and-the-wait-polls.md)).

## Conditional Writes

- `WithGeneration` / `WithPoolGeneration` (and `UpdatePoolWithGeneration`)
  guard reads and writes on the resource generation; a write that matches no
  row returns `ErrGenerationConflict`.
- `UpdateSecretValueIfUnchanged` (guarded on `updated_at`) and
  `UpdateSecretRequestIfPending` (guarded on status) return
  `ErrGenerationConflict` the same way.

## Field Ownership on Whole-Row Writes

A resource whose status has more than one writer must not be saved by writing
every column from an in-memory copy. The copy was read before whatever the
caller just did, and a concurrent writer's update lands in that window — so the
save silently replays a stale value.

- `Store.UpdateSandbox` omits `observedSandboxColumns` (runtime state and its
  report watermark, provisioning progress, resources); only
  `ApplySandboxStateReports`, `ApplySandboxProgressReports`, and
  `UpdateSandboxResources` write them. See
  [`resources/sandboxes/DESIGN.md`](../resources/sandboxes/DESIGN.md) for who
  owns what and
  [ADR 0034](../../../docs/adr/0034-sandbox-state-and-runtime-state-are-separate-fields.md)
  for the incident. It omits `sandboxMetaColumns` (ADR 0136's meta copy) and
  `sandboxPlatformColumns` too, for the same reason: the sandbox's platform is
  written by the insert and by placement (`SchedulablePoolForSandbox`), which
  runs inside the reconciler's provider call, and the reconciler then saves the
  row it loaded before that call. And `sandboxSkillColumns` (`skills`,
  `skill_names`): skills are fixed at create, and every scan of many sandboxes
  (`ListSandboxes`, a pool's state sync, the upgrade sweep) leaves `skills`
  unloaded — only a read of one sandbox loads them — so a scanned row saved
  back must not write an empty set over them (ADR 26-10-09-395).
- Agent telemetry is written as narrow column updates, never a row `Save`:
  `UpdateSandboxAgentStatus`, `UpdateSandboxResources`,
  `RecordPoolProvisionProgress`, `RecordPoolResources`.
- Pool status ownership is split between agent calls (`RegisterPool`,
  `UpdatePoolStatus`) and the reconciler; see
  [`resources/pools/DESIGN.md`](../resources/pools/DESIGN.md).

When a new field gains a second writer, narrow the write rather than relying on
callers to be careful.

`HarnessConfig.BoundSecrets` is the other way round: no writer at all. It is a
read-only, unmigrated field that `withBoundSecrets` counts from the bindings on
every read that returns a harness config, the `HarnessConfig` a sandbox preloads
included. A new read of either must apply it, or it reports a harness with
credentials as having none.

## Resource Scope

Every resource query must use the store-owned GORM handles rather than opening or
resolving databases itself. Project-owned resources should filter by `project_id`;
pool bootstrap tokens should filter by `pool_id`; user-owned resources should
filter by `user_id`. Peers and `server_state` rows are server-wide and carry no
project scope. `*ByID` lookups (`GetPoolByID`, `GetSandboxByID`,
`GetHarnessConfigByID`) skip project scope and are for trusted control-plane
paths that hold only an ID (agent auth, reconcile dirty IDs).

Lookups by ID go through `firstByID`, which also resolves a unique prefix of a
generated ID; a non-unique prefix is `ErrNotFound`.

Pool deletion guards depend on sandbox assignment checks. `CountSandboxesForPool`
counts every `sandboxes` row with that `pool_id`, and the pool service and
reconciler treat that count as the authoritative stateful assignment; do not infer
emptiness from pool health, driver drift detection, or runtime state.
`DeletePool` itself only removes the row.

Deletes are real row deletes, not tombstones; `hard_delete_test.go` guards
against `gorm.DeletedAt` reappearing on a model.

Do not add database-routing or request-context identity assumptions to store
methods. Pass the resource boundary explicitly through method parameters or use
IDs already carried by persisted rows.

Typed resource views — `Store.Sandboxes()` (`SandboxStore`) and
`Store.SandboxProviderInstances()` (`SandboxProviderInstanceStore`) — expose
`Get`, `Create`, `Update`, `ID`, `Reload`, and `Transaction` (sandboxes add
`UpdateWithGeneration` and `Generation`), delegating to the `Store` methods. They
embed the store, and their `Transaction` rebinds it to the transaction, so their
writes stay atomic with anything else written in it.

## Secret Sealing

With `WithSealer`, the store encrypts secret material on write and decrypts it
only on request: `secrets.encrypted_value` (`OpenSecretValue`) and
`sandboxes.secret_state` (`OpenSandboxSecretState`). Plaintext rows pass through
unchanged. Deleting secrets (directly, with an anonymous sandbox assignment, or
with the project) nulls `encrypted_value` before removing the row.

## Credential approval pins and the retirement outbox

Approval-minted grants and host trusts carry run/version pins
([ADR 26-10-15-871](../../../docs/adr/26-10-15-871-an-approval-pins-a-sandbox-generation-and-credential-version-and-is-replayed.md)):
`SecretGrant.SandboxGeneration`/`SecretVersion` and
`HostTrust.SandboxGeneration`. **Zero is unpinned** and is the only shape a
pre-configured grant or a pre-upgrade row takes.

- `pins.go` owns the gates the live listings apply per row: `GrantPinLive`
  (run present and matching, secret `value_version` unchanged) and
  `TrustPinLive` (run present and matching). `ListLiveAgentCredentials` and the
  live trust listings call them after the SQL time/scope filter, so a rebuild,
  archive/delete, or replaced credential drops the authorization without the
  row being deleted. A missing counterpart reads as dead, not an error — one
  gone row must not fail a whole listing.
- The credential version moves only on an explicit person-driven value
  replacement: `UpdateSecret` with `UpdateSecretBumpsValueVersion` re-reads and
  increments `value_version` inside its transaction. A refresh is not a
  replacement.
- `credential_retirements.go` is the outbox that makes a pool drop local state
  when an authorization ends early. Rows are enqueued **inside the event's
  transaction**, so the event can never commit without them: `UpdateSecret`
  replacement and `DeleteSecret` (`enqueueSecretUseRetirements`), a sandbox
  generation move or delete (`enqueueSandboxUseRetirements`), and trust revoke
  (`enqueueTrustRetirement`). Only pinned (`sandbox_generation > 0`) rows are
  enqueued; a standing grant has no pool activation to retire. Items are
  addressed to the sandbox's pool, listed un-acked oldest-first
  (`ListPendingCredentialRetirements`), and acknowledged by ID
  (`AcknowledgeCredentialRetirements`); the ack is scoped to the pool and
  idempotent. The live gates above stay authoritative — the outbox only drives
  prompt local invalidation; see the pool's [proxyagent design](../../../pool-agent/DESIGN.md).
- The upgrade is one migration step (`database.backfillCredentialPins`, every
  start): secrets start on `value_version = 1`; approval-minted grants,
  protocol requests, and trusts/trust requests pin to their sandbox's current
  generation; grants no request references stay unpinned. A second run writes
  nothing. The same step drops the database-level `sandboxes.pool_id` foreign
  key (pool membership is enforced on use; the GORM relation stays for
  preload).

## Error Contract

`ErrNotFound` and `ErrGenerationConflict` alias server-owned `apperrors`
sentinels so store callers can compare errors through this package without
depending on implementation details. `ErrInUse` is store-owned: a delete
refused because a live resource still references the target
(`DeleteHarnessConfig` with sandboxes on it).

Map database-specific not-found errors to `ErrNotFound` at the store boundary
(`mapNotFound`). Use `ErrGenerationConflict` when a generation-guarded or
otherwise conditional read or write proves the request is stale.

## File Organization

Keep files split by resource area:

| File area | Responsibility |
| --- | --- |
| `store.go` | `Store`, `New`, `WithSealer`, handle resolution, error aliases. |
| `transactions.go` | `Store.Transaction`. |
| `id_lookup.go` | `firstByID`: exact or unique-prefix ID lookup. |
| `projects.go`, `users.go` | Projects, members, per-user defaults, users, project user keys. |
| `harness_configs.go`, `harness_config_secret_bindings.go` | Harness configs and their secret env bindings. |
| `sandboxes.go` | Sandbox desired-state persistence, `SandboxStore`, reconcile scans, archive expiry, agent-status/resource telemetry. |
| `sandbox_state_reports.go` | Pool-agent state and provisioning-progress reports (the observed columns' only writer). |
| `providers.go` | Provider instance persistence and `SandboxProviderInstanceStore`. |
| `pools.go` | Pool persistence: CRUD, agent registration and heartbeats, bootstrap tokens, telemetry, the schedulable-pool placement gate, and pool-scoped sandbox counts. |
| `secrets.go`, `secret_grants.go` | Secrets (sealed values), secret requests, and host-scoped grants. |
| `pins.go` | The run/version gates (`GrantPinLive`, `TrustPinLive`) applied by the live grant/trust listings. |
| `credential_retirements.go` | The per-pool retirement outbox: enqueue-with-the-event, list pending, acknowledge by ID. |
| `host_trusts.go` | Host trust asks and pins, with the live run gate and revoke-time retirement enqueue. |
| `sandbox_secrets.go` | Sandbox secret assignments and agent credentials. |
| `credential_verdicts.go` | Recorded credential verdicts, command and request: written per decision, read per project through `CredentialVerdictFilter`; the judge reads its standing allows (`StandingVerdicts`) and a round's asks for a body (`BodyAsks`, from the primary: a read-after-write that decides who answers the next round) directly. |
| `ssh_keys.go` | Project SSH keys. |
| `peers.go` | Enrolled peers, looked up by ID or validated prefix. |
| `server_state.go` | Server-wide key/value markers (e.g. one-time seeding). |

## Pool Health Freshness

`BeginPoolHealthChecks` stamps all pools before service workers start, leaving
identity, registration, lifecycle, and agent observations intact. Only a new
`UpdatePoolStatus` stamps `StatusReportedAt`. Generation-guarded pool updates
write lifecycle and runtime fields only; metadata updates write the editable
manifest only. Neither can overwrite a newer heartbeat, registration, or
telemetry report.
Placement uses `Pool.IsReady`, not a persisted lifecycle verdict. API projections
use the same health derivation; see [pools](../resources/pools/DESIGN.md).
