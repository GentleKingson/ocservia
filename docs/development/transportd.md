# Iroh transport development

`ocservia-transportd` owns one Iroh 1.0.x endpoint and routes only
`ocserv-platform/enroll/1` and `ocserv-platform/agent/1`. The Go boundary remains
the versioned gRPC service on a `0660` Unix socket; Go code does not import Iroh
types. The process has no database client or database credentials.

The controller key is supplied with `--key-file`. The file must be an absolute,
non-symlink regular file owned by the service user, with no group or other
permission bits, and must contain exactly 32 raw bytes or 64 lowercase
hexadecimal characters. Never put this file
in an image, source tree, command-line argument, or log. The endpoint identifier,
which is public, is logged at startup.

Approved and revoked endpoint identifiers can be supplied with repeated
`--approved-endpoint` and `--revoked-endpoint` flags. They are 32-byte lowercase
hexadecimal public identifiers. Enrollment accepts non-revoked identities;
agent sessions require an approved, non-revoked identity. A later control-plane
step replaces this startup-only test policy with the typed enrollment lifecycle.

Run with the public relay set:

```bash
ocservia-transportd \
  --socket /run/ocserv-platform/transportd.sock \
  --key-file /run/secrets/controller-iroh.key \
  --relay-mode default \
  --approved-endpoint <agent-endpoint-id>
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
Rolling back migration `000003_transport_path_changed` removes derived
`path_changed` events before restoring the earlier event-type constraint.
