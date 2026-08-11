# Node enrollment development

Enrollment is explicit and does not activate a node. An authenticated operator
first creates a token with `POST /api/v1/enrollment-tokens`. Every token must
name the expected 32-byte EndpointID; unbound production tokens are rejected.
The response is marked `Cache-Control: no-store` and returns the plaintext
token once; only its SHA-256 digest is stored. Tokens expire after at most 15
minutes.

The Agent keeps its endpoint key in an owner-only local directory and pins the
controller EndpointID. Reusing the directory preserves the Agent EndpointID;
attempting to provision it with a different controller pin is rejected. The
enrollment ALPN submits the token and public Agent metadata and returns a
pending node ID. The request carries an Ed25519 proof made by that Endpoint
SecretKey over a versioned, domain-separated canonical request. It binds the
token hash, EndpointID, protocol version, Agent metadata, environment, nonce,
timestamp, and sorted advertised capabilities. The Controller verifies this
proof independently; the EndpointID reported by transportd is only an
additional channel binding. If the response is lost, the same authenticated
EndpointID can reuse the consumed token to recover its node ID before or after
approval; revocation permanently ends recovery. Pending nodes cannot use the
agent ALPN. The pending endpoint binding and advertised supported capabilities
are retained for approval.

Run enrollment with the same protected identity directory that the installed
Agent will use:

```bash
sudo -u ocserv-agent ocservia-agent \
  --identity-dir /var/lib/ocservia-agent/identity \
  --controller "$CONTROLLER_ENDPOINT_ID" \
  --enrollment-token-file /run/secrets/ocservia-enrollment-token \
  --enrollment-environment production
```

The token file must be an absolute, non-symlink regular file readable only by
the Agent, or a root-owned group-readable file for the Agent group. Enrollment
prints the pending UUIDv7 node ID. It does not create a mutation-capable Agent
session.

Approve the node with `POST /api/v1/nodes/{node_id}/approval`, including a
non-empty reason, policy, labels, and the allowed capability set. Revoke it with
`POST /api/v1/nodes/{node_id}/revocation`. Revocation is retained in the
database, rejects later handshakes, and asks the transport to close the current
connection. The database transaction also creates durable convergence work.
The worker retries an exact revision until transportd reports `applied` with
the same retained state and revision; a stale or rejected result remains
pending. Revocation closes the connection only after that exact tombstone was
applied. Controller ingress independently verifies the event's node and
EndpointID against active database trust, so an event from a lingering revoked
session is rejected during convergence.

The Agent handshake advertises supported capabilities. The Controller computes
the sorted intersection with the approved database set and returns only that
negotiated subset. Supporting extra capabilities does not prevent enrollment or
connection. Handshake protocol `1.1` signs the negotiated subset, EndpointID,
node ID, authorization revision, and expiry in `SessionGrantV1`; protocol `1.0`
is limited to approved read-only capabilities.

Enable the trust service on worker or all roles by setting both:

```text
OCSERV_CONTROLLER_ENDPOINT_ID=<64 lowercase hex characters>
OCSERV_TRUST_SOCKET=/run/ocserv-trust/control-plane.sock
```

Set `OCSERV_TRANSPORT_UID` and `OCSERV_TRANSPORT_GID` to transportd's exact
numeric identity. Production rejects a missing transport identity or a
transport UID equal to the Controller UID. The trust-socket directory must be
owned by the control-plane process and must not be group- or world-writable.
Keep it separate from the transport socket directory so a compromised
transport process cannot replace the trust service.

The endpoint value must match the public identity derived from the controller
key used by `ocservia-transportd`. The trust socket's parent directory must
already exist and be restricted to the service users. Both UDS peers verify the
other process's exact UID, socket UID/GID/mode, trusted non-writable ancestry,
and stable device/inode identity. Shutdown removes only the socket instance the
server created.

Rollback requires restoring explicit startup endpoint bindings in transportd
before disabling the trust service. Migration
`000018_enrollment_revocation_trust` removes durable convergence work when
reversed; do not roll it back while a trust transition is pending. Migration
`000004_enrollment_trust` is destructive in reverse and must not be rolled back
unless enrollment history and bindings have been preserved or the enrolled
nodes will be enrolled again with new endpoint keys.
