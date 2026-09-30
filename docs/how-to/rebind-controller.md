# Rebind an Agent to another Controller

Use the packaged `ocservia-agent-rebind` command locally as root. This is a
maintenance operation; it preserves the Agent endpoint key, ocserv users,
configuration, certificates, sealing keys and historical recovery databases.
It does not copy Controller data or grant node approval.

Install a release containing the rebind CLI, Agent, privd and upgrader together.
The host requires Python 3, OpenSSL and util-linux. Use the packaged systemd
launchers and their standard state paths; custom launchers or state arguments
are refused because their recovery evidence cannot be inferred safely. Independently provision the
target Controller EndpointID and Ed25519 command verification public key. The
key and token files must be protected root-owned regular files under trusted
root-owned ancestry. Existing relay configuration must reach the target.
The initial `agent.env` must retain its independently verified
`AGENT_ENDPOINT_ID` enrollment binding. Bootstrap installation reruns refuse
rebound nodes; use the native package lifecycle for subsequent upgrades.

1. If the old Controller is available, revoke its node using the normal
   independently approved [node revocation flow](enroll-node.md). If it is
   permanently unavailable, record that fact with `--source-disposition
   unreachable`; contacting it is not a prerequisite.
2. Create a short-lived bootstrap token on the new Controller through
   `POST /api/v1/node-bootstrap-tokens`. Keep the same Agent EndpointID. Rebind
   uses the existing possession proof and recoverable `obt1_` token protocol;
   it does not reuse the old Controller's NodeID.
3. Prepare and enroll, supplying a reason and the source disposition:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind prepare \
     --controller <target-controller-endpoint-id> \
     --command-key-file /etc/ocservia-agent/new-controller.pub.pem \
     --token-file /etc/ocservia-agent/new-controller.token \
     --environment production \
     --source-disposition revoked \
     --reason 'Replace the retired Controller'
   ```

   Add `--dry-run` to validate prerequisites without enrollment or authority
   publication. Preparation prints a local operation UUID before contacting the
   Controller. It then prints `ENROLLED_LOCAL` and the new pending NodeID.
4. Complete the existing [approval and activation procedure](enroll-node.md)
   on the target, including its required privd attestation registration. Bootstrap
   possession does not authorize approval. Do not approve an enrollment whose
   response is still unknown locally; recover it first.
5. Commit the confirmed operation:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind commit <operation-uuid>
   ```

Commit stops Agent, privd and existing upgrade runners, publishes one root-owned
authority record, and starts privd before Agent. It verifies the new Controller's
signed session grant independently through privd before recording `VERIFIED`
and sealing the source binding. The selector covers the Controller endpoint,
command key, new NodeID and all derived durable namespaces. An old Controller
returning later does not regain authority.

## Resume and inspect

```sh
sudo /usr/libexec/ocservia/ocservia-agent-rebind status <operation-uuid>
sudo /usr/libexec/ocservia/ocservia-agent-rebind resume <operation-uuid>
```

`resume` is for preparation/enrollment. A response timeout retains the original
token and identity; retrying recovers that token's same pending node. Do not
create another token or node to resolve an unknown response. A second unfinished
operation is refused. Incorrect pins, keys or tokens do not replace the source
authority. Incomplete filesystem staging is retained for diagnosis and refused
instead of being repaired by generating an endpoint key.

After authority publication, repeat `commit` to retry startup and session
verification. Failure to restart or verify the session leaves the target
committed and the source evidence intact; it does not automatically restore the
old Controller. Resolve target approval, connectivity or host health before
retrying. A stale operation cannot override a later binding. Package upgrades
and rollback share the lifecycle lock, and refuse binaries that cannot enforce
an existing binding.

Local operation records live under `/var/lib/ocservia-rebind/<operation-uuid>`.
Keep these records and the source databases for recovery. Pending/running/Unknown
commands, prepared privileged effects, incomplete upgrades or unreadable evidence
quarantine target mutations. Read-only operation and new authority verification
remain possible. Preserve and reconcile those records; changing their age or
deleting a database must never be used to clear quarantine.

Rebind performs no historical cleanup. The safety and retention boundaries are
specified in the [rebind contract](../development/controller-rebind.md).
