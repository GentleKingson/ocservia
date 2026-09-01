# Enroll a node

Enrollment binds a managed node's persistent EndpointID to a Controller node
record. It does not activate the node; an operator must approve it afterward.

## Before you begin

- The Agent package is installed on the node.
- You know the Controller EndpointID and the target workspace UUIDv7.
- You have an authenticated API client with permission to create an enrollment
  token and approve a node.

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
     --enrollment-environment production
   ```

   Record the UUIDv7 node ID printed by the command, then remove the token
   file.

5. Approve the pending node with an authenticated API client:

   ```http
   POST /api/v1/nodes/<node-uuidv7>/approval
   Content-Type: application/json

   {
     "labels": {"environment": "production"},
     "policy": "<approved-node-policy>",
     "capabilities": ["<capabilities-approved-for-this-node>"],
     "reason": "Approve managed node"
   }
   ```

   Use the capabilities required by the node's policy, not a broader set by
   default. If the authorization policy requires independent approval, send
   the approved request ID in the `X-Approval-ID` header.

## Verify

The API response should report `status: active`. The node should then appear
online with a fresh observation in the Controller inventory after its Agent
service starts.

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
