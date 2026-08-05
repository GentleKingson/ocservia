# Controlled session and service operations

The first production command set consists of four typed operations:

- `session.disconnect` targets one numeric session ID on one node boot;
- `session.terminate` targets the same identity and also invalidates its reconnect cookie;
- `ip_ban.remove` targets one canonical IPv4 or IPv6 address;
- `service.reload` targets only `ocserv.service`.

Clients submit a reason, a unique `Idempotency-Key`, and the node revision in
`If-Match` or `expected_version`. Session requests must also carry the boot ID
returned by the node API. The API returns `202 Accepted` with an asynchronous
Operation. Callers must display its persisted state and must not treat queueing
or dispatch as success.

Idempotency keys are scoped to a workspace and bind the target node, typed
payload, expected revision, delivery behavior, actor, authorization action,
reason, and TTL. Reusing a key for another node or intent returns a conflict.
Request and trace identifiers describe an individual attempt and may differ on
an otherwise identical replay.

Each command requires its matching approved node capability. Session and IP-ban
targets must exist in current observed state before the Controller commits the
Operation, Command, outbox event, and audit intent. The Agent revalidates the
typed envelope, expiry, revision, capability, semantic hash, target syntax, and
boot identity before calling privd.

Privd never accepts raw commands. It maps the four mutations to fixed bounded
invocations: `occtl disconnect id`, `occtl terminate id`, `occtl unban ip`, and
`systemctl reload ocserv.service --no-ask-password`. Duplicate delivery replays
the journaled result. An uncertain outcome stays `unknown` until observation
proves the effect is present or absent; only persisted proof of absence permits
a retry.

## Compatibility and rollback

The protocol change adds new oneof tags and does not reuse existing tags. Older
Agents do not advertise the new capabilities, so the Controller rejects these
operations before dispatch. During binary rollback, first stop creation and
dispatch of the four command types and drain or reconcile all nonterminal
Operations. The migration down script intentionally refuses to restore the old
payload constraint while rows of a new command type remain. Preserve the Agent
SQLite journal across rollback so duplicate and uncertain outcomes remain
reconcilable.
