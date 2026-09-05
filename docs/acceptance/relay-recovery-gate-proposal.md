# Relay Recovery Gate Proposal

Status: **approved; implementation under validation, not release authorization**.

The historical cause of G6 run 33966370408 attempt 1 remains unknown. The
following experiment does not reconstruct that run or claim release readiness.

## Observed Contract Conflict

The targeted experiment uses the real enrollment/approval API, Controller roles,
PostgreSQL outbox, transportd, Iroh relay-b, and the unprivileged Agent with its
SQLite journal. The Agent is isolated from the Controller's bridge before its
mutation session starts. Neither experiment changes that session or owner term.

The control command needs one dispatch. For the fault command, a compile-time
test-only transportd hook discards exactly one successful synthetic-noop result
before event publication. It does not acquire a Controller database lock, change
database state, synthesize a result, close Iroh, or call recovery code.

The ordinary missing-result reaper subsequently prepares RECONCILE_ONLY. Its
prior/current message identities correlate with the two actual frame writes.
Agent journal observations before each response show the same durable effect,
execution timestamp and result digest. The cumulative effect counter does not
increase on reconciliation. The real returned result converges to one durable
succeeded result, succeeded command/operation, and a published, unlocked outbox.

The existing relay proof rejects this valid sequence solely because it contains
two matching frame-write events. This demonstrates a proof/contract mismatch,
not the cause of the historical second write and not a general claim that any
retry is safe. Other failures must continue to fail closed.

## Proposed Acceptance Rule

Do not replace `length == 1` with `length >= 1`.

1. Preserve every matching event and the original failure status. Initially
   accept only one delivery, or exactly two fully explained deliveries. Longer
   sequences remain unproven by this experiment, not silently accepted.
2. Retain the existing command/node, relay-b path, before/after session, owner
   fence, connection and epoch checks for **every** delivery.
3. Require the first delivery to be EXECUTE_OR_REPLAY. Permit only explicitly
   justified RECONCILE_ONLY follow-up, within the existing recovery bound.
   An unexplained additional EXECUTE_OR_REPLAY or RETRY_IF_EFFECT_ABSENT is not
   accepted by this narrowly scoped rule.
4. Require unchanged command, operation, node, idempotency key, semantic hash
   version/hash, sequence, expected revision and capability. Message identity
   must change; delivery mode and authorized timing may change. Do not compare
   whole envelope bytes or require an old signature/fence lease to be reused.
5. For this first correction, accept only the demonstrated same-owner
   sent-result-timeout reason. Other recovery reasons still need their own
   evidence and remain rejected. Correlate the follow-up with committed
   attempt/state evidence and the recovery reason. A log emitted before
   transaction commit is insufficient on
   its own. Preserve the real transport/Agent signature and fencing checks.
6. Require the target's durable journal/effect and result consistency evidence,
   including replay status. Do not infer exactly-once effects from command ID or
   frame count. Keep the current successful result and outbox assertions.
7. Preserve the first dispatch for the existing timing/statistical contract.
   Additional deliveries remain explicit evidence, not discarded log noise.
8. Missing correlation, changed identity/session/semantics, extra effects,
   inconsistent results, missing injection evidence in the experiment, or
   failed collection must not produce an accepted proof.

The approved rule is implemented by `scripts/g6-relay-proof.mjs`, called by
the runtime adapter and final evidence builder. The latter additionally requires
the FD-A Agent's target journal and effect/result observations. It does not
establish that every possible response failure has been reproduced.

## Instrumentation Scope

The patch adds metadata-only transport/Agent diagnostics, targeted read-only
Controller snapshots, and bounded relay failure finalization. The existing
production recovery decisions are unchanged. The relay-a pre-fault proof still
requires one delivery; relay-b uses the bounded rule above.
No secret payload, key, password, or raw production command result is exported.
The Agent effect/result digest observation is limited to synthetic noop.

`relay-recovery-test` is disabled by default and is enabled only by the separate
experiment build script. Its target must match the exact command ID; only an
initial successful, non-replayed synthetic-noop result with matching identity
and semantic hash can be dropped. An exclusive marker permits at most one drop.
The experiment fails unless the marker/event and second dispatch are observed.

The experiment is one-host container isolation using diagnostic binaries, not
a new formal G6 authority, release artifact, production signing check, or
replacement for native architecture/package upgrade acceptance.

## Next Authorization Boundary

After approval of a precise Gate correction, implement and verify that change,
freeze the resulting new Candidate, and obtain its Basic CI, security-inclusive
dual-architecture Release dry-run/package upgrades and full Formal G6 evidence.
Do not relabel prior Candidate results, create v0.5.0, or start an undirected G6
rerun merely to obtain another green result.
