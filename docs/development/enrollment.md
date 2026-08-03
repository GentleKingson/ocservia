# Node enrollment development

Enrollment is explicit and does not activate a node. An authenticated operator
first creates a token with `POST /api/v1/enrollment-tokens`. The response is
marked `Cache-Control: no-store` and returns the plaintext token once; only its
SHA-256 digest is stored. Tokens expire after at most 15 minutes.

The Agent keeps its endpoint key in an owner-only local directory and pins the
controller EndpointID. Reusing the directory preserves the Agent EndpointID;
attempting to provision it with a different controller pin is rejected. The
enrollment ALPN submits the token and public Agent metadata and returns a
pending node ID. Pending nodes cannot use the agent ALPN.

Approve the node with `POST /api/v1/nodes/{node_id}/approval`, including a
non-empty reason, policy, labels, and the allowed capability set. Revoke it with
`POST /api/v1/nodes/{node_id}/revocation`. Revocation is retained in the
database, rejects later handshakes, and asks the transport to close the current
connection. Approval and revocation retries are idempotent so transport
synchronization can be retried after a temporary UDS failure.

Enable the trust service on worker or all roles by setting both:

```text
OCSERV_CONTROLLER_ENDPOINT_ID=<64 lowercase hex characters>
OCSERV_TRUST_SOCKET=/run/ocserv-platform/control-plane-trust.sock
```

The endpoint value must match the public identity derived from the controller
key used by `ocservia-transportd`. The trust socket's parent directory must
already exist and be restricted to the service users.

Rollback requires restoring explicit startup endpoint bindings in transportd
before disabling the trust service. Migration `000004_enrollment_trust` is
destructive in reverse and must not be rolled back unless enrollment history
and bindings have been preserved or the enrolled nodes will be enrolled again
with new endpoint keys.
