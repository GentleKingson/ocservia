# Agent and privd

The node runtime splits work between an unprivileged `ocservia-agent` process,
which handles network connectivity and local SQLite state, and a small root
daemon, `ocservia-privd`. Privd has no TCP listener and exposes only the local
Unix socket `/run/ocserv-platform/privd.sock`. On that socket it serves eight
read-only queries without requiring a signed command: service status, Ocserv
version, active sessions, IP bans, the `/etc/ocserv/ocserv.conf` fingerprint,
hash-free users and groups parsed from the fixed Ocserv password file, and
recent durable Agent-upgrade results signed by privd as an attested proof.
Observing desired effects requires the original Controller-signed command, as
do all configuration, certificate, session, IP, service, password, group, and
upgrade mutations.

Privd checks the Unix peer UID before decoding any frame, then requires the
original Controller-signed command on every privileged request. Using its own
pinned Controller keyring and node ID, privd independently verifies the
signature, expiry, claims, and recomputed semantic hash, and derives the fixed
operation and effect identity directly from the signed typed payload. The Agent
cannot supply mutation arguments of its own, and RPC input cannot select a
program, systemd unit, occtl argument, or filesystem path. When privd invokes a
child process, it drains stdout and stderr separately under independent size
limits and a deadline, parses the output into stable DTOs, and never returns
raw child output across the privilege boundary. The systemd unit retains only
`CAP_DAC_OVERRIDE`, which packaged Ocserv needs to reach its mode `0711`
control socket, and blocks all IP traffic with `IPAddressDeny=any`. Its primary
group is `ocserv-agent` for socket access, paired with supplementary group
`root` so newly staged password files can be published as `root:root` mode
`0600` without granting general `CAP_CHOWN`.

Privileged mutations record their state in a bounded, root-only SQLite store
under `/var/lib/ocservia-privd` that never leaves the host. Each
authenticated command record binds the node, command, operation, idempotency
key, action, authorization and effect revisions, semantic payload hash, effect
kind, resource, expiry, and bounded successful result, allowing an exact replay
to return the stored result without repeating the root effect. Records for
desired user, group, and configuration changes also bind the authoritative file
transition. Because the store's identity is authenticated, a missing or
mismatched database or HMAC key fails closed. Privd resolves any prepared
whole-file transition before starting another user or group mutation so later
writes cannot overwrite recovery evidence, and only an exact prepared record
with a matching before-state proves that an effect is absent. The service unit
grants write access only to `/var/lib/ocservia-privd` and the fixed Ocserv
directory; operators must keep the generated HMAC key and SQLite database
together during backup, restore, and binary rollback.

Every terminal privileged response includes a `PrivdResultReceiptV1` covering
either a success or a terminal rejection with error code `privd_rejected` or
`capacity_exceeded`. Transient errors such as an unavailable adapter omit the
receipt and remain non-terminal. For `agent.upgrade`, the initial receipt
records only that the upgrade was "scheduled", while the upgrader's final
durable outcome is retrieved later through the attested upgrade-result read.
Privd signs each receipt with a per-node Ed25519 key kept in the root-owned
mode-`0700` state directory as a mode-`0600` single-link regular file. Key
generation uses random bytes written with create-new semantics, file fsync,
atomic rename, and parent-directory fsync; privd refuses to run if the key's
owner, mode, length, file type, link count, or symlink state is unsafe. The
canonical receipt format is independent of Protobuf wire encoding and binds the
node, command, operation, idempotency key, semantic hash, command and result
kinds, exact result digest, effect identity and revision, acceptance and
completion timestamps, and certificate-specific digests. The root effect store
commits the exact result bytes and signed proof before returning success, and
replays those identical bytes and signature on an exact duplicate request.

The Agent treats this proof as opaque bytes, committing the result and proof
together in a single SQLite journal transaction for later replay and marking the
command Unknown if the proof is missing or malformed. Transportd checks only
the bounded shape and version before forwarding the payload unchanged. On the
Controller, the result handler reconstructs the canonical transcript and
accepts a privileged terminal outcome (success or rejection) only when its
claims match the issued command and the signing key is an independently
registered, active, in-window key for that node. If verification fails, the
Controller emits audit and security-alert records and routes the command into
reconciliation without advancing desired state.

During reconciliation, the Agent first queries privd for the exact
authenticated effect record. When a matching root record exists, privd returns
the original result bytes, receipt, and signature, which the Agent atomically
restores to its local journal before reporting a terminal result. Read-only
observation of desired effects can prove that an effect never took place and
unlock a safe retry, whereas synthesizing a privileged success or failure still
requires the original receipt. If root evidence is missing or conflicting, the
command stays Unknown.

## Privd receipt v1 canonical contract

Privd signs a fixed binary transcript independent of Protobuf marshal output. The
transcript starts with the ASCII domain string
`ocservia/privd-result-receipt/v1` followed by a NUL byte. Enum and version
fields are encoded as unsigned 32-bit big-endian integers, and the effect
sequence uses an unsigned 64-bit big-endian integer. Every byte slice and UTF-8
string is preceded by an unsigned 32-bit big-endian byte length. Timestamps
consist of signed 64-bit seconds followed by unsigned 32-bit nanoseconds, both
big-endian, where seconds must be nonnegative and nanoseconds less than one
billion. Booleans and optional-presence markers occupy a single byte (`0` or
`1`), keeping absent fields distinct from present-but-empty values.

Fields appear in a fixed order: receipt version, node ID, key ID, command ID,
operation ID, idempotency key, semantic-hash version and digest, command kind,
result kind, terminal state, exact result digest, error-code digest, effect
record ID and sequence, accepted and completed timestamps, replay marker, and
the optional certificate binding. When present, the certificate binding orders
the certificate ID, CSR digest, public-key digest, requested-subject digest,
and root effect record ID. UUIDs are 16-byte UUIDv7 values, digests and Ed25519
public keys are 32 bytes, effect IDs are 16 to 32 bytes, and signatures are 64
bytes. The complete canonical transcript is bounded to 2048 bytes and the
encoded proof to 64 KiB. Unknown receipt versions, out-of-range enums, lengths,
or timestamps fail closed, and unknown Protobuf fields are excluded from the
transcript.

Establishing key trust requires a random one-time Controller credential
delivered exclusively to root provisioning. The operator runs the installed
root-only helper
`ocservia-privd attestation-registration <key-path> <node-uuidv7> <controller-nonce-hex> <credential-context-sha256-hex>`
and submits its public JSON output alongside the one-time credential to the
Controller registration endpoint. Shipping this helper inside the `ocservia-privd`
binary guarantees that verified package installs and rollbacks never lack the
provisioning tool. Because the Agent cannot read the key or credential, it
cannot register a self-chosen key. Key rotation provisions a new key path and
credential, with the Controller allowing at most one old-and-new key overlap
for up to 24 hours, while explicit revocation increments the node authorization
revision. During an operator-approved rotation, update the root-managed
`PRIVD_ATTESTATION_KEY_FILE` setting in `privd.env` to point to a fresh file
name, register and verify the new key before retiring the old one, and never
overwrite or copy a private key in place.

Roll out upgrades in strict order: deploy the receipt-aware Controller and its
database migration first, initialize and independently register the privd key
(consuming the root-authenticated credential to approve
`privd_result_attestation_v1`), upgrade privd and the Agent, and confirm that
the Agent advertises and negotiates that capability before dispatching
privileged commands. Production runs have no legacy-success fallback, and
historical certificate rows remain read-only history until a new attested CSR
is generated. Rolling back requires stopping privileged dispatch, reconciling
Unknown work, and restoring the Controller, Agent, privd, root effect store,
and key state as a matched set without ever rolling back a single peer in
isolation.

## Provisioned trust

Both node services refuse to start in production unless the pinned Ed25519
public key and the purpose-separated sealing keys are configured. At startup,
privd derives each RSA public key from its private key, checks it against the
enrolled fingerprint, and rejects reused key pairs. The command verification
key is loaded through descriptor-relative, no-follow operations, while sealing
keys must be root-owned regular single-link files with private permissions and
safe directory ancestry. Privd also reads `NODE_ID` from its configuration and
rejects otherwise valid Controller proofs issued for any other node. Enrollment
signs and persists both public-key descriptors so that subsequent session and
password operations fail closed if the advertised or selected key ID diverges
from the enrolled binding.

When updating `/etc/ocserv/ocpasswd`, privd writes a single-link `root:root`
mode `0600` regular file using descriptor-relative, no-follow file operations,
rejecting any existing file or parent directory with unsafe ownership,
permissions, hardlinks, or symlinks.

## Package lifecycle

Native builds, verified staging, manual trust provisioning, upgrade snapshots,
and package removal are documented in
[Agent package lifecycle](../operations/agent-lifecycle.md). For initial
package-based installation, see
[Install a managed node](../getting-started/managed-node.md), and use
[Agent upgrade and rollback](../how-to/agent-lifecycle.md) for ongoing
maintenance commands. Across upgrades and rollbacks, preserve the matched
Agent/privd/upgrader package set, Controller trust, endpoint identity, local
journals, and root effect and key state, since replacing package binaries alone
does not reconcile Unknown commands.
