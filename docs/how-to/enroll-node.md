# Enroll a node

Enrolling a managed node binds its persistent EndpointID to a node record on
the Controller in a pending state. Enrollment does not activate the node; an
operator must complete the separate approval workflow afterward.

A Bootstrap Token authorizes a single narrowly scoped enrollment transaction
that creates a pending node bound to the presenting EndpointID. Possessing the
token does not grant `node.approve`, select approved capabilities, activate the
node, or start local services; approval requires a separate content-bound
decision by a second authorized principal.

## Before you begin

- The Agent package is installed on the node.
- You have the Controller EndpointID and the target workspace UUIDv7.
- Sealing keys are ready for your chosen workflow: the standard installer
  generates any missing sealing keys automatically, whereas the advanced manual
  path requires two distinct sealing private keys already provisioned on the
  node along with their key IDs and public-key SHA-256 descriptors.
- For production deployments, the dedicated relay drop-in and launcher are
  installed, `RELAY_URL_A` is exported, and `RELAY_URL_B` is omitted or empty
  (only one dedicated Relay is supported, and the installer rejects any
  non-empty `RELAY_URL_B`). Ensure the relay access token is provisioned at
  `/etc/ocservia-agent/relay-access-token`, or set `RELAY_ACCESS_TOKEN_SOURCE`
  so the installer can install it.
- You have an authenticated requester API client with permission to create a
  node bootstrap token and request node approval (SecurityAdmin or
  PlatformAdmin).
- A second authorized principal is available to review and approve the node
  approval request independently.

## Steps

1. With an authenticated API client, create a short-lived bootstrap token.

   ```http
   POST /api/v1/node-bootstrap-tokens
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "reason": "Bootstrap managed node"
   }
   ```

   Specify a `workspace_id` that the caller is authorized to access; the
   `X-Workspace-ID` header may be omitted only when the caller belongs to
   exactly one workspace. The API returns the plaintext `obt1_` token once, and
   it expires within 15 minutes (`ttl_seconds` may only shorten that window).
   Including an optional `expected_endpoint_id` pre-binds the token so the
   Controller rejects any other presenting endpoint before consuming it. Write
   the token to a protected file, and never expose it in a URL, command-line
   argument, log, or shared shell history.

2. Point `BOOTSTRAP_TOKEN_SOURCE` at the protected token file and run the
   version-pinned Stage-1 installer. The installer prepares the local identity,
   enrolls immediately with the Controller, deletes the plaintext token source
   after enrollment succeeds, writes `agent.env` atomically, and stops at
   `ENROLLED_LOCAL` without enabling or starting any service.

   ```bash
   export BOOTSTRAP_TOKEN_SOURCE=/protected/node-bootstrap-token
   ./install.sh --version vX.Y.Z
   ```

   The token source must be a non-empty regular file (never a symlink) with no
   group or world permissions, and you must also supply the Controller, Relay,
   and command-key inputs listed in
   [Install a managed node](../getting-started/managed-node.md).

   Record the printed UUIDv7 node ID and continue at
   [Approve the node](#approve-the-node).

### Advanced endpoint-bound enrollment

When no bootstrap token source is provided, you can use the two-step
endpoint-bound enrollment flow.

1. On the node, prepare its persistent identity and record the printed
   EndpointID:

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --prepare-enrollment
   ```

   Identity preparation runs offline without contacting the Controller or
   opening a transport session, and reusing the same identity directory
   preserves the EndpointID across runs.

2. Issue a short-lived endpoint-bound enrollment token, replacing the
   placeholders with the target workspace and the exact EndpointID from step 1:

   ```http
   POST /api/v1/enrollment-tokens
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "expected_endpoint_id": "<64-lowercase-hex-endpoint-id>",
     "reason": "Enroll managed node"
   }
   ```

   The plaintext token is returned once and expires within 15 minutes. Follow
   the same workspace rule as step 1 of the bootstrap flow, and never place the
   token in a URL, log, or shared shell history.

3. Copy the token to a root-owned file readable by the Agent group:

   ```bash
   sudo install -o root -g ocserv-agent -m 0640 \
     /protected/enrollment-token /etc/ocservia-agent/enrollment-token
   ```

4. Run enrollment against the same identity directory:

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

   During enrollment, the Agent signs and persists both sealing public-key
   descriptors. Record the UUIDv7 node ID printed by the command and remove
   `/etc/ocservia-agent/enrollment-token`. The key descriptors are enrollment
   inputs; in `agent.env`, record the returned `NODE_ID` alongside the
   `AGENT_ENDPOINT_ID` binding from step 1.

   Before proceeding to approval, complete the remaining host configuration in
   [Finish enrollment](../getting-started/managed-node.md#3-finish-enrollment).

## Approve the node

5. Once the Agent configuration and production relay path are in place, use the
   requester account to create a content-bound `node.approve` request:

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

   Record the returned UUIDv7 `id` as `APPROVAL_ID` and its `request_hash` as
   `APPROVAL_REQUEST_HASH`. Because the independent approver reviews the exact
   `labels`, `policy`, and `capabilities` in this payload, request only the
   capabilities required by the node's policy.

6. Have a second authorized principal review the request content and approve it
   by binding the decision to the returned `request_hash`:

   ```http
   POST /api/v1/approval-requests/<approval-uuidv7>:approve
   Content-Type: application/json

   {
     "reason": "Independent node approval review",
     "expected_request_hash": "<approval-request-hash>"
   }
   ```

7. Using the requester account, activate the pending node with the approved
   request ID:

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

   The `labels`, `policy`, and `capabilities` in this activation payload must
   match the approved `node.approve` request exactly, and keeping the same
   `reason` string makes the audit trail easier to follow. Node activation
   always requires a valid approved request UUIDv7 in `X-Approval-ID`;
   independent approval cannot be bypassed by policy.

## Verify

A completed activation returns `status: active`. If the API responds with `503`
("Trust update pending"), the approval has already been committed in the
database while transport trust synchronization is still catching up; either
wait for background convergence or repeat the identical request with the same
`X-Approval-ID` (which is idempotent once activated) without creating a second
approval request. Because neither the bootstrap installer nor manual enrollment
enables or starts systemd units, enable and start both services as shown in
step 4 of
[Install a managed node](../getting-started/managed-node.md#4-approve-and-start-services),
then confirm that the node appears online with a fresh observation in the
Controller inventory.

## Revoke a node

Node revocation follows the same independent two-principal approval pattern as
activation and works from any non-revoked state, including a pending node. You
also use this flow on a reachable source Controller when rebinding a node to
another Controller.

1. As the requester (a principal holding `node.revoke`, such as SecurityAdmin
   or PlatformAdmin), create a content-bound revocation request:

   ```http
   POST /api/v1/approval-requests
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "action": "node.revoke",
     "resource_type": "node",
     "resource_id": "<node-uuidv7>",
     "reason": "Revoke managed node",
     "ttl_seconds": 600
   }
   ```

2. Have a different SecurityAdmin or PlatformAdmin verify and approve the
   returned request hash, just as in step 6 above.
3. As the requester, submit the revocation with the approved request ID:

   ```http
   POST /api/v1/nodes/<node-uuidv7>/revocation
   X-Approval-ID: <approved-request-uuidv7>
   Content-Type: application/json

   {"reason": "Revoke managed node"}
   ```

   The response reports `status: revoked`. If you receive a `503` ("Revocation
   committed") response, the revocation is already committed in the database
   and the trust convergence worker will finish synchronizing the transport
   disconnect in the background.

## Troubleshooting

- Enrollment and bootstrap tokens are single-use and short-lived. Issue a new
  token only after confirming that the previous token was rejected or expired;
  if the response was lost, follow the replay procedure below first.
- When the Controller commits a bootstrap enrollment and the response is lost,
  retrying with the same bootstrap token and persistent EndpointID returns that
  same pending node for as long as the node remains pending. The Controller
  rejects the replay if the presenting EndpointID differs or if the node has
  already been approved, and it refuses any bootstrap enrollment for an
  EndpointID already present in its database.
- The expected EndpointID, Controller EndpointID, and identity directory must
  stay aligned. Changing the Controller pin or replacing the identity directory
  represents a new trust decision.
- A pending node cannot execute privileged mutations until it has been
  approved and activated.

## See also

- [Install a managed node](../getting-started/managed-node.md)
- [Node enrollment reference](../development/enrollment.md)

## Recover an uncertain enrollment response

If an enrollment response is lost or your SSH session drops mid-run, keep the
original EndpointID, identity directory, and protected token material intact.
When using a bootstrap token, rerun the same installer with the original
bootstrap binding so the Controller's same-endpoint recovery path can return
the enrollment record it already committed. Never generate a new identity or
issue a new token simply because the client did not receive a response.

Legacy endpoint-bound enrollment tokens do not support that bootstrap replay
contract. Before deciding whether a new token is needed after an interrupted
endpoint-bound run, an administrator must look up the node by its original
EndpointID in the Controller inventory. Similarly, if an approval request
expires before activation, create a fresh approval request bound to the current
content and have an independent approver review it; an expired approval never
requires a new node identity.

Local installer states track only host-side progress: `ENROLLED_LOCAL` confirms
that the local Node ID has been persisted to disk, and `SERVICES_ACTIVE`
confirms only that both local systemd units are enabled and active. Because the
installer has no administrator API session, it reports Controller trust,
connection state, and telemetry freshness as `NOT_OBSERVED`. To check those
fields, query `GET /api/v1/nodes/{node_id}` from an authorized workspace
session on the Controller and inspect `trust_status`, `connection_state`, and
`freshness` (the `status` field returned by the approval/activation POST tracks
a different state transition). Never copy that administrator session onto the
managed node.

Once local enrollment is confirmed, the installer deletes only its fixed
internal copy of the enrollment token (which stays `root:ocserv-agent` with
mode `0640` so the Agent can read it, instead of being converted to a root-only
file). If power loss, `SIGKILL`, or another interruption stops the process
between configuration commit and cleanup, rerunning the installer validates the
existing identity and binding (failing closed on unsafe file metadata),
completes any pending token cleanup, and prints the local summary. The external
bootstrap token source file is deleted only by the enrollment invocation that
actually consumed it, so a later rerun will not delete an arbitrary newly
configured source path. Whenever enrollment fails or remains uncertain, the
installer retains recovery material along with the long-term identity, Relay
token, public trust files, and sealing private keys.
