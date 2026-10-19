# Controller rebind and retention contract

For the manual operator workflow, follow the [local rebind procedure](../how-to/rebind-controller.md). Data retention runs independently on its own schedule under the safety rules below.

At the filesystem level, the identity crate's `Identity::stage_rebind` helper verifies the expected source EndpointID and Controller pin, copies the existing endpoint key into a separate owner-only identity directory pinned to the target Controller, and publishes that directory through a durable no-replace rename while leaving the source pin untouched. If a partial staging directory is already present from an interrupted attempt, retry refuses to proceed. Because `Identity::stage_rebind` only prepares the directory, the privileged binding lifecycle and operational CLI remain responsible for driving the full activation transition.

Runtime authority selection is governed by `/etc/ocservia-agent/active-binding`, a root-owned, single-link regular file with mode `0640` (or `0440`) beneath root-controlled parent directories. That record stores the schema version, the new UUIDv7 NodeID, the target Controller EndpointID, the preserved Agent EndpointID, an independently provisioned Ed25519 command key, and the mutation quarantine flag. Both services load the complete record at startup and fail closed if the file is malformed; only the absence of `/etc/ocservia-agent/active-binding` selects the original installation configuration. When present, the selector directs the Agent, privd, and the upgrader to isolated state directories under each service's `bindings/<node-id>` hierarchy, while business resources and sealing keys remain in place. Each namespaced Agent journal also pins its Controller and NodeID pair and refuses to adopt unbound recovery state.

All node services and upgrade runners must stop before `/etc/ocservia-agent/active-binding` is published. Writing the selector file by hand is not a supported administrative shortcut; operators must run the local lifecycle CLI so preflight checks, enrollment, recovery classification, and audit recording all execute in order. Once an active binding selector exists on disk, upgrade and rollback preflight checks require binding-aware binaries and refuse to install any package version that ignores this authority boundary.

## Authority and identity

Rebinding is an explicit privileged operation executed on the managed node. The transfer reassigns the node's sole Controller authority while keeping its endpoint private key, ocserv users, configuration, certificates, and sealing keys intact. Because the process does not copy Controller databases, users, telemetry, or approvals, the target Controller registers the host as a brand-new pending node with no prior history from the source. Rebinding is a deliberate administrative migration, with no automatic discovery, failover, or implicit trust between Controllers, and ordinary identity provisioning still rejects any attempt to substitute a Controller endpoint.

Each binding groups together the Controller EndpointID, an independently provisioned command verification keyring, the Controller-assigned NodeID, and a dedicated durable namespace for commands and effects. All of those attributes switch atomically, and the new Controller must allocate a fresh NodeID. Session grants, authorization revisions, connection owner floors, idempotency identities, and resource revision floors are all scoped to the active binding. Both the Agent and privd enforce the active authority independently, and the retired keyring loses all mutation authority immediately.

## Durable transition

A rebind operation carries a single durable identity that records both its expected source binding and its explicitly pinned target, excluding concurrent rebind, upgrade, or rollback work. Execution proceeds through five ordered stages:

1. Run preflight validation over node identity, independently provisioned target trust, file ownership, and service configuration, including the single dedicated Relay invariant (a non-empty `RELAY_URL_B` in `relays.env` is rejected before enrollment, operation creation, or service shutdown). Free disk space is not checked by the CLI. In-flight or unresolved privileged work does not block the transfer; commit inspects existing evidence and publishes the new binding with mutations quarantined if any prior work remains unresolved.
2. Stage the target identity directory without altering the active binding, then enroll against the target Controller using the existing Agent endpoint private key and possession-proof protocol.
3. Verify the target Controller endpoint, pending enrollment status, and newly assigned NodeID in the enrollment response. Enrollment remains separate from independent approval and node activation.
4. Quiesce both node services and any privileged upgrade runner, then atomically commit the durable binding selector across all authority and state paths so a crash at any point recovers either the complete source binding or the complete target binding, never a split state.
5. Start privd followed by the Agent, confirm an authenticated session with the target Controller, mark the source binding as sealed in the operation record without mutating source files, and durably record completion.

Any failure before selector commit, whether from a network error, a rejected token or target pin, an invalid command key, a lost response, or a staging error, leaves the source binding active and untouched. Once the target selector is committed, a failed service restart or session check fails closed in a recoverable state and never falls back to the source Controller silently. If target commands may already have run, moving back to the original Controller requires a new explicit rebind operation instead of a filesystem rollback. The CLI rejects any target Controller or command key that matches the current binding, and a Controller rejects bootstrap enrollment for any EndpointID already present in its database regardless of state, which prevents rebinding back to a Controller that previously held the same endpoint. Failed transitions keep their diagnostic evidence on disk and report the next recovery step.

The rebind CLI accepts only `obt1_` bootstrap tokens, which support idempotent retry: the Controller binds a consumed token to the requesting endpoint and returns the same pending node record if a retry arrives while the node is still pending. The CLI persists the target identity and token reference before sending the first request, and an ambiguous or lost response never authorizes minting a second token or creating a duplicate node. Legacy endpoint-bound enrollment tokens remain single-use and require authenticated inventory recovery if the response is lost. Neither enrollment path grants node approval.

When the source Controller is still reachable by administrators, revoke the node there through the standard approved revocation workflow so trust state converges cleanly. Local rebind does not depend on source reachability; the CLI records the operator-declared source disposition (`revoked` or `unreachable`) without contacting the source. If the source is unreachable, its historical records stay frozen on that Controller, and local enforcement on the node rejects any future sessions or signed commands from the old Controller even if connectivity returns later.

## Recovery evidence

Rebinding never deletes historical state on the node. Instead, the transition retires the source journal, effect store, receipts, and upgrade intents alongside their old authority identity. Commands issued by the new Controller cannot look up, resume, or inherit prior commands or results even if an ID happens to collide, and old revision or fence floors remain read-only historical evidence without conferring target authority.

Because an older Unknown command or prepared privileged effect could still reference a shared local ocserv resource, namespace separation by itself is not enough to guarantee safety. Whenever commit-time inspection detects unresolved source evidence, including a missing or unreadable Agent journal or an unreadable privileged-effect store, the new binding is published with mutations quarantined, and privd refuses to prepare any new authorized privileged effects until explicit reconciliation resolves the state. The CLI offers no flag to bypass or clear this quarantine, though read-only telemetry and status reporting continue to work. Never relabel source evidence as a target Controller command or assume an uncertain effect finished just because the local service is healthy.

## Independent retention

Historical compaction runs in periodic background passes that process bounded, indexed batches inside short transactions with durable progress tracking and idempotent retry, avoiding per-node timers or inline cleanup during rebind. Existing telemetry retention keeps its established 14-day raw, 90-day five-minute, and 13-calendar-month hourly windows.

On the Controller, the scheduler compacts terminal command payloads and retired-node snapshots independently of rebind operations. Each database transaction processes at most 32 commands or nodes (and at most 32 session rows per selected node) and verifies the scheduler leadership fence before committing. Command rows already marked as compacted are skipped on retry. The cutoff ages are controlled by `OCSERV_COMMAND_DETAIL_RETENTION_DAYS` and `OCSERV_RETIRED_NODE_RETENTION_DAYS`.

Before a command row can be compacted, both the command and its parent operation must be in a terminal state, the signed envelope and database TTL must have expired, outbox publication must be complete, no active lease or sending/Unknown attempt or Unknown result may exist, and no linked configuration, artifact, or upgrade projection may remain unresolved. Compaction replaces the command and outbox payloads with a non-dispatchable evidence header containing the original envelope digest, while preserving identities, request and idempotency hashes, terminal outcomes, authorization signatures, semantic hashes, revision and fence claims, and privileged result proofs. Configuration-plan payloads and results are kept intact because subsequent approvals and recovery workflows read them directly. Once a command is compacted, late results cannot mutate it and late dispatch bookkeeping cannot restore the full payload.

Retired-node cleanup applies only to revoked endpoints that have no unresolved commands or projections. Each pass deletes bounded session snapshots and clears ordinary health and path JSON while preserving the core node row and endpoint revocation records. Audit and security-event compaction follows `OCSERV_AUDIT_RETENTION_DAYS` (default 365, configurable from 90 to 2555 days). In each pass, the worker verifies at most 32 original audit rows before stripping reason strings and before/after summaries, leaving an independently authenticated compact record with every original hash-chain and checkpoint link intact. Any corrupt row or unknown signing key halts the entire batch. Database routines enforce a hard 90-day minimum age and block ordinary runtime table mutations. Up to 32 aged security-event detail payloads per pass are similarly replaced with a digest while retaining identity, severity, and timestamps. Raw telemetry and rollups are untouched by this pass.

On managed nodes, the host-wide `ocservia-agent-retention.timer` triggers hourly with a randomized delay. Each run acquires the shared binding lifecycle lock and scans up to 32 operation records from a durable cursor. Only verified, inactive, unquarantined bindings older than the cutoff are eligible; missing or corrupt privileged evidence, or any unresolved command, effect, artifact, or upgrade, blocks cleanup for that binding. Within each retired Agent journal transaction, the worker compacts up to 32 nonprivileged result payloads into hashes while keeping command identities, terminal outcomes, and all revision and fence floors. Privileged results and root-authenticated effect evidence are never compacted. Journal compaction runs under the unprivileged Agent account while root coordinates protected lifecycle state, and a retired marker on the journal prevents the Agent from ever reopening it for command execution. Endpoint key copies and legacy filesystem paths stay in place for package compatibility. Once the separate rebind-history retention window passes, verified completed rebind records drop their bootstrap token and routine detail fields, keeping only the binding transition summary, declared source disposition, and protected tombstone.

| Data | Default | Configuration range | Safety boundary |
| --- | --- | --- | --- |
| Retired node ordinary history | 90 days | 30 to 730 days | Retain node identity, revocation and required references |
| Terminal command detail | 90 days | 30 to 365 days | Compact; retain command/idempotency identity and recovery proof |
| Audit/security detail | 365 days | 90 to 2555 days | Preserve authenticated chain continuity and verification |
| Rebind detail | 365 days | 90 to 2555 days | Retain compact binding/revocation tombstone |
| Agent retired binding detail | 30 days | 7 to 180 days | Only after all unresolved work is reconciled |

Startup validation rejects negative, zero, or out-of-range retention values. No configuration knob can disable replay protection, authority fencing, or Unknown-effect quarantine. Because retention is eligibility-driven, records in pending, accepted, running, or unknown states, as well as unresolved effects and unreconciled state, never age out by wall-clock time alone. Cleaning up a retired binding requires every blocker to be clear and always leaves a compact tombstone behind.

Callers reading historical state must distinguish compacted terminal records from missing evidence. Replaying a request against a compacted command must neither re-execute the mutation nor synthesize a fake empty success, and audit compaction must keep the full authenticated hash chain verifiable.

## Acceptance evidence

Implementation boundaries require focused unit and integration tests, and end-to-end acceptance must exercise real enrollment, session establishment, and privileged-effect execution across two Controllers for the following scenarios:

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

Wire transcripts, semantic hashes, protobuf field numbers, and existing REST contracts remain unchanged. The implementation reuses the established enrollment, approval, revocation, trust convergence, and database transaction primitives without introducing Controller federation or automatic failover.

The real-process acceptance suite validates both source dispositions:

```sh
OCSERV_REBIND_E2E=revoked scripts/database-controller-e2e.sh postgres all
OCSERV_REBIND_E2E=unreachable scripts/database-controller-e2e.sh postgres all
```

Each run provisions two independent Controller databases, transport identities, and command signing keys, routes traffic through the private TLS relays, and executes the installed local rebind CLI against real Agent, privd, and Ocserv binaries. Completing the transfer requires standard bootstrap possession proof, independent approval, and root attestation registration on the target Controller. The suite then confirms that endpoint keys and local business state are byte-for-byte unchanged, a new NodeID and fresh signed session are active after service restart, and a user mutation succeeds under the target Controller. To verify authority isolation, the test brings an unreachable source Controller back online and injects an unexpired source-signed command envelope directly into privd under the Agent UID, asserting that privd rejects the old authority. Finally, the suite triggers the retention worker before and after the configured cutoff and verifies that compacted command identities, proofs, and retired tombstones remain intact.

Because the test container does not boot systemd as PID 1, its systemctl adapter supervises real daemon processes directly and translates Ocserv status probes and reload requests, while enrollment, sessions, command signatures, root effects, database transactions, and retention passes all run unmocked.
