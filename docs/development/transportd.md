# Iroh transport development

`ocservia-transportd` owns one Iroh 1.0.x endpoint and routes only
`ocserv-platform/enroll/1` and `ocserv-platform/agent/1`. The Go boundary remains
the versioned gRPC service on a `0660` Unix socket; Go code does not import Iroh
types. The process has no database client or database credentials. Both the
administrative socket and trust socket require configured, exact peer UIDs;
shared group membership grants pathname access but is not authentication.
The transport and control-plane containers share the numeric `ocservia` group
(GID 65532), while retaining distinct non-root users, so only that group can
traverse the runtime directory and connect to the socket.

The controller key is supplied with `--key-file`. The file must be an absolute,
non-symlink regular file owned by the service user, with no group or other
permission bits, and must contain exactly 32 raw bytes or 64 lowercase
hexadecimal characters. Never put this file
in an image, source tree, command-line argument, or log. The endpoint identifier,
which is public, is logged at startup.

For the database-backed lifecycle, pass
`--trust-socket /run/ocserv-trust/control-plane.sock`,
`--control-plane-uid 65534`, and `--control-plane-gid 65532`. The Iroh hook
then admits known Agent EndpointIDs from a bounded authoritative snapshot. Agent
ALPN fails closed until the initial sync completes. An unknown enrollment
EndpointID is limited by its local pre-auth class and never causes a Controller
database call before application data. The trust client verifies the socket and server identity;
the Controller trust server independently verifies transportd's exact UID.
Each socket path must be a non-symlink Unix socket with exact UID/GID/mode under
trusted, non-writable ancestry, and clients reject a pathname identity change
during connection. The startup-only `--approved-binding NODE_UUID=ENDPOINT_ID`
and `--revoked-endpoint ENDPOINT_ID` flags remain available for isolated tests
and observation-only rollback. Static bindings negotiate protocol 1.0 with no
session grant and only the explicit status, version, sessions, IP-ban, and
configuration-fingerprint read capabilities advertised by the Agent. Telemetry
continues, but command dispatch, configuration and certificate operations, and
artifact fetch or consumption are denied before transportd opens an Agent
stream. Restoring management operations requires `--trust-socket` and a valid
Controller-signed protocol 1.1 session grant; transportd cannot mint one.
Endpoint IDs are 32-byte lowercase hexadecimal public identifiers and node IDs
are UUIDv7 values.

Before an I04-to-I05 cutover, inventory every startup `--approved-binding` and
keep the I04 transport running while migration 000004 is applied. The old
database did not store EndpointIDs, so the migration deliberately changes
legacy `approved` nodes to fail-closed `pending` rather than falsely activating
them. For each legacy node, create a one-time token constrained to its exact
workspace, node name, environment, and inventoried EndpointID. Enrollment with
that token attaches the EndpointID to the existing node record; explicitly
approve it before switching transportd to `--trust-socket`. Verify that every
formerly approved node has one active row in `node_endpoint_keys`, then remove
the static binding flags. Do not start trust-socket mode while any legacy node
is still pending or lacks an endpoint binding.

Run with the public relay set:

```bash
ocservia-transportd \
  --socket /run/ocserv-platform/transportd.sock \
  --key-file /run/secrets/controller-iroh.key \
  --relay-mode default \
  --trust-socket /run/ocserv-trust/control-plane.sock \
  --control-plane-uid 65534 \
  --control-plane-gid 65532
```

Use `--relay-mode disabled` for isolated direct-path tests. The endpoint limits
handshake size and time, frame and flow-control windows, remotely initiated
streams, connection count, idle time, connection attempts, event retention, and
UDS subscribers. Trust admission is partitioned into known-Agent handshake,
Agent authorization, Agent registration recheck, unknown-enrollment pre-auth,
and enrollment-completion classes. Each has its own rolling window, semaphore,
bounded metrics, and refusal reason. Unknown enrollment can never borrow the
reserved Agent classes. When bounded identity state is full a new identity is
refused; an old identity is not evicted to refresh attack capacity.
The fixed class label is the only trust-series dimension for
`trust_attempts_total`, `trust_budget_rejections_total`, and
`trust_checks_in_flight`; EndpointIDs never become labels.

Enrollment reads exactly one bounded first application frame under a short
deadline. That frame carries the format-limited one-time token. Under only the
unknown pre-auth connection limits, the Controller first validates the Endpoint
proof and an unconsumed token without modifying state. Only that successful
authority check enters the independent enrollment-completion class; the second
transactional check locks and atomically consumes the token with the durable
node write. Concurrent or later replay is rejected. Tokens and raw EndpointIDs
are never metric labels. Approve, revoke, and endpoint rotation advance the
synchronized snapshot; revoked identities remain fail closed while the
Controller is unavailable. Direct and Relay paths use the same internal
classes. Relay credentials remain defense in depth and are not an Agent
principal or a substitute for reserved Controller capacity.
Connection queries report the agent instance, selected direct
or relay path, path detail, RTT, connection time, and last-seen time.
They also report the negotiated capability set, authorization revision, and
signed-session expiry. Static read-only sessions report revision zero and no
expiry. In database-backed mode, transportd accepts a nonzero revision and
expiry only from the Controller handshake response; it cannot manufacture a
grant. Command dispatch checks the explicit session mode, retained capability,
revision, and expiry before opening a stream. Artifact paths apply the same
session-mode fence. A higher authoritative trust revision closes a connection
retaining an older session grant.

Trust updates report `applied`, `stale`, or `rejected` together with the exact
retained state and revision. Revoked EndpointIDs remain tombstoned with their
original node binding: neither a stale nor a higher-revision ordinary Active
update can reactivate or rebind them. Connection side effects occur only after
the exact authoritative transition advances the retained state.

Iroh is pinned to `1.0.0` in `Cargo.toml`; the workspace carries a provenance-bound
patch of that exact crates.io release, and `Cargo.lock` pins the complete resolved
graph. Transportd opts into persistent connections to every member only for a
custom dedicated relay set containing at least two relays. One preferred relay
remains the sole published home address, while already-authenticated standby
connections allow an incoming Agent to reach the same live Controller endpoint
after the home relay fails. Agents and endpoints using default, disabled, or a
single custom relay retain upstream idle-connection behavior. Patch upgrades
within 1.0.x still require direct, relay, ALPN rejection, path-event, shutdown,
dependency, audit, and license tests because transport and relay internals may
change without affecting the Go contract.

The side-effect-free `ocservia-transportd-stub` remains the default development
stack and rollback mode. To roll back an unshipped Iroh deployment, stop the real
transport process, preserve the controller key, start the stub on the same UDS
path, and restart the Go worker so its watch reconnects. Active Iroh connections
will close and agents must reconnect after the real transport is restored.
Do not roll back migration `000004_enrollment_trust` while enrolled nodes must
remain manageable. The database rollback removes enrollment tokens, endpoint
bindings, and capability approvals. Static startup bindings preserve
observation only; they cannot execute commands or move configuration,
certificate, or artifact state. Restore the startup binding flags before
stopping the trust service, then roll back the migration only after preserving
the required public endpoint-to-node mapping. Restore trusted Controller
authority and a valid signed session before resuming management operations.
