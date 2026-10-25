# Iroh transport development

`ocservia-transportd` manages a single Iroh `1.4.0` endpoint that routes only
`ocserv-platform/enroll/1` and `ocserv-platform/agent/1`, exposing a versioned
gRPC service to the Go Controller over a mode `0660` Unix domain socket so Go
packages never import Iroh types directly. The daemon holds neither a database
client nor database credentials. Both its administrative socket and the trust
socket enforce exact configured peer UIDs: while the transport and
control-plane containers share the numeric `ocservia` group (GID `65532`) under
distinct non-root UIDs so only group members can traverse the runtime directory
and reach the socket path, group membership grants only filesystem traversal
and never substitutes for peer UID authentication.

Pass the Controller identity key via `--key-file`, pointing to an absolute,
non-symlink regular file owned by the service user with zero group or world
permission bits and containing either 32 raw bytes or 64 lowercase hexadecimal
characters. Keep this key file out of container images, the source tree,
command-line flags, and logs; at startup, the daemon logs only the derived
public endpoint identifier.

In the normal database-backed lifecycle, start `ocservia-transportd` with
`--trust-socket /run/ocserv-trust/control-plane.sock`,
`--control-plane-uid 65534`, and `--control-plane-gid 65532` (where endpoint
IDs are 32-byte lowercase hexadecimal public keys and node IDs are UUIDv7
values). Under this configuration, the Iroh connection hook admits known Agent
`EndpointID`s from a bounded authoritative snapshot and keeps the Agent ALPN
fail-closed until the initial snapshot synchronization completes. Unknown
enrollment `EndpointID`s are constrained by a dedicated local pre-auth class
that never triggers a Controller database query before application-layer frame
validation. Both sides of the trust socket verify peer and filesystem identity:
the trust client checks the socket file and server UID, the Controller trust
server independently verifies `transportd`'s UID, and both require a
non-symlink Unix socket with exact UID, GID, and mode beneath trusted,
non-writable parent directories while rejecting any pathname identity change
during connection setup.

For isolated testing or observation-only rollback, the startup flags
`--approved-binding NODE_UUID=ENDPOINT_ID` and `--revoked-endpoint ENDPOINT_ID`
configure static bindings without a trust socket. Static bindings negotiate
protocol `1.0` without a session grant, enabling only the status, version,
session, IP-ban, and configuration-fingerprint read capabilities advertised by
the Agent. Although telemetry ingestion continues under static bindings,
`transportd` blocks command dispatch, configuration and certificate mutations,
and artifact fetches or finalization before opening an Agent stream. Resuming
management mutations requires reconnecting `--trust-socket` and obtaining a
Controller-signed protocol `1.1` session grant, which `transportd` cannot mint
on its own.

For isolated local development using the public relay set (never for
production), run:

```bash
ocservia-transportd \
  --socket /run/ocserv-platform/transportd.sock \
  --key-file /run/secrets/controller-iroh.key \
  --relay-mode default \
  --trust-socket /run/ocserv-trust/control-plane.sock \
  --control-plane-uid 65534 \
  --control-plane-gid 65532
```

Pass `--relay-mode disabled` when running isolated direct-path tests. The
endpoint enforces bounds on handshake payload size and duration, frame and
flow-control windows, remotely initiated streams, active connections, idle
timeout, connection rate, event retention, and UDS subscribers. Trust admission
is divided into five independent classes: known-Agent handshake, Agent
authorization, Agent registration recheck, unknown-enrollment pre-auth, and
enrollment completion. Each class maintains its own rolling rate window,
concurrency semaphore, bounded metrics, and rejection reason so unknown
enrollment traffic can never consume capacity reserved for enrolled Agents, and
when the bounded identity table fills, new unknown identities are shed without
evicting existing entries. Each class keeps in-process counters for accepted
attempts, rate and capacity rejections, and checks in flight, keyed only by the
fixed class name and never by raw `EndpointID`s or tokens. `transportd` does
not export these counters as metrics.

On the enrollment ALPN, `transportd` reads a single bounded application frame
under a strict deadline to extract the format-checked one-time token. Still
within the unknown pre-auth budget, the Controller first verifies the endpoint
possession proof and confirms that the token is unconsumed without mutating
database state. Only after that read-only authority check succeeds does the
request enter the enrollment-completion class, where a second transactional
call locks and atomically consumes the token alongside the durable node write,
rejecting any concurrent or subsequent replay. Node approval, revocation, and
endpoint rotation advance the synchronized trust snapshot, and revoked
identities remain fail-closed even if the Controller becomes unreachable.
Direct and Relay connections share the same internal admission classes; Relay
authentication acts only as defense in depth and neither establishes an
Agent principal nor taps reserved Controller capacity.

Connection status queries return the Agent instance ID, active direct or relay
path with path details, RTT, connection establishment timestamp, and last-seen
timestamp, together with the negotiated capability set, authorization revision,
and signed-session expiration time. Static read-only sessions report revision
zero and no expiration, whereas database-backed sessions accept a nonzero
revision and expiry only from the Controller handshake response. Before
opening an Agent stream for command dispatch or artifact transfer, `transportd`
verifies the session mode, negotiated capability, authorization revision, and
grant expiry, and closes any connection whose retained session grant is
superseded by a higher authoritative trust revision.

Calling `SendCommand` returns as soon as `transportd` writes the command frame
to the Agent stream and closes the send side, before the Agent accepts or
executes the command; the Agent's execution outcome is read asynchronously and
published later as a `command_result` event. If a command stream closes early,
times out, or yields a malformed frame prior to a terminal event, `transportd`
emits an `unknown` result. A command stream carries exactly one terminal frame;
a Heartbeat or any other non-result frame on it is malformed and is never
published as telemetry. In every such case `transportd` emits the `unknown` result that the Controller must reconcile instead of
treating it as a clean failure or resending it as a new command. When one to
eight `--controller-verification-key-file` flags are configured, `transportd`
also validates the Controller-signed owner-fence carriers on command, artifact,
and trust-update requests. Once a node registers a fence, those carriers are
mandatory on every subsequent call, and adding `--require-fencing` (which
requires at least one verification key) rejects nodes that have not yet
registered a fence.

Trust synchronization calls return `applied`, `stale`, or `rejected` along with
the exact retained state and revision, and connection side effects run only
after an authoritative transition advances that retained state. Once an
`EndpointID` is revoked, its tombstone and original node binding are permanent:
neither a stale message nor a higher-revision `Active` update can reactivate or
rebind a revoked endpoint.

The Rust workspace pins Iroh to `1.4.0` in `Cargo.toml` using a
provenance-bound patch of that exact `crates.io` release, with the full
dependency graph locked in `Cargo.lock`. Production Agent and `transportd`
launchers accept only a single dedicated Relay URL (although the underlying
`--relay-url` CLI parsers accept between one and eight URLs, the product
launchers enforce the single-relay limit). In the lower-level transport library
and its test suite, configuring a custom dedicated relay set with two or more
relays (an unsupported configuration in production) maintains persistent
standby connections to all members while publishing a single preferred home
relay, allowing incoming Agents to reach the live Controller endpoint if the
home relay drops; default, disabled, and single-relay modes keep upstream
idle-connection handling. Because internal transport or relay behavior can shift
across patch releases without altering the Go gRPC contract, any Iroh patch
update must still pass direct, relay, ALPN-rejection, path-event, shutdown,
dependency, audit, and license checks.

The [standalone `iroh-relay` build](../../deploy/production/relay-build.md) is
pinned separately under `iroh` in `toolchains.lock`: `scripts/build-relay.sh`
verifies the upstream crate archive and compiles the unmodified CLI against
`deploy/production/relay.Cargo.lock` for the production Relay image
(`deploy/production/relay.Dockerfile`). The Rust base image tag
reflects the Rust compiler version, not an Iroh endpoint or Relay protocol
version. Never substitute an unpinned `cargo install --version` build, because
the standalone binary must use the reviewed lock independently of the endpoint
workspace. Relay `1.4.0`'s archive lock and the retained production Relay lock
both select `rustls` `0.23.46`.

Production deployments support a single dedicated Relay; the low-level
multi-member mechanism in the transport library does not extend product support
to Relay redundancy. Schedule a communication maintenance window when servicing
the Relay, preserve its URL and TLS material, and after bringing it back online
verify that Agents establish fresh connections and reconcile pending command
results. Upgrade `transportd` and Agents without altering keys, ALPN strings,
session grants, or authorization revisions. Rolling back to an older
protocol-compatible binary is not automatically safe from a security
standpoint: Iroh `1.0.0` reintroduces the oversized Relay batch and predictable
mapped-address flaws, and building Relay from its upstream lockfile
reintroduces the older `rustls` advisory. When rolling back, use a prior build
that already includes those security fixes, or isolate the affected service
without disabling authentication or resetting journals and revisions.
Production upgrades always require an explicit version inventory,
authorization, and maintenance window.

In local development, `ocservia-transportd-stub` is the side-effect-free
default and must never be used for production recovery. During incident
recovery, preserve enrollment tokens, endpoint bindings, capability approvals,
and the Controller identity key, and use a forward fix or a planned
[isolated restore](../operations/incident-recovery.md#database-recovery) since
no database down migrations exist. Because static startup bindings provide
read-only visibility without the ability to dispatch commands or advance
configuration, certificate, or artifact state, restore live Controller trust
authority and a valid signed session before resuming management operations.
