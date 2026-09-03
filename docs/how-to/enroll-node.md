# Enroll a node

Enrollment binds a managed node's persistent EndpointID to a Controller node
record. It does not activate the node; an operator must approve it afterward.

## Before you begin

- The Agent package is installed on the node.
- You know the Controller EndpointID and the target workspace UUIDv7.
- Two distinct sealing private keys are provisioned on the node, and you have
  their key IDs and public-key SHA-256 descriptors.
- For production, the dedicated relay drop-in is installed, both
  `RELAY_URL_A` and `RELAY_URL_B` are exported, and
  `/etc/ocservia-agent/relay-access-token` is provisioned.
- You have an authenticated requester API client with permission to create an
  enrollment token and request node approval.
- A different authorized principal is available to independently approve the
  node approval request.

## Steps

1. On the node, prepare its persistent identity. Keep the printed EndpointID.

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --prepare-enrollment
   ```

   This is offline preparation. It does not contact the Controller or start a
   session. Reusing the same identity directory preserves the EndpointID.

2. With an authenticated API client, create a short-lived token. Replace the
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
     --relay-url "$RELAY_URL_A" \
     --relay-url "$RELAY_URL_B" \
     --relay-token-file /etc/ocservia-agent/relay-access-token
   ```

   Enrollment signs and persists both public-key descriptors. Record the UUIDv7
   node ID printed by the command, then remove the token file. The descriptors
   are enrollment inputs; the returned value that belongs in `agent.env` is
   the new `NODE_ID`.

   Before approval, return to [Configure the Agent after enrollment](../getting-started/managed-node.md#7-configure-the-agent-after-enrollment).

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
service starts — enable both services as in step 8 of [Install a managed
node](../getting-started/managed-node.md#8-approve-and-start-the-node); the
bootstrap and this enrollment never start or enable a service themselves.

## Troubleshooting

- A token is one-time and short-lived. Create a new token instead of reusing a
  failed or expired one.
- The expected EndpointID, Controller EndpointID, and identity directory must
  match. A changed controller pin or replaced identity directory is a new
  trust decision.
- A pending node is not mutation-capable until it is approved.

## See also

- [Install a managed node](../getting-started/managed-node.md)
- [Node enrollment reference](../development/enrollment.md)
