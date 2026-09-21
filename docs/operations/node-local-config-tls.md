# Node-local TLS Configuration Profile

The matched complete profile uses `ocserv.config.complete.plan` and
`ocserv.config.complete.apply`. Older nodes reject it; there is no fallback to
the incomplete legacy plan. Native acceptance is required before this profile
can close the T07 positive-apply exclusion. See the
[reviewed contract](../development/complete-config-contract.md).

## Provisioning

Provision a dedicated unprivileged `ocservia-vpn` user whose primary and only
group is `ocservia-vpn`. The profile fixes ocserv's worker identity to that
account; it does not change Agent or privd identities/capabilities.

Keep the server certificate and private key on the node. Root owns the source
files with one link, no symlinks and non-writable ancestry. The key is `0600`;
the public certificate is `0600` or `0644`. An explicit `CA:FALSE`, valid
server-auth certificate must match the private key. Optional CA material must
be a valid `CA:TRUE` certificate. Certificate and SPKI hashes below cover only
public DER, never private key bytes.

Register a SecretRef through `POST /api/v1/secret-provider-refs` using provider
`node-local-tls-v1`, an immutable version and public `key_path` metadata:

```text
<node UUID>/<certificate DER SHA-256>/<SPKI DER SHA-256>/<CA DER SHA-256 or none>
```

Use lowercase hex digests. The returned reference UUID is the bundle identity.
As the node root operator, run the shipped source-tree provisioning helper:

```sh
bash deploy/managed-node/provision-config-tls.sh \
  NODE_UUID REFERENCE_UUID VERSION CERTIFICATE PRIVATE_KEY [CA_CERTIFICATE]
```

The helper refuses replacement of an existing version. It writes a protected
manifest and `0400 root:root` files under the fixed
`/etc/ocservia-agent/config-tls/<reference UUID>/<version>` directory, publishes
the directory atomically and shares a resource lock with root plan/apply.
Its output is public reference metadata only. Retain old versions for rollback;
do not edit/delete version contents or bypass the provisioning lock.

Before starting a newly provisioned ocserv, configure the finite profile with
the bundle's exact `server-cert.pem`, `server-key.pem` and optional `ca-cert.pem`
paths, the dedicated worker identity, plain authentication and chosen ports.
Validate the complete config with `ocserv -t -c /etc/ocserv/ocserv.conf`, keeping
that file `0600 root:root`. For an already running server, initial profile
activation or changing TLS reference/version requires a separately authorized
maintenance restart. Do not pretend a HUP changes startup-only settings. No
Agent/privd command performs that restart. Retain the old config/TLS bundle and
restore both under the same maintenance procedure if activation fails.

## Plan And Apply

Use the same reference UUID for `server-cert`, `server-key` and optional
`ca-cert`; the root resolver selects each fixed slot. The API resolves the
current reference version and pins it in the command. An already created plan
does not follow later SecretRef rotation. Plan/apply rejects a TLS path/version,
listener port, authentication, worker identity or socket change relative to the
protected active config. Includes, vhosts, duplicate directives and an active
config outside the finite grammar fail closed. Operator changes to startup
bindings require a verified restart before resuming automated config work.

The finite profile requires `auth`, `cookie-timeout`, `device`, `dns`,
`ipv4-network`, `max-clients`, `max-same-clients`, `server-cert`, `server-key`,
`socket-file`, `tcp-port` and `udp-port`. Optional `route` is one canonical IPv4
network or `default`. `auth` and `socket-file` have fixed production paths;
TLS paths, includes and executable directives cannot be supplied by callers.

The node detail form submits the complete profile. A valid plan has no partial
validation warnings and includes a logical candidate hash, exact materialized
file hash, previous file hash, revision and expiry. A separate principal
reviews that immutable binding before apply. Replays cannot authorize a new
operation. Root revalidates TLS and the complete native config before atomic
publication and reload; failed health checks restore exact old file bytes.

Never turn an uncertain apply into a new request or manually edit its journal,
effect store or Controller records. Preserve exact command/hash/fence/receipt
evidence and follow the [stable recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
