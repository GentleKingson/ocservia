# Rebind an Agent to another Controller

Run the packaged `ocservia-agent-rebind` CLI locally on the managed node as
root when moving an Agent to another Controller during planned maintenance or
recovery. Rebinding preserves the local Agent endpoint key, ocserv users,
configuration, certificates, sealing keys, and historical recovery databases on
the host. Because the operation does not copy or migrate Controller data, the
target Controller sees a brand-new pending node with no history from the
previous one; rebinding is not a high-availability or automatic failover
mechanism, and an operator must still approve the node on the target
Controller.

Before starting, install a matched release that includes the rebind CLI, Agent,
privd, and upgrader together on a host with Python 3, OpenSSL, and util-linux.
The node must use the packaged systemd launchers and their standard state paths
(custom launchers or state arguments are refused because their recovery
evidence cannot be inferred safely), and the initial `agent.env` must retain
its independently verified `AGENT_ENDPOINT_ID` enrollment binding. Once a node
has been rebound, bootstrap installation reruns refuse it, so future upgrades
must use the native package lifecycle.

Independently provision the target Controller EndpointID and Ed25519 command
verification public key on the host, storing the key and bootstrap token as
protected root-owned regular files under trusted root-owned parent directories.
The existing relay configuration must already reach the target Controller
through a single dedicated Relay, with `RELAY_URL_B` omitted or left empty in
`relays.env`. If `RELAY_URL_B` is non-empty, `prepare` (including `--dry-run`),
`resume`, and any uncommitted operation will abort before enrollment, operation
creation, or service shutdown, whereas `status` inspection and post-publication
recovery remain available.

1. If the old Controller is still reachable, revoke its node record using the
   standard independently approved
   [node revocation flow](enroll-node.md#revoke-a-node). When the old
   Controller is permanently unavailable, declare that state with
   `--source-disposition unreachable` in step 3; contacting the old Controller
   is not required, and the CLI records the disposition you declare without
   probing it.
2. On the new Controller, create a short-lived bootstrap token via
   `POST /api/v1/node-bootstrap-tokens` with `expected_endpoint_id` set to the
   existing Agent EndpointID so the Controller checks that binding before
   consuming the token. Rebinding uses the existing possession proof and
   recoverable `obt1_` token protocol (no other token type is accepted) and
   always assigns a new NodeID. The target must be a different Controller with
   a different command key, its `environment` must match the token's, and its
   database must have no record of this EndpointID in any state, as a
   Controller that has previously held the endpoint (even in `revoked` state)
   refuses the enrollment.
3. Prepare the rebind and enroll with the target Controller, supplying a reason
   and the source disposition:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind prepare \
     --controller <target-controller-endpoint-id> \
     --command-key-file /etc/ocservia-agent/new-controller.pub.pem \
     --token-file /etc/ocservia-agent/new-controller.token \
     --environment production \
     --source-disposition revoked \
     --reason 'Replace the retired Controller'
   ```

   Pass `--dry-run` to validate host prerequisites without enrolling or
   publishing authority changes. During a normal run, `prepare` prints
   `PREPARED` along with a local operation UUID before contacting the
   Controller, followed by `ENROLLED_LOCAL` and the new pending NodeID.
4. Complete the standard
   [approval and activation procedure](enroll-node.md#approve-the-node) on the
   target Controller. The node must be active before `commit` can verify a
   session, and bootstrap token possession does not authorize approval (if the
   enrollment response in step 3 is still unknown locally, recover it before
   approving the node). Privileged mutations also require registering the
   node's privd attestation key and approving `privd_result_attestation_v1`
   ([production deployment](../operations/production-deployment.md)); until
   that attestation is approved, the node remains readable while privileged
   mutations fail closed.
5. Commit the confirmed operation:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind commit <operation-uuid>
   ```

Running `commit` stops the Agent, privd, and any active upgrade runners,
publishes a single root-owned authority record, and starts privd before the
Agent. It then waits up to about a minute for a signed session grant from the
new Controller and verifies it independently through privd before recording
`VERIFIED` and marking the source binding sealed in the operation record while
leaving the historical source files in place. Because the authority selector
covers the Controller endpoint, command key, new NodeID, and all derived
durable namespaces, an old Controller that comes back online later cannot
regain authority.

## Resume and inspect

```sh
sudo /usr/libexec/ocservia/ocservia-agent-rebind status <operation-uuid>
sudo /usr/libexec/ocservia/ocservia-agent-rebind resume <operation-uuid>
```

Use `resume` when preparation or enrollment is interrupted before authority
publication. Because a response timeout leaves the original token and identity
in place, rerunning `resume` recovers that token's same pending node on the
target Controller; do not create a second token or node record to work around
an unknown response. Only one unfinished operation is permitted at a time.
Supplying an incorrect pin, key, or token leaves the source authority
untouched, and incomplete filesystem staging is preserved for diagnosis and
refused instead of being repaired by generating a new endpoint key.

Once `commit` has published the authority record, rerun `commit` to retry
service startup and session verification. If restarting services or verifying
the session fails at that stage, the target binding remains committed with the
source evidence intact, and the node never reverts automatically to the old
Controller; resolve the target approval, network connectivity, or host health
issue and rerun `commit`. A stale operation cannot override a newer binding,
and package upgrades and rollbacks share the lifecycle lock and refuse binaries
that cannot enforce an existing binding.

Local operation records are stored under
`/var/lib/ocservia-rebind/<operation-uuid>` and must be kept alongside the
source databases for recovery. If `commit` finds unfinished commands in the
Agent journal, a missing journal, prepared privileged effects, incomplete
upgrades, or unreadable evidence when publishing the binding, it quarantines
privileged mutations for the target Controller while still permitting read-only
operation and new authority verification. The CLI provides no command to clear
that quarantine; preserve and reconcile the underlying records, and never alter
file timestamps or delete a state database to clear quarantine.

Rebind performs no historical cleanup; see the
[rebind contract](../development/controller-rebind.md) for the full safety and
retention boundaries.

## Independent local retention

Package installation enables a single host-wide
`ocservia-agent-retention.timer` that runs hourly (inspect failures with
`journalctl -u ocservia-agent-retention.service`). To override the default
windows, create a root-owned `/etc/ocservia-agent/retention.env` with mode
`0600`:

```ini
OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS=30
OCSERV_REBIND_HISTORY_RETENTION_DAYS=365
```

Permitted values range from 7 to 180 days for
`OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS` and from 90 to 2555 days for
`OCSERV_REBIND_HISTORY_RETENTION_DAYS`; zero, negative, unknown, or duplicate
settings are rejected as errors. Every run validates `retention.env` before
touching disk, and you can run
`sudo /usr/libexec/ocservia/ocservia-agent-retention` directly to validate the
file and execute one bounded cleanup pass. The worker compacts ordinary retired
command results and expired rebind details while preserving keys, authority
tombstones, privileged proofs, revision and fence floors, and all unresolved
recovery evidence. Changing retention settings never clears a mutation
quarantine.
