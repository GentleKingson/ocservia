# Controlled session and service operations

Four typed commands govern active VPN sessions, IP bans, and the local daemon:

- `session.disconnect` targets one numeric session ID on one node boot;
- `session.terminate` targets the same identity and also invalidates its reconnect cookie;
- `ip_ban.remove` targets one canonical IPv4 or IPv6 address;
- `service.reload` targets only `ocserv.service`.

Because reloading the VPN daemon is a high-risk action, `service.reload` also requires a matching, unexpired approval record created by the requester and approved by a different authorized principal, passed on the mutation request in `X-Approval-ID`.

Every mutation request supplies a reason string, a unique `Idempotency-Key`, an optional `ttl_seconds` (1 to 300, defaulting to 60), and the current node revision in `If-Match` or `expected_version`; session commands must also include the boot ID reported by the node API. When accepted, the endpoint returns `202 Accepted` with an asynchronous Operation record. Clients must render that Operation's persisted lifecycle state and never treat initial queueing or transport dispatch as completed execution.

Idempotency keys are scoped to the workspace and bind the target node, typed command payload, expected revision, delivery parameters, acting principal, authorization action, reason, and TTL. Reusing an idempotency key for a different node or mutation payload fails with a conflict, whereas request and trace IDs identify an individual HTTP attempt and may vary across otherwise identical replays.

Before committing the Operation, Command, outbox event, and audit intent, the Controller verifies that the target node holds the matching approved capability and that any targeted session or IP ban is present in current observed state. Once the command reaches the node, the Agent revalidates the signed envelope, expiration timestamp, revision, capability, semantic hash, target syntax, and boot identity before forwarding the request to privd.

Privd never executes arbitrary shell strings. Instead, it maps the four command types to fixed, bounded argument vectors: `occtl disconnect id`, `occtl terminate id`, `occtl unban ip`, and `systemctl reload ocserv.service --no-ask-password`. `occtl terminate` exists only from Ocserv 1.4.2, so the Agent negotiates `ocserv.session.terminate` for a session only when privd reports such a version (enrollment keeps the static capability set), and privd rejects a terminate on an older or unparsable version before running any child, a definite failure rather than `unknown`. Duplicate deliveries replay the result already recorded in the local journal. If an execution outcome is interrupted or ambiguous, the command remains `unknown` until subsequent node telemetry proves whether the effect took place, and only persisted proof of absence authorizes a retry.

## Compatibility and rollback

Each command type uses a distinct protobuf oneof tag and requires the node to advertise the corresponding capability, so unsupported commands are rejected before dispatch. Before rolling back a Controller or Agent binary, stop creating and dispatching these four command types and allow all nonterminal Operations to drain or reconcile. Keep both the Agent SQLite journal and the Controller command history intact so duplicate deliveries and `unknown` outcomes remain reconcilable. The repository provides no database down migrations; when schema state cannot be read by an older binary, apply a forward fix or follow a planned [isolated restore](../operations/incident-recovery.md#database-recovery).

When selected by the CI relevance gate, Basic CI's `go` and `rust` jobs run the standard Go and Rust unit and integration test suites without invoking the standalone Agent and transport boundary scripts. For focused end-to-end verification outside Basic CI, run `scripts/i11-session-operations.sh` manually.
