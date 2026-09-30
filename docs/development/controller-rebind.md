# Controller rebind and retention contract

Status: implementation contract. The [local rebind procedure](../how-to/rebind-controller.md)
implements the manual lifecycle; independent retention has its own safety gates.
Baseline: `95581ec89ee73c9b7403291078b8cefe1c43527d`.

The identity crate now provides `Identity::stage_rebind`: it verifies the
expected source EndpointID and Controller pin, copies the existing endpoint
key into a separate owner-only identity directory, and publishes that directory
with a durable no-replace rename. It never changes the source pin. A partial
staging directory is evidence of an incomplete preparation and is refused on
retry. This primitive is not an activation API; the privileged binding lifecycle
and operational CLI must still enforce the complete transition below.

The runtime selector is `/etc/ocservia-agent/active-binding`, a root-owned,
single-link regular file with mode `0640` (or `0440`) under root-controlled
ancestry. It contains the version, new UUIDv7 NodeID, Controller EndpointID,
preserved Agent EndpointID, independently provisioned Ed25519 command key and
mutation quarantine state. Both services load this complete record; only
absence selects the original installation configuration. Invalid records fail
startup. The record selects separate Agent, privd and upgrader directories
under each service's `bindings/<node-id>` state hierarchy. Business resources
and sealing keys stay in their existing locations. A namespaced Agent journal
also pins its Controller/NodeID and refuses adoption of unbound recovery state.

All services and upgrade runners must be stopped before publishing the selector.
Publication alone is not a supported administrator procedure: use the local
lifecycle's preflight, enrollment, recovery and audit steps. Upgrade
and rollback preflight require binding-aware binaries once the selector exists;
they must not install a binary which ignores this authority boundary.

## Authority and identity

Rebind is an explicit privileged operation on the managed node. It transfers
the node's sole Controller authority without rotating its endpoint private key
or resetting ocserv users, configuration, certificates, or sealing keys. It
does not migrate Controller databases or discover, fail over to, or implicitly
trust another Controller. Ordinary identity provisioning continues to reject
Controller endpoint substitution.

A binding identifies the Controller EndpointID, independently provisioned
command verification keyring, Controller-side NodeID, and its durable command
and effect namespace. These values change together. A new Controller must
allocate a different NodeID. Session grants, authorization revisions, connection
owner floors, idempotency identities and resource revision floors belong to
that binding. The Agent and privd independently enforce the active authority.
The former keyring never remains an alternative mutation authority.

## Durable transition

The operation has one durable identity and records its expected source binding
and explicitly pinned target. Concurrent rebind, upgrade and rollback operations
must be excluded. The stages are:

1. Preflight identity, independently provisioned target trust, file ownership,
   available storage, service configuration and outstanding privileged work.
2. Stage the target without changing the active binding. Enroll using the same
   Agent endpoint private key and existing possession-proof protocol.
3. Verify the response's Controller endpoint, pending result and new NodeID.
   Keep enrollment distinct from independent approval and activation.
4. Quiesce both services and any privileged upgrade runner; commit one durable
   binding selector covering all authority and state paths. A crash must load
   either a complete source or a complete target, never a mixture.
5. Start privd then Agent, verify an authenticated target session, and seal the
   source binding. Record completion durably.

Network failure, rejected token or target pin, invalid command key, response
loss or staging failure must leave the source binding intact. A committed
target whose restart or session verification fails remains recoverable and
fails closed; recovery must not silently reactivate the source Controller.
After target commands can have executed, returning to the source is another
explicit transfer, not filesystem rollback. Preserve the failed transition's
evidence and expose the next recovery action.

Prefer existing `obt1_` bootstrap enrollment for retry: the Controller binds a
consumed token to its endpoint and returns that same pending node on a valid
retry. Persist the target and token identity before the first request; an
unknown response never authorizes issuing another token or creating another
node. Legacy endpoint-bound enrollment tokens remain single-use; response loss
requires authenticated inventory recovery. Neither flow grants approval.

When source administration is available, use the normal approved node
revocation operation and its existing trust convergence. Source availability
is not required for the local transfer. An unreachable source cannot be
updated; its history remains there. Once switched, local enforcement rejects
source sessions and signed commands even if that Controller returns.

## Recovery evidence

Rebind never deletes historical data. Retire the source journal, effect store,
receipts and upgrade intents together with their authority identity. New
commands cannot look up, resume or inherit an old command or its result by an
accidental ID match. Old revision/fence floors remain evidence and do not
become target authority.

Namespace isolation alone is insufficient for uncertain external effects:
an old Unknown or prepared privileged effect can refer to the same ocserv
resource. Preserve the evidence and fail closed on conflicting writes until
explicit reconciliation establishes the outcome. Do not relabel old evidence
as a new Controller's command or infer completion from service health.

## Independent retention

Use a periodic worker with bounded indexed batches and short transactions,
durable progress where required, and idempotent retry. No per-node timers and
no cleanup inside rebind. Existing telemetry maintenance retains its own
14-day raw, 90-day five-minute and 13-calendar-month hourly windows.

The scheduler currently compacts terminal command payloads and retired-node
ordinary snapshots independently of rebind. Each transaction takes at most 32
commands or nodes (at most 32 session rows per selected node), and checks the
scheduler leadership fence before commit. Marked command rows are skipped on
retry. `OCSERV_COMMAND_DETAIL_RETENTION_DAYS` and
`OCSERV_RETIRED_NODE_RETENTION_DAYS` configure these cutoffs.

Command compaction requires both command and operation terminal state, an expired
signed envelope and database expiry, completed outbox publication, no lease,
no sending/Unknown attempt or Unknown result, and no unresolved associated
configuration, artifact or upgrade projection. It replaces command/outbox payloads
with a non-dispatchable evidence header and records the original envelope digest.
Identities, request/idempotency hashes, terminal outcome, authorization signatures,
semantic hashes, revision/fence claims and privileged result proofs remain.
Configuration-plan payloads/results remain because later approval and recovery
read them. Late results cannot mutate a compacted command; late dispatch bookkeeping
cannot restore its full payload.

Retired-node cleanup requires a revoked endpoint and no unresolved command or
projection. It removes bounded session snapshots and clears ordinary health/path
JSON, retaining the node and endpoint revocation records. Audit/security detail uses `OCSERV_AUDIT_RETENTION_DAYS` (365, range 90–2555).
Each pass verifies at most 32 original audit records before removing reason and
before/after summaries, preserving an independently authenticated compact record
and every original chain/checkpoint link. Corrupt or unknown-key evidence stops
the entire batch. Database routines enforce a 90-day minimum and deny ordinary
runtime mutation. At most 32 old security-event detail objects are replaced by
a digest and retained identity/severity/timestamps. Raw telemetry and rollups are
not touched. Local rebind detail still requires its separate retired-state worker.

| Data | Default | Configuration range | Safety boundary |
| --- | --- | --- | --- |
| Retired node ordinary history | 90 days | 30–730 days | Retain node identity, revocation and required references |
| Terminal command detail | 90 days | 30–365 days | Compact; retain command/idempotency identity and recovery proof |
| Audit/security detail | 365 days | 90–2555 days | Preserve authenticated chain continuity and verification |
| Rebind detail | 365 days | 90–2555 days | Retain compact binding/revocation tombstone |
| Agent retired binding detail | 30 days | 7–180 days | Only after all unresolved work is reconciled |

Configuration is validated at startup; negative, zero and out-of-range values
are errors. No setting disables replay, fence or Unknown protection. Cleanup
is eligibility-based: pending, accepted, running, unknown, unresolved effects
and unresolved reconciliation never expire by age. Retired binding cleanup
requires all these blockers to be absent and must retain a compact tombstone.

Before command compaction is enabled, readers must distinguish compacted
terminal records from missing evidence. Retain command and binding identity,
idempotency key and semantic hash/version, terminal state, required receipts,
and replay/revision/fence evidence. A repeated request must not execute again
or invent an empty successful result. Audit cleanup must not invalidate the
existing authenticated hash chain or convert truncation into trusted history.

## Acceptance evidence

Each implementation PR supplies focused tests for its boundary. Final evidence
must exercise the real enrollment, session and privileged-effect paths with
two Controllers, including these cases:

| Case | Required outcome |
| --- | --- |
| Source online or permanently offline | Same Agent EndpointID; different target NodeID; business state preserved |
| Wrong target endpoint, token or command key | Refused; source binding unchanged |
| Enrollment committed, response lost | Recover the same pending node; no blind second enrollment |
| Crash around each commit/restart boundary | Complete binding recovered; no mixed authority |
| Source returns after successful transfer | Source sessions and mutations denied |
| Agent and privd restart | Target authority and isolated namespace restored |
| Old journal Unknown or unresolved privileged effect | Evidence retained; no inheritance; conflicting mutation denied |
| Retention cutoff and repeated worker run | Bounded, resumable, idempotent compaction; safety records retained |
| Telemetry maintenance | Existing 14d / 90d / 13 calendar months unchanged |

Wire transcript, semantic hash, protobuf field numbers and existing REST
behavior remain stable. Reuse enrollment, approval, revocation, trust convergence
and database transaction abstractions; no federation or automatic failover.
