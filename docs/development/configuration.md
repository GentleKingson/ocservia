# Configuration planning and apply

## Plan, apply and recovery

### Planning

A plan renders a typed template for one node, validates the node's configuration
revision and advertised capability matrix, and sends the immutable candidate to
the Agent for side-effect-free validation.

Templates contain an allowlisted set of Ocserv directives. Node variables use
`${NAME}` references. Secret-bearing directives use structured `SecretRef`
objects; the API does not accept secret values or caller-selected target paths.
Equivalent inputs produce identical canonical output and a SHA-256 candidate
hash.

The matched [complete node-local TLS profile](#node-local-tls-provisioning)
uses separate capabilities and typed payloads. It resolves immutable protected
TLS bundles and validates the entire native configuration, without omitted
directives or partial-validation warnings. Its result includes both the logical
candidate hash and the exact materialized file hash. These and the plan ID,
previous file hash, revision and expiry are bound into approval. Older nodes
cannot accept this profile and there is no legacy fallback.

For the legacy profile, privd writes a validation candidate only to a generated file beside the fixed
Ocserv configuration, validates every directive with a bounded structural parser, and
validates the non-secret directive set with the fixed Ocserv binary. Unresolved
SecretRef lines are omitted only from the native-parser staging input and are
reported as a typed warning; no secret key is materialized. Privd then removes
the staging file. It
fingerprints the current configuration before and after validation and rejects
the result if current state changed. Planning never replaces the current file
and never reloads Ocserv.

`GET /config-plans/{plan_id}` returns the candidate hash, operation state,
validation state, warnings, and a secret-safe diff. The response never contains
SecretRef keys or current secret values. A production apply approval can be
requested only after the plan is valid and unexpired; its independent approval
record is bound to the candidate hash, node, expected revision, and expiry.
Planning and apply never turn a stale caller revision into success by rereading
and substituting the current revision; a stale expected revision fails.

### Apply and rollback

Apply accepts an unexpired, remotely validated plan and an independent approval
bound to its candidate hash. Controller locks the node, rechecks revision,
validation fingerprint, capability and automation lock, then consumes approval
and commits Operation, command, outbox, audit intent and apply record together.
The complete profile also pins plan ID, logical/materialized hashes, previous
file hash, revision and expiry. Root revalidates immutable TLS and the full
native parser input; legacy command and receipt identities are unchanged.

Privd accepts no caller-selected path or executable. Under the fixed config
lock it checks the current fingerprint, creates same-directory backup/staging
files preserving mode/ownership, fsyncs, atomically publishes and fsyncs the
directory. It reloads the fixed service and checks the parser, systemd and an
`occtl` session query. At most ten successful backups are retained per node.

Failed reload/health checks trigger exact backup restoration, reload and
fingerprint/health verification, returning `rolled_back`. Failed restoration or
rollback health returns `failed_critical`; Controller locks config automation
and emits a critical alert. Operators must repair and verify the node locally
before a later recovery workflow clears the lock.

Each queued apply consumes a monotonically increasing desired effect revision,
including rolled-back and Unknown attempts. This revision is separate from
applied configuration revision and cannot be reused after an `A -> B -> A`
transition. Privd durably binds `config_apply / ocserv.conf / desired_revision`
to command, idempotency and semantic hashes. Applied/rolled-back evidence
survives proof expiry. Refreshed authorization can extend reconciliation time;
its expiry is admission metadata, not durable effect identity. File equality
or a rebuilt Agent journal cannot make old authorization current again.

### Recovery

Stop new plan/apply requests and reconcile or expire nonterminal planning work;
reconcile every nonterminal `config_apply`. Preserve terminal command history
and root effect evidence. The tree has no database down migrations; use a
forward fix or a planned
[isolated restore](../operations/incident-recovery.md#database-recovery).

## Complete profile contract

The complete profile uses separately negotiated capabilities and typed payloads;
older nodes must not fall back to v1. Existing v1 payloads, hashes and receipts
stay unchanged. Capability advertisement is not runtime acceptance.

The finite profile permits plain authentication against `/etc/ocserv/ocpasswd`,
the fixed `/run/ocserv.socket` and operator-provisioned node-local TLS.
Reject unknown/duplicate directives, includes, scripts, arbitrary paths and
unresolved SecretRefs. Required directives and provisioning are listed below.

Each opaque TLS reference binds immutable version, node, allowed slot, public
certificate fingerprint and SPKI fingerprint. Root derives paths under its
fixed trust directory and rejects unsafe ancestry, symlinks, hardlinks, wrong
owner/mode, missing versions, mismatched bindings, expired certificates and
key/certificate mismatch. Private key bytes and private-key-content digests
never enter commands, API, audit or Controller storage. The manifest is a trust
input, not an operation database; shared locking prevents version replacement
during plan/apply/rollback.

Keep logical candidate identity, exact materialized file hash and signed command
semantic hash distinct. Logical identity pins node, directives, reference UUIDs,
versions and public bindings. Approval additionally pins plan, both hashes,
expected/current revision and expiry. Consume it transactionally with command/
outbox creation. Same-key replay returns the existing operation; a new key
cannot reuse the grant. Independent principals and self-approval refusal follow
the [approval boundary](../reference/stable-contracts.md#approval-principal-boundary).

Generate required wire fields from Proto/OpenAPI. Go/Rust canonical vectors and
strict-wire negative tests must cover new payloads without changing old hash
meanings. Missing/conflicting command/hash/fence/root/journal/receipt evidence
remains Unknown; never clear it or resend an uncertain mutation.

Acceptance requires focused protocol/Controller transaction/root path/TLS tests
and real native parser, apply, reload, exact rollback, maintenance restart and
browser checks. [Release policy](release-checks.md) owns exact-candidate business
and native package checks on both supported architectures. Contract review,
unit tests and earlier candidates cannot establish this acceptance.

## Node-local TLS provisioning

The matched complete profile uses `ocserv.config.complete.plan` and
`ocserv.config.complete.apply`. Older nodes reject it without legacy fallback. Acceptance follows the
[complete profile contract](#complete-profile-contract).

### Provisioning

Provision a dedicated unprivileged `ocservia-vpn` user whose primary and only
group is `ocservia-vpn`. The profile fixes ocserv's worker identity to that
account; it does not change Agent or privd identities/capabilities.

Keep the server certificate and private key on the node. Root owns the source
files with one link, no symlinks and non-writable ancestry. The key is `0600`;
the public certificate is `0600` or `0644`. An explicit `CA:FALSE`, valid
server-auth certificate must match the private key. Optional CA material must
be a valid `CA:TRUE` certificate. Certificate and SPKI hashes below cover only
public DER, never private key bytes.

Register a SecretRef through `POST /api/v1/secret-provider-refs` using provider
`node-local-tls-v1`, an immutable version and public `key_path` metadata:

```text
<node UUID>/<certificate DER SHA-256>/<SPKI DER SHA-256>/<CA DER SHA-256 or none>
```

Use lowercase hex digests. The returned reference UUID is the bundle identity.
As the node root operator, run the shipped source-tree provisioning helper:

```sh
bash deploy/managed-node/provision-config-tls.sh \
  NODE_UUID REFERENCE_UUID VERSION CERTIFICATE PRIVATE_KEY [CA_CERTIFICATE]
```

The helper refuses replacement of an existing version. It writes a protected
manifest and `0400 root:root` files under the fixed
`/etc/ocservia-agent/config-tls/<reference UUID>/<version>` directory, publishes
the directory atomically and shares a resource lock with root plan/apply.
Its output is public reference metadata only. Retain old versions for rollback;
do not edit/delete version contents or bypass the provisioning lock.

Before starting a newly provisioned ocserv, configure the finite profile with
the bundle's exact `server-cert.pem`, `server-key.pem` and optional `ca-cert.pem`
paths, the dedicated worker identity, plain authentication and chosen ports.
Validate the complete config with `ocserv -t -c /etc/ocserv/ocserv.conf`, keeping
that file `0600 root:root`. For an already running server, initial profile
activation or changing TLS reference/version requires a separately authorized
maintenance restart. Do not pretend a HUP changes startup-only settings. No
Agent/privd command performs that restart. Retain the old config/TLS bundle and
restore both under the same maintenance procedure if activation fails.

### Plan And Apply

Use the same reference UUID for `server-cert`, `server-key` and optional
`ca-cert`; the root resolver selects each fixed slot. The API resolves the
current reference version and pins it in the command. An already created plan
does not follow later SecretRef rotation. Plan/apply rejects a TLS path/version,
listener port, authentication, worker identity or socket change relative to the
protected active config. Includes, vhosts, duplicate directives and an active
config outside the finite grammar fail closed. Operator changes to startup
bindings require a verified restart before resuming automated config work.

The finite profile requires `auth`, `cookie-timeout`, `device`, `dns`,
`ipv4-network`, `max-clients`, `max-same-clients`, `server-cert`, `server-key`,
`socket-file`, `tcp-port` and `udp-port`. Optional `route` is one canonical IPv4
network or `default`. `auth` and `socket-file` have fixed production paths;
TLS paths, includes and executable directives cannot be supplied by callers.

The node detail form submits the complete profile. A valid plan has no partial
validation warnings and includes a logical candidate hash, exact materialized
file hash, previous file hash, revision and expiry. A separate principal
reviews that immutable binding before apply. Replays cannot authorize a new
operation. Root revalidates TLS and the complete native config before atomic
publication and reload; failed health checks restore exact old file bytes.

Never turn an uncertain apply into a new request or manually edit its journal,
effect store or Controller records. Preserve exact command/hash/fence/receipt
evidence and follow the [stable recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
