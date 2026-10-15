# Node enrollment development

For the step-by-step operator workflow, see [Enroll a node](../how-to/enroll-node.md).

Node enrollment registers an Agent identity without activating the node. The
standard bootstrap flow uses a token created via
`POST /api/v1/node-bootstrap-tokens`, which carries an `obt1_` prefix, is
persisted solely as a SHA-256 digest, and remains unbound to any `EndpointID`
until a valid endpoint possession proof arrives at the Controller. When the
token creation request specifies the optional `expected_endpoint_id`, the
Controller rejects any other endpoint before consuming the token. In a single
database transaction, enrollment commits the endpoint binding, `PENDING_APPROVAL`
node row, sealing keys, advertised capabilities, token consumption, and audit
event. Because the Controller rejects any `EndpointID` already present in its
database regardless of node state, a bootstrap token can never re-enroll an
endpoint that has already been bound or revoked. Once consumed, a bootstrap
token may be replayed only by that same `EndpointID` while the node remains in
`PENDING_APPROVAL`, returning the original pending node ID even if the token's
initial validity window has elapsed; replays from any other endpoint or after
node approval fail.

Proving possession of a bootstrap token authorizes only creating or recovering
that bound `PENDING_APPROVAL` record. Activating the node requires an
authenticated requester to submit a content-bound `node.approve` request, a
separate authorized principal to approve it, and a subsequent activation call
carrying that approval ID in `X-Approval-ID`. Enabling the host service remains
a separate node-side step after Controller approval, so completing bootstrap
enrollment by itself neither activates capabilities nor opens a
mutation-capable Agent session.

An alternative manual workflow pre-generates the long-lived endpoint identity
on the host before requesting a legacy endpoint-bound token:

```bash
sudo -u ocserv-agent ocservia-agent \
  --identity-dir /var/lib/ocservia-agent/identity \
  --controller "$CONTROLLER_ENDPOINT_ID" \
  --prepare-enrollment
```

Running `--prepare-enrollment` works offline: it creates or loads the protected
node identity and pinned Controller endpoint ID, then prints the Agent's
64-character lowercase hexadecimal `EndpointID` without reading an enrollment
token, contacting the Controller, or starting an Agent session. Re-running the
command with the same `--controller` value prints the same `EndpointID`, whereas
passing a different Controller pin fails.

With that `EndpointID` in hand, an authenticated operator requests a legacy
token via `POST /api/v1/enrollment-tokens` by setting `expected_endpoint_id` to
the exact 32-byte hex identifier (requests with a missing or malformed value
are rejected). The API returns the plaintext token once with
`Cache-Control: no-store` and persists only its SHA-256 digest, subject to a
maximum lifetime of 15 minutes.

On the node, the Agent stores its endpoint private key inside an owner-only
local directory alongside the pinned Controller `EndpointID`. Through the
enrollment ALPN, the Agent submits the token and public Agent metadata together
with an Ed25519 signature produced by its Endpoint `SecretKey` over a
versioned, domain-separated canonical transcript that binds the token digest,
`EndpointID`, protocol version, Agent metadata, environment, nonce, timestamp,
and sorted capability list. The Controller verifies this signature
independently, treating the `EndpointID` reported by `transportd` as an
additional channel binding. Legacy tokens are strictly single-use: any
concurrent or subsequent replay is rejected even if the initial network
response was dropped, in which case operators look up the pending node ID in
the authenticated Controller inventory without rerunning the enrollment
command. While in `PENDING_APPROVAL`, the node cannot open the agent ALPN, and
its endpoint binding and advertised capabilities wait in the database for
approval.

To run enrollment with a legacy token, write the token into a protected regular
file (an absolute non-symlink path readable only by the Agent user, or owned by
root and group-readable by the Agent group) and invoke the Agent against the
same identity directory prepared earlier:

```bash
sudo -u ocserv-agent ocservia-agent \
  --identity-dir /var/lib/ocservia-agent/identity \
  --controller "$CONTROLLER_ENDPOINT_ID" \
  --enrollment-token-file /run/secrets/ocservia-enrollment-token \
  --enrollment-environment production
```

The command outputs the pending UUIDv7 node ID and exits without opening a
mutation-capable Agent session.

To activate a pending node, call `POST /api/v1/nodes/{node_id}/approval` with
an `X-Approval-ID` header referencing an approved `node.approve` request.
Because the approval hash binds the node ID, `EndpointID`, node version,
policy, labels, and capability set, the activation request body must supply
those exact fields along with a non-empty reason, and the requested capabilities
must be a non-empty subset of those advertised during enrollment. Only a
`PENDING_APPROVAL` node transitions to active; repeating the identical request
against an already activated node verifies the consumed approval and returns
idempotently.

To revoke a node from any non-revoked state (including `PENDING_APPROVAL`),
call `POST /api/v1/nodes/{node_id}/revocation` with an `X-Approval-ID` header
referencing an approved `node.revoke` request and a non-empty reason.
Revocation is recorded durably in the database, blocks future handshakes, and
instructs `transportd` to close any active connection. Because the database
transaction also queues a durable trust-convergence task, receiving a `503`
from the approval or revocation endpoint after commit indicates only that
`transportd` synchronization is still pending, while the database state change
itself remains committed. The background worker retries the exact trust
revision until `transportd` replies `applied` with that retained state and
revision, keeping stale or rejected responses pending and closing a revoked
node's connection only after its exact tombstone is applied. Meanwhile,
Controller event ingress checks every incoming event's node ID and `EndpointID`
against active database trust, rejecting events from any lingering revoked
session before convergence finishes. When the revocation request itself
completes the fenced trust update and then the fenced connection close, it
records them on the queued task for exactly that node, endpoint, revision and
reason, so the worker does not retry a close whose connection, and with it the
fencing owner, is already gone; a failed close stays pending. The worker never
counts lost connection ownership as delivery, and any other intent stays with
the worker.

An RPC timeout, cancellation or unavailable response can occur before or after
the mutation. Neither that error nor a later owner/connection snapshot proves
the historical result. An identical trust update is idempotent for the same
node, EndpointID, state and revision, but still requires a valid binding for its
operation identity (which also covers the reason). Applying a higher revision
can itself close the old connection, so a lost update acknowledgement can also
remove the owner needed for a retry. Retries are conditional on fencing and
cannot guarantee eventual completion.

`CloseNode` verifies its binding before removing the current connection; its
`NOT_FOUND` response is treated by the Go client as satisfied current absence.
That response does not identify which earlier operation, if any, closed the
connection. Accordingly, `close_applied` records satisfaction of the existing
close contract, not a durable receipt for a particular historical close.
`GetNodeConnection` and `GetOwnerFence` expose current in-memory snapshots, not
operation receipts. Restart loads authoritative database trust into transportd's
in-memory policy before accepting connections, but does not reconstruct lost
RPC acknowledgements or settle queue records. A failed or nonmatching
`RecordSynchronized` can leave work pending even after transport effects ran;
the API logs a persistence error but does not undo committed revocation.
See the [read-only incident checks](../operations/incident-recovery.md#revocation-committed-with-synchronization-pending).

When claiming trust-convergence work, the worker computes lease deadlines from
a fresh database wall-clock read after acquiring the claim lock, and uses the
retained attempt counter to fence late RPC responses even if the same worker
reclaims the same revision, preventing equal or older revisions from
overwriting newer intent. Each claim holds a 10-second lease that the worker
renews before every external call and at one-third lease intervals while a
transport RPC is in flight. Losing lease renewal cancels the in-flight RPC, and
the worker completes a final renewal check before writing completion
bookkeeping so a delayed response cannot clobber a superseding update; if
closing a revoked connection fails, the worker retries the close without
re-sending the already acknowledged trust update.

During the regular Agent handshake, the Agent advertises its supported
capabilities and the Controller returns the sorted intersection with the node's
approved database capability set, so advertising newer unapproved capabilities
never blocks enrollment or connection. Under handshake protocol `1.1`, the
Controller signs that negotiated subset, the `EndpointID`, node ID,
authorization revision, and expiration time into `SessionGrantV1`, whereas
protocol `1.0` sessions are restricted to approved read-only capabilities.

Configuring `OCSERV_CONTROLLER_ENDPOINT_ID` enables the HTTP enrollment routes
(which otherwise return `404`) and starts the trust service on
`OCSERV_TRUST_SOCKET` for `worker` and `all` roles. Aside from one-shot
database maintenance commands, production startup fails if the endpoint ID is
omitted:

```text
OCSERV_CONTROLLER_ENDPOINT_ID=<64 lowercase hex characters>
OCSERV_TRUST_SOCKET=/run/ocserv-trust/control-plane.sock
```

Configure `OCSERV_TRANSPORT_UID` and `OCSERV_TRANSPORT_GID` with the exact
numeric UID and GID of `ocservia-transportd`; production startup refuses a
missing transport identity or a transport UID that matches the Controller UID.
The directory housing `OCSERV_TRUST_SOCKET` must already exist, be owned by the
control-plane user without group or world write permissions, and remain
separate from the transport socket directory so a compromised transport process
cannot replace the trust socket. The configured `OCSERV_CONTROLLER_ENDPOINT_ID`
must match the public key derived from `ocservia-transportd`'s controller key.
When connecting over the Unix domain socket, both peers verify the remote
process UID, the socket file's UID, GID, and mode, its non-writable directory
ancestry, and its stable device and inode numbers, and graceful shutdown
unlinks only the specific socket inode created by that server instance.

Before running database recovery, stop new node enrollments and allow pending
trust transitions to converge, preserving all consumed token records, endpoint
bindings, approvals, and revocation history. Static transport bindings support
read-only observation only and cannot authorize privileged commands. Because
the repository provides no database down migrations, recover via a forward fix
or a planned
[isolated restore](../operations/incident-recovery.md#database-recovery).
