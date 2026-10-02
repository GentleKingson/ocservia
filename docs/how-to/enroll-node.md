# Enroll a node

Enrollment binds a managed node's persistent EndpointID to a Controller node
record. It does not activate the node; an operator must approve it afterward.

A Bootstrap Token authorizes one narrowly scoped enrollment transaction: it
may create only a pending node bound to the presenting EndpointID. It does not
grant `node.approve`, select approved capabilities, activate the node, or start
services. Approval is a separate content-bound decision by a different
authorized principal.

## Before you begin

- The Agent package is installed on the node.
- You know the Controller EndpointID and the target workspace UUIDv7.
- Two distinct sealing private keys are provisioned on the node, and you have
  their key IDs and public-key SHA-256 descriptors.
- For production, the dedicated relay drop-in and launcher are installed,
  `RELAY_URL_A` is exported, `RELAY_URL_B` is omitted or empty, and
  `/etc/ocservia-agent/relay-access-token` is provisioned.
- You have an authenticated requester API client with permission to create a
  node bootstrap token and request node approval.
- A different authorized principal is available to independently approve the
  node approval request.

## Steps

1. With an authenticated API client, create a short-lived bootstrap token.

   ```http
   POST /api/v1/node-bootstrap-tokens
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "reason": "Bootstrap managed node"
   }
   ```

   The plaintext `obt1_` token is returned once and expires within 15 minutes.
   Place it in a protected file; do not put it in a URL, command argument, log,
   or shared shell history.

2. Give Stage-1 the protected source path and run the pinned installer. It
   prepares the identity, enrolls immediately, deletes the plaintext source
   after success, writes `agent.env` atomically, and stops at
   `ENROLLED_LOCAL` without enabling or starting a service.

   ```bash
   export BOOTSTRAP_TOKEN_SOURCE=/protected/node-bootstrap-token
   ./install.sh --version vX.Y.Z
   ```

   Record the printed UUIDv7 node ID and continue at [Approve the
   node](#approve-the-node).

### Advanced endpoint-bound enrollment

The existing two-run flow remains available when no bootstrap token source is
provided.

1. On the node, prepare its persistent identity. Keep the printed EndpointID.

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --prepare-enrollment
   ```

   This is offline preparation. It does not contact the Controller or start a
   session. Reusing the same identity directory preserves the EndpointID.

2. Create a short-lived endpoint-bound token. Replace the
   placeholders with the target workspace and the exact EndpointID from step
   1.

   ```http
   POST /api/v1/enrollment-tokens
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "expected_endpoint_id": "<64-lowercase-hex-endpoint-id>",
     "reason": "Enroll managed node"
   }
   ```

   The plaintext token is returned once and expires within 15 minutes. Do not
   put it in a URL, log, or shared shell history.

3. Copy the token to a root-owned file readable by the Agent group:

   ```bash
   sudo install -o root -g ocserv-agent -m 0640 \
     /protected/enrollment-token /etc/ocservia-agent/enrollment-token
   ```

4. Run enrollment with the same identity directory:

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --enrollment-token-file /etc/ocservia-agent/enrollment-token \
     --enrollment-environment production \
     --user-password-seal-key-id "$USER_PASSWORD_SEAL_KEY_ID" \
     --user-password-seal-public-key-sha256 "$USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256" \
     --p12-password-seal-key-id "$P12_PASSWORD_SEAL_KEY_ID" \
     --p12-password-seal-public-key-sha256 "$P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256" \
     --relay-mode custom \
     --relay-url "${RELAY_URL_A:?set the dedicated HTTPS relay URL}" \
     --relay-token-file /etc/ocservia-agent/relay-access-token
   ```

   Enrollment signs and persists both public-key descriptors. Record the UUIDv7
   node ID printed by the command, then remove the token file. The descriptors
   are enrollment inputs; the returned value that belongs in `agent.env` is
   the new `NODE_ID`, alongside the `AGENT_ENDPOINT_ID` binding recorded from
   the identity preparation.

   Before approval, return to [Finish enrollment](../getting-started/managed-node.md#3-finish-enrollment).

## Approve the node

5. As the requester, create a content-bound `node.approve` approval request
   only after the Agent configuration and production relay path are complete:

   ```http
   POST /api/v1/approval-requests
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "action": "node.approve",
     "resource_type": "node",
     "resource_id": "<node-uuidv7>",
     "reason": "Approve managed node",
     "ttl_seconds": 600,
     "node_approval": {
       "labels": {"environment": "production"},
       "policy": "<approved-node-policy>",
       "capabilities": ["<capabilities-approved-for-this-node>"]
     }
   }
   ```

   Record the returned UUIDv7 `id` as `APPROVAL_ID` and its
   `request_hash` as `APPROVAL_REQUEST_HASH`. The approval request's labels,
   policy, and capabilities are the exact activation content that the
   independent approver must review. Keep the activation reason consistent for
   audit clarity.

6. Have a different authorized principal approve that request. The approver
   must verify the request content before approving it and must bind the
   decision to the returned request hash:

   ```http
   POST /api/v1/approval-requests/<approval-uuidv7>:approve
   Content-Type: application/json

   {
     "reason": "Independent node approval review",
     "expected_request_hash": "<approval-request-hash>"
   }
   ```

7. As the requester, activate the pending node with the approved request ID:

   ```http
   POST /api/v1/nodes/<node-uuidv7>/approval
   X-Approval-ID: <approved-request-uuidv7>
   Content-Type: application/json

   {
     "labels": {"environment": "production"},
     "policy": "<approved-node-policy>",
     "capabilities": ["<capabilities-approved-for-this-node>"],
     "reason": "Approve managed node"
   }
   ```

   The `labels`, `policy`, and `capabilities` in this activation request must
   exactly match the values in the `node.approve` request above. Keep the
   activation reason consistent for audit clarity.
   Node activation always requires a valid approved UUIDv7 in
   `X-Approval-ID`; independent approval is not policy-optional. Use the
   capabilities required by the node's policy, not a broader set by default.

## Verify

The API response should report `status: active`. The node should then appear
online with a fresh observation in the Controller inventory after its Agent
service starts — enable both services as in step 4 of [Install a managed
node](../getting-started/managed-node.md#4-approve-and-start-services); the
bootstrap and this enrollment never start or enable a service themselves.

## Troubleshooting

- A token is one-time and short-lived. Create a new token instead of reusing a
  failed or expired one.
- If the Controller committed bootstrap enrollment but the response was lost,
  retry with the same token and the same persistent EndpointID. That exact
  replay returns the same pending node; a different EndpointID is rejected.
- The expected EndpointID, Controller EndpointID, and identity directory must
  match. A changed controller pin or replaced identity directory is a new
  trust decision.
- A pending node is not mutation-capable until it is approved.

## See also

- [Install a managed node](../getting-started/managed-node.md)
- [Node enrollment reference](../development/enrollment.md)

## Recover an uncertain enrollment response

Keep the original EndpointID, identity directory and protected token material when
an enrollment response is lost or SSH is interrupted. For a bootstrap token,
rerun the same installer with the original bootstrap binding; the existing
same-endpoint recovery can return an enrollment that the Controller already
committed. Do not create a new identity or authorization merely because the
client did not receive a response.

Legacy endpoint-bound enrollment tokens do not have that bootstrap replay
contract. An administrator must first find the node by its original EndpointID
in the Controller inventory before deciding whether a new token is required.
Approval expiry requires a fresh approval request bound to the current content
and an independent approver; it does not imply a new node identity.

`ENROLLED_LOCAL` confirms the persisted local Node ID; `SERVICES_ACTIVE` confirms
only both local units are enabled and active. The installer has no administrator
session and marks Controller trust, connection and freshness as `NOT_OBSERVED`.
On the Controller host, read `GET /api/v1/nodes/{node_id}` with the authorized
workspace/session and inspect `trust_status`, `connection_state` and `freshness`.
The approval/activation response's `status` describes a different transition.
Do not transfer that administrator session to the Node.

After confirmed local enrollment, the installer removes only its fixed protected
enrollment-token copy. A rerun finishes this cleanup after validating the existing
identity and binding, including after interruption between configuration commit
and cleanup. Unsafe metadata fails closed. The bootstrap source is removed only
by the successful enrollment invocation that used it; a rerun does not delete an
arbitrary newly configured source. Failed or uncertain enrollment retains recovery
material. Long-term identity, Relay token, public trust and sealing private keys
are retained. Power loss or SIGKILL can interrupt cleanup; rerun and inspect the
local summary. Enrollment copies remain root:ocserv-agent `0640`, so the Node can
read them; they are not converted to root-only files.
