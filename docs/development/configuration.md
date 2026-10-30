# Configuration planning and apply

## Plan, apply and recovery

### Planning

Configuration planning renders a typed template for a target node, checks the
node's current configuration revision and advertised capability matrix, and
dispatches the immutable candidate to the Agent for side-effect-free
validation. Templates are restricted to an allowlisted set of Ocserv
directives, with node-scoped variables referenced as `${NAME}` and
secret-bearing directives represented by structured `SecretRef` objects; the
API rejects inline secret values and caller-chosen target paths. Rendering is
deterministic so equivalent inputs always produce identical canonical output
and the same SHA-256 candidate hash.

The [complete node-local TLS profile](#node-local-tls-provisioning) uses its
own capability identifiers and typed command payloads. During planning, it
resolves immutable protected TLS bundles on the node and validates the complete
native configuration without omitting directives or emitting partial-validation
warnings. Its validation result returns both the logical candidate hash and the
exact materialized file hash, which are bound alongside the plan ID, previous
file hash, revision, and expiry into the subsequent approval record. Nodes that
do not support the complete profile reject these commands outright with no
fallback to the legacy profile.

Under the legacy profile, privd writes the validation candidate to a temporary
file next to the fixed Ocserv configuration, checks every directive through a
bounded structural parser, and runs the fixed Ocserv binary to validate the
non-secret directive subset. Unresolved `SecretRef` lines are omitted only from
the temporary file passed to the native parser and surface as typed warnings,
without materializing any secret key on disk. Privd deletes the temporary file
as soon as validation finishes, compares the active configuration's fingerprint
before and after the check, and rejects the plan if the underlying file changed
in the meantime. Planning never modifies the active configuration file or
reloads Ocserv.

Calling `GET /config-plans/{plan_id}` returns the candidate hash, operation
state, validation status, warnings, and a secret-safe diff that omits both
`SecretRef` keys and active secret values. Operators can request a production
apply approval only after a plan has validated cleanly and remains unexpired.
The resulting independent approval record is bound to the candidate hash, node
ID, expected revision, and expiration time. Neither planning nor apply ever
masks a stale caller revision by re-reading and substituting the current
revision.

### Apply and rollback

Applying a configuration requires an unexpired, remotely validated plan paired
with an independent approval bound to that plan's candidate hash. Within a
single transaction, the Controller locks the node row, re-verifies the
configuration revision, validation fingerprint, negotiated capability, and
automation lock, consumes the approval, and commits the Operation, signed
command, outbox entry, audit intent, and apply record. When using the complete
profile, the apply command also pins the plan ID, logical and materialized
hashes, previous file hash, revision, and expiry. Root revalidates both the
immutable TLS bundle and the full native configuration, and legacy command and
receipt formats stay unchanged.

On the node, privd never accepts a caller-supplied file path or executable
name. Holding the fixed configuration lock, it:

1. Verifies the active file's fingerprint.
2. Writes backup and staging files in the same directory with preserved
   permissions and ownership.
3. Fsyncs the staged file, atomically renames it into place, and fsyncs the
   parent directory.
4. Reloads the fixed Ocserv service.
5. Verifies the parser check, systemd unit status, and an `occtl` session query.

Privd retains up to ten successful backup files per node.

If the reload or post-reload health check fails, privd restores the exact
backup bytes, reloads the service, and re-verifies the fingerprint and health
probes before returning `rolled_back`. If restoring the backup or verifying the
rolled-back service also fails, privd returns `failed_critical`, causing the
Controller to lock configuration automation for that node and raise a critical
alert. An operator must repair and verify the node locally before a recovery
workflow can clear the automation lock.

Every queued apply increments a monotonic desired effect revision, including
attempts that end in `rolled_back` or `Unknown`. This desired effect revision is
tracked separately from the applied configuration revision, so an
`A -> B -> A` configuration rollback still advances the effect revision and
never reuses an earlier number. Privd durably binds
`config_apply / ocserv.conf / desired_revision` to the command ID, idempotency
key, and semantic hash so applied and rolled-back evidence remains valid even
after the command's authorization proof expires. Recovery can extend the
database observation window while retaining the original signed execution
deadline, as described in [command recovery](command-reliability.md). Neither
matching file contents nor a reconstructed Agent journal can make an expired
command authorization executable again.

### Recovery

Before recovering a database or rolling back binaries, stop new plan and apply
requests, reconcile or expire pending plans, and reconcile every nonterminal
`config_apply` operation while preserving terminal command history and root
effect records. The repository provides no database down migrations, so
recover via a forward fix or a planned
[isolated restore](../operations/incident-recovery.md#database-recovery).

## Complete profile contract

The complete configuration profile is gated by dedicated capabilities and typed
Protobuf payloads. Older nodes reject complete-profile commands without falling
back to v1. Existing v1 payloads, hashes, and receipts remain untouched.
Advertising a capability at handshake time does not bypass runtime validation
when a command executes.

Within the finite grammar of the complete profile, configurations may use plain
authentication against `/etc/ocserv/ocpasswd`, the fixed `/run/ocserv.socket`
control socket, and operator-provisioned node-local TLS material. Unknown or
duplicate directives, `include` statements, hook scripts, caller-chosen file
paths, and unresolved `SecretRef` entries are rejected.

Each opaque TLS reference binds an immutable version, node UUID, allowed
directive slot, public certificate fingerprint, and SPKI fingerprint. Privd
resolves file paths strictly beneath its fixed root-owned trust directory and
rejects unsafe parent permissions, symlinks, hardlinks, wrong file ownership or
modes, missing versions, mismatched node or slot bindings, expired
certificates, or private keys that do not match the public certificate. Private
key bytes and digests of private key material never enter command payloads, API
responses, audit logs, or Controller database tables. The on-disk bundle
manifest is only a local trust input, read under a shared file lock that
prevents version removal or replacement while a plan, apply, or rollback is in
progress.

The protocol keeps three digests distinct: the logical candidate identity
(which pins the node, directive values, reference UUIDs, versions, and public
fingerprint bindings), the exact materialized file hash, and the signed command
semantic hash. Approval records bind the plan ID, both configuration hashes,
the expected current revision, and the expiration timestamp, and are consumed
in the same database transaction that creates the command and outbox rows.
Replaying an apply request with the same HTTP idempotency key returns the
existing operation. Submitting a new idempotency key cannot reuse an already
consumed approval. Requester and approver separation follows the
[approval boundary](../reference/stable-contracts.md#approval-principal-boundary).

Wire messages are generated from the Protobuf and OpenAPI contracts, backed by
shared Go and Rust canonical hash vectors and strict-wire negative tests that
cover the complete-profile payloads without altering existing v1 or v2 hashes.
If any command, hash, fence, root effect, journal, or receipt evidence is
missing or inconsistent, the operation stays `Unknown` and is never cleared
automatically or resent as a fresh mutation.

Release acceptance requires passing focused protocol, Controller transaction,
root path, and TLS unit tests together with end-to-end native parser, apply,
reload, exact byte-level rollback, maintenance restart, and browser checks.
[Release policy](release-checks.md) defines the required exact-candidate
business and native package gates across both supported CPU architectures.
Contract review, unit tests, or checks from earlier candidates do not satisfy
release acceptance.

## Node-local TLS provisioning

Nodes supporting the complete profile negotiate `ocserv.config.complete.plan`
and `ocserv.config.complete.apply`, enforcing all invariants from the
[complete profile contract](#complete-profile-contract).

### Provisioning

Create a dedicated unprivileged `ocservia-vpn` system user whose primary and
sole group is `ocservia-vpn`. Complete-profile configurations pin Ocserv's
worker account to `ocservia-vpn` while leaving the `ocservia-agent` and
`ocservia-privd` service accounts and capabilities unchanged.

Server TLS certificates and private keys remain on the managed node under
root-owned regular single-link files with non-writable directory ancestry and
no symlinks. The private key requires mode `0600` and the public certificate
requires `0600` or `0644`. The leaf certificate must specify `CA:FALSE`, carry
valid server-authentication usage, and match the private key. When an optional
CA bundle is supplied, it must be a valid `CA:TRUE` certificate. All
certificate and SPKI digests recorded in metadata are computed over public DER
bytes only.

Register the bundle reference in the Controller via
`POST /api/v1/secret-provider-refs` with provider `node-local-tls-v1`, an
immutable version string, and public `key_path` metadata formatted as lowercase
hex digests:

```text
<node UUID>/<certificate DER SHA-256>/<SPKI DER SHA-256>/<CA DER SHA-256 or none>
```

The UUID returned by that API call identifies the bundle. On the managed node,
run the provisioning helper as root:

```sh
bash deploy/managed-node/provision-config-tls.sh \
  NODE_UUID REFERENCE_UUID VERSION CERTIFICATE PRIVATE_KEY [CA_CERTIFICATE]
```

The helper refuses to overwrite an existing version directory. Under a shared
resource lock coordinated with privd plan and apply operations, it writes a
protected manifest and mode `0400 root:root` files inside
`/etc/ocservia-agent/config-tls/<reference UUID>/<version>`, publishes the
version directory atomically, and prints only public reference metadata. Keep
previous bundle versions on disk so configuration rollback can reference them,
and never edit or delete version directories by hand.

Before starting a newly installed `ocserv` instance, write an initial
`/etc/ocserv/ocserv.conf` (`0600 root:root`) that conforms to the finite
profile. Point it to the bundle's `server-cert.pem`, `server-key.pem`, and
optional `ca-cert.pem` paths, and configure the `ocservia-vpn` worker account,
plain `/etc/ocserv/ocpasswd` authentication, and the target listener ports.
Then verify it with
`ocserv -t -c /etc/ocserv/ocserv.conf`. On a server that is
already running, initial activation of the complete profile or any change to
the TLS reference or version requires a separately authorized maintenance
restart, because `SIGHUP` does not reload startup-only bindings and no Agent or
privd command restarts the daemon. Keep the previous configuration and TLS
bundle intact so both can be restored under the same maintenance window if the
restart fails.

### Plan and apply

In a complete-profile template, `server-cert`, `server-key`, and optional
`ca-cert` all reference the same `SecretRef` UUID, and privd's root resolver
maps each directive to its fixed file slot within that bundle. When creating a
plan, the Controller resolves the reference's current version and pins it in
the signed command so a subsequent `SecretRef` rotation does not alter an
existing plan. Both planning and apply reject any candidate that changes the
TLS path or version, listener ports, authentication method, worker account, or
control socket path relative to the active `/etc/ocserv/ocserv.conf`. They also
fail closed if the active file contains `include` directives, virtual hosts,
duplicate directives, or settings outside the finite grammar. If an operator
changes any startup-only setting during maintenance, `ocserv` must be restarted
and verified locally before automated configuration plans resume.

Every complete-profile template must specify `auth`, `cookie-timeout`,
`device`, `dns`, `ipv4-network`, `max-clients`, `max-same-clients`,
`server-cert`, `server-key`, `socket-file`, `tcp-port`, and `udp-port`, and may
optionally include a single `route` directive set to one canonical IPv4 CIDR or
`default`. The `auth` and `socket-file` directives are locked to their fixed
production paths, and callers cannot supply custom TLS file paths, `include`
directives, or hook scripts.

The node detail page in the Web UI submits complete-profile plans. Once a plan
validates without warnings, it records the logical candidate hash, exact
materialized file hash, previous file hash, revision, and expiry for review by
a second principal prior to apply. Replaying an apply request cannot authorize
a different operation. During apply, privd re-verifies the TLS bundle and runs
the native `ocserv` parser over the complete file before atomically replacing
`/etc/ocserv/ocserv.conf` and reloading the daemon, restoring the exact
previous file bytes if any post-reload health check fails.

If an apply ends in an uncertain state, never submit a new apply request or
hand-edit the Agent journal, root effect store, or Controller database rows;
preserve all command, hash, fence, and receipt records and follow the
[stable recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
