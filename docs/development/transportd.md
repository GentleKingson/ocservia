# Iroh transport development

`ocservia-transportd` owns one Iroh 1.0.x endpoint and routes only
`ocserv-platform/enroll/1` and `ocserv-platform/agent/1`. The Go boundary remains
the versioned gRPC service on a `0660` Unix socket; Go code does not import Iroh
types. The process has no database client or database credentials.
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
`--trust-socket /run/ocserv-trust/control-plane.sock`. The Iroh hook
then checks remote EndpointIDs with the Go trust service before reading
application data. The startup-only `--approved-binding NODE_UUID=ENDPOINT_ID`
and `--revoked-endpoint ENDPOINT_ID` flags remain available for isolated tests
and rollback. Endpoint IDs are 32-byte lowercase hexadecimal public
identifiers and node IDs are UUIDv7 values.

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
  --trust-socket /run/ocserv-trust/control-plane.sock
```

Use `--relay-mode disabled` for isolated direct-path tests. The endpoint limits
handshake size and time, frame and flow-control windows, remotely initiated
streams, connection count, idle time, connection attempts, event retention, and
UDS subscribers. Connection queries report the agent instance, selected direct
or relay path, path detail, RTT, connection time, and last-seen time.

Iroh is pinned to `1.0.0` in `Cargo.toml`; `Cargo.lock` pins the complete resolved
graph. Patch upgrades within 1.0.x still require direct, relay, ALPN rejection,
path-event, shutdown, dependency, audit, and license tests because transport and
relay internals may change without affecting the Go contract.

The side-effect-free `ocservia-transportd-stub` remains the default development
stack and rollback mode. To roll back an unshipped Iroh deployment, stop the real
transport process, preserve the controller key, start the stub on the same UDS
path, and restart the Go worker so its watch reconnects. Active Iroh connections
will close and agents must reconnect after the real transport is restored.
Do not roll back migration `000004_enrollment_trust` while enrolled nodes must
remain manageable. The database rollback removes enrollment tokens, endpoint
bindings, and capability approvals. Restore the startup binding flags before
stopping the trust service, then roll back the migration only after preserving
the required public endpoint-to-node mapping.
