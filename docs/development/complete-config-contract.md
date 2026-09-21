# Complete ConfigPlan Contract

Status: reviewed scope, implementation present, native acceptance pending. The
maintainer accepted the T07 finite plain-auth / node-local TLS proposal in the
task conversation on 2026-09-21. This acceptance is not independent business
approval custody and does not make the existing positive-apply exclusion PASS.

The existing v1 plan/apply payloads, semantic hashes and historical receipts
remain unchanged. The complete profile requires a separately negotiated
capability and new typed command payloads; it must never fall back to v1 on an
older node. Matched nodes advertise the new capabilities only with the complete
implementation; advertisement alone is not production acceptance evidence.

## Finite Profile

Only plain authentication against `/etc/ocserv/ocpasswd`, the fixed control
socket `/run/ocserv.socket`, and operator-provisioned node-local TLS are in
scope. Required directives are auth, device, tcp-port, udp-port, socket-file,
server-cert, server-key, ipv4-network, dns, max-clients, max-same-clients and
cookie-timeout. Optional CA and routes remain typed and bounded. Unknown or
duplicate directives, includes, scripts, arbitrary file paths and unresolved
SecretRefs are rejected. Native parser validation sees the entire materialized
configuration, including TLS, before a plan is apply-eligible.

Apply is reload-only, never an implicit service restart. Authentication method,
listener ports, worker user/group, control socket and TLS/CA paths must already
match the running node's operator-provisioned startup configuration. Root
rejects changed/missing/duplicate bindings, includes, vhosts and directives
outside the finite profile before planning or effect preparation. Initial
activation and TLS reference/version changes require a separately authorized
node-operator maintenance restart; they are not hot-reload promises. Ocserv's
[1.2.4 configuration contract](https://gitlab.com/openconnect/ocserv/-/raw/1.2.4/doc/sample.config)
distinguishes these startup bindings from reloadable settings. A parser check
and a healthy control socket alone cannot prove that new TLS paths took effect.

## Node-local TLS

Each reference has an opaque UUID, immutable version, one allowed TLS slot,
node identity, public certificate fingerprint and public SPKI fingerprint.
Root derives all paths under its fixed trust directory. No caller-supplied
path, private key bytes or private-key-content digest enters a command, API,
audit record or Controller database. The root-owned manifest is an operator
trust input, not a second operation database.

Resolution must reject unsafe ancestry, symlinks, hardlinks, wrong owner/mode,
missing versions, mismatched node/slot/public bindings, expired certificates
and mismatched key/certificate pairs. Private material remains root-only.
Provisioning and resolution share a resource lock; version contents cannot be
replaced or removed while a plan/apply/rollback is in use. Both old and new TLS
versions remain available for rollback.

## Identity And Approval

Keep three distinct identities: logical candidate hash, materialized file hash
and exact signed command semantic hash. The logical identity pins node,
directives, reference UUIDs, versions and public bindings. The root-attested
materialized hash names exact file bytes, never private key contents.

Approval binds the plan, node, both hashes, expected/current revision and
expiry. Operations consumes it in the existing transaction that creates the
command/outbox. Same-key replay returns the existing operation; a new key
cannot reuse a consumed grant. Requester and approver must differ. Independent
human custody is additional to this principal check; simulated-operator
acceptance must explicitly exclude it rather than claim it was established.
Production personnel requirements remain unchanged.

New required wire fields are generated from Proto/OpenAPI, not hand-edited in
generated files. Canonical Go/Rust hash vectors and strict-wire negative cases
must cover the new payloads without changing old hash-version meanings.

## Effects And Recovery

Reuse the existing config resource lock, same-filesystem staging, backup/fsync,
effect store, Agent journal and authenticated root response. Validate the
complete candidate, verify the expected revision and current physical hash,
publish atomically, reload only ocserv, then verify health and observed hash.
Rollback must restore the exact old config and retained TLS, not merely report
a healthy service. A failed rollback is critical, not success.

Recovery requires authenticated evidence for the exact command/hash/fence.
Missing or conflicting root/journal/receipt evidence remains Unknown and
requires manual reconciliation. Never clear durable state or resend an
uncertain mutation. The historical T07 Unknown run remains evidence of this
boundary even when another attempt succeeds.

## Acceptance Gate

Support remains blocked until focused protocol, Controller transaction, root
path/TLS, native parser/apply/reload/rollback/restart and real-browser tests pass.
The final exact candidate then requires a complete business run and fresh
v0.6.2-to-0.7.0 T06 native upgrade gate on both architectures. Contract approval
alone does not advertise a capability, enable a runtime path or transfer older
candidate evidence.
