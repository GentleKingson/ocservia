# Agent and privd

The node runtime is split into an unprivileged `ocservia-agent` and a small
root `ocservia-privd`. The Agent owns network connectivity and local SQLite
state. Privd has no TCP listener. It accepts seven unauthenticated local reads on
`/run/ocserv-platform/privd.sock`: service status, Ocserv version, sessions, IP
bans, the fingerprint of `/etc/ocserv/ocserv.conf`, and hash-free users and
groups derived from the fixed Ocserv password file. Desired-effect observation
requires the original signed command, as do all configuration, certificate,
session, IP, service, password, and group operations.

Privd verifies the Unix peer UID before decoding a request, but UID admission is
only the first layer. Every privileged request carries the original
Controller-signed command. Privd pins its own Controller keyring and node ID,
independently verifies signature, expiry, claims, and recomputed semantic hash,
then derives the fixed operation and effect identity from the signed typed
payload. Agent-selected mutation arguments are not accepted. RPC input cannot
select a program, systemd unit, occtl argument, or filesystem path. Child stdout
and stderr are drained separately with independent limits and a deadline, then
parsed into stable DTOs. Raw child output is never returned across the
privilege boundary.
The privd unit keeps only `CAP_DAC_OVERRIDE`, which packaged Ocserv requires to
connect to its mode `0711` control socket, and blocks all IP traffic with
`IPAddressDeny=any`.

Privileged mutations use a root-only bounded SQLite store under
`/var/lib/ocservia-privd`. The store is local only and is not a business
database or network service. Authenticated command records bind node, command,
operation, idempotency, action, authorization and effect revisions, semantic
payload hash, effect kind, resource, expiry, and the bounded successful result.
An exact replay returns that result without repeating the root effect. Desired
user/group/config records additionally bind the authoritative file transition.
The authenticated store identity makes missing or mismatched database/key state
fail closed. Privd resolves every prepared whole-file transition before another
user/group mutation, so later changes cannot erase earlier recovery proof. Only
an exact prepared record with a matching before-state proves an absent effect.
The unit grants write access only to this state directory and the fixed Ocserv
directory. Keep the generated HMAC key and database together during backup,
restore, and binary rollback.

Every successful privileged terminal response also carries
`PrivdResultReceiptV1`, signed with a per-node Ed25519 key stored only in the
root-owned mode-`0700` privd state directory as a mode-`0600` single-link
regular file. Creation is random, create-new, file-fsynced, atomically renamed,
and parent-fsynced; unsafe owner, mode, length, type, link, or symlink state
stops privd. The canonical receipt is independent of Protobuf encoding and
binds node, command, operation, idempotency key, semantic hash, command/result
kind, exact result digest, effect identity/revision, acceptance/completion time,
and certificate-specific digests. The root effect store persists exact result
bytes and signed proof before returning success; exact replay returns the same
bytes and signature without another effect.

The Agent treats proof as opaque. Its journal commits result and proof in one
SQLite transaction and replays the stored bytes. Missing or malformed proof
becomes Unknown. Transportd enforces only bounded shape/version and forwards it
unchanged. Controller reconstructs the canonical transcript and accepts a
privileged success only against an independently registered active key for that
node. Failure emits audit and security-alert records and enters reconciliation;
it cannot advance desired state.

Reconciliation first asks privd for the exact authenticated effect record. A
matching root record returns its original result bytes, receipt, and signature;
the Agent then atomically restores that evidence before reporting a terminal
result. Read-only desired-effect observation can prove absence for a safe retry,
but it can never synthesize privileged success or failure without the original
receipt. Missing or conflicting root evidence remains Unknown.

## Privd receipt v1 canonical contract

Privd signs the following transcript, not Protobuf marshal output. It begins
with the ASCII domain `ocservia/privd-result-receipt/v1` plus a NUL byte. Enum
and version values are unsigned 32-bit big-endian integers; effect sequence is
unsigned 64-bit big-endian. Every byte string and UTF-8 string has an unsigned
32-bit big-endian length prefix. Timestamps are signed 64-bit seconds followed
by unsigned 32-bit nanoseconds, both big-endian; seconds must be nonnegative and
nanoseconds must be below one billion. Booleans and optional-presence markers
are one byte (`0` or `1`), so absent and present-but-empty are distinct.

The fixed field order is receipt version, node ID, key ID, command ID,
operation ID, idempotency key, semantic-hash version and digest, command kind,
result kind, terminal state, exact result digest, error-code digest, effect
record ID and sequence, accepted and completed timestamps, replay marker, then
the optional certificate binding. That binding orders certificate ID, CSR
digest, public-key digest, requested-subject digest, and root effect record ID.
UUIDs are exactly 16-byte UUIDv7 values; digests and Ed25519 public keys are 32
bytes; effect IDs are 16 to 32 bytes; signatures are 64 bytes; the complete
canonical transcript is at most 2048 bytes and the encoded proof at most 64
KiB. Unknown receipt versions and out-of-range enum, length, or timestamp values
fail closed. Unknown Protobuf fields do not enter the transcript.

Key trust is established with a random one-time Controller credential delivered
only to root provisioning. Run the installed root-only
`ocservia-privd attestation-registration <key-path> <node-uuidv7> <controller-nonce-hex> <credential-context-sha256-hex>`
helper, then relay its public JSON plus the one-time credential to the
registration endpoint. The helper is part of the installed privd binary, so the
verified package and rollback path cannot omit a separate provisioning tool.
Agent cannot read the key or credential and cannot register a
self-selected key. Rotation creates a new key path and credential; Controller
permits at most one old/new overlap for 24 hours. Explicit revocation also
advances the node authorization revision. Set the root-managed
`PRIVD_ATTESTATION_KEY_FILE` in `privd.env` to a new file name only during an
operator-approved rotation, register and verify that key before retiring the
predecessor, and never overwrite or copy a private key in place.

Upgrade in this order: receipt-aware Controller and migration; initialize and
independently register the privd key, whose root-authenticated credential
consumption approves `privd_result_attestation_v1`; upgrade privd and Agent;
verify the Agent advertises and negotiates that capability before dispatching
privileged work. There is no production legacy-success mode. Historical certificate
rows are migration legacy only to preserve schema rollback and cannot start new
signing until a fresh attested CSR is produced. Rollback must stop privileged
dispatch, reconcile Unknown work, and restore the matched Controller, Agent,
privd, root effect store, and key state. Never roll back only one peer.

## Build and install

```bash
cd rust
cargo build --locked --release --package ocservia-agent --package ocservia-privd
cd ..
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 SOURCE_DATE_EPOCH=1786147200 ./scripts/package-agent.sh
VERIFIED_PACKAGE="$(sudo AGENT_TRUSTED_KEY_SHA256=<pinned-public-key-der-sha256> \
  ./scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  /etc/ocservia/release-signing.pub.pem)"
sudo "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

The verifier is the only supported extraction path. It stages and verifies the
exact archive below root-only `/var/lib/ocservia-upgrade/package-staging`; the
installer refuses a source tree or an independently extracted download.

Before enabling the units, install the independently provisioned Controller
command verification key and two distinct RSA private keys for user-password
and P12-password unsealing. Edit `/etc/ocservia-agent/agent.env` with the
Controller key path, controller EndpointID, node UUID, distinct sealing key
IDs, and the lowercase SHA-256 of each public key's DER encoding:

```bash
sudo install -o root -g ocserv-agent -m 0640 \
  controller-command-verification-key.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
sudo install -o root -g root -m 0600 user-password-seal-private.pem \
  /etc/ocservia-agent/user-password-seal-private.pem
sudo install -o root -g root -m 0600 p12-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem
```

Both services refuse production startup without the pinned Ed25519 public key
and exact purpose-separated sealing key configuration. Privd derives each RSA
public key at startup, compares it with the enrolled fingerprint, and refuses
reused key pairs.
The command verification key uses descriptor-relative no-follow loading; the
sealing keys require root ownership, private mode, regular single-link files,
and safe ancestry. Privd also reads the configured `NODE_ID`
and rejects a valid Controller proof for any other node.
Enrollment signs and persists both public-key descriptors. Later sessions and
password operations fail closed if the advertised or selected key ID no longer
matches that enrollment binding.

Then enable the services:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

The installer creates the locked `ocserv-agent` service account and preserves
an existing enrollment configuration. Before changing any installed file,
`upgrade-agent.sh` rejects legacy configuration that does not name a valid,
safely provisioned Ed25519 Controller command verification key. After this
preflight it keeps one matched snapshot of the previous binary pair, base
systemd units, and production relay drop-in state under the root-only
`/var/lib/ocservia-upgrade` hierarchy, outside privd's systemd-managed runtime
state, before restarting the units. Privd publishes `/etc/ocserv/ocpasswd`
as a one-link `root:root` mode `0600` regular file through descriptor-relative,
no-follow operations and rejects unsafe legacy ownership, modes, links, or
parent ancestry instead of inheriting them.

## Rollback and removal

Run `sudo /usr/libexec/ocservia/ocservia-agent-rollback` to roll back an
upgrade. It validates and restores the Agent binary, privd binary, both base
units, and the relay drop-in state from
`/var/lib/ocservia-upgrade/upgrade-backup`, after verifying its root-only
ancestry and trusted digest manifest. It then reloads systemd and starts privd
before the Agent. Restoring either binary without its matched unit and peer
binary is unsupported.

`sudo ./scripts/uninstall-agent.sh` removes units and binaries but retains node
identity, the Agent journal, and privd desired-effect evidence. Use
`--purge-state` only after the node has been revoked, no Unknown command remains,
and the retained identity and reconciliation evidence are no longer needed.
