# Integrated deployment

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag
(positive N without leading zeros). RCs are not recommended stable releases.

The P1 network prototype was developed against source baseline
[`e861b72130d4d409883f68d01805566f26cbafbc`](https://github.com/GentleKingson/ocservia/commit/e861b72130d4d409883f68d01805566f26cbafbc),
and initial P1/P3–P5 rollout and main acceptance records
([`368906c65d23bc7abff088dc4bbbdbf160012b32`](https://github.com/GentleKingson/ocservia/commit/368906c65d23bc7abff088dc4bbbdbf160012b32))
are archived at
[`af40cd3684269f7e31a3c8d6981775411a8172ec`](https://github.com/GentleKingson/ocservia/blob/af40cd3684269f7e31a3c8d6981775411a8172ec/deploy/production/integrated/README.md).
Read the
[Integrated deployment contract](../../../docs/development/integrated-deployment-contract.md).
Do not bypass the deployment-configuration checks in `controller.sh` for deployment.

## Lifecycle configuration

Use the existing bootstrap, `install.sh`, `controller.sh` and `compose.sh`,
not a second installer. Standalone remains the default. Integrated requires a
v2 platform manifest, Compose >= 2.24.4, and these additional `install.env`
settings alongside the normal database, authentication and Controller Secrets.
This section is the **manual** path: you write `install.env` and prepare every
file below. Quick (`controller-bootstrap.sh --quick`, see [Quick
mode](../../../docs/operations/bootstrap-hosting.md#quick-mode)) is a preset
over the same installer for a new host: it generates this configuration in
`/etc/ocservia/install.env` (Integrated, bundled PostgreSQL, ACME, Local
authentication) and the protected Controller and Signer material (see [Quick
install materials](../../../docs/development/certificates-and-signer.md#quick-install-materials)),
so only the manual path needs the file preparation described here. Manual
installs may use any supported database and login mode, and either TLS mode.

```dotenv
OCSERV_DEPLOYMENT_MODE=integrated
OCSERV_RELAY_PUBLIC_HOST=relay.example.com
OCSERV_RELAY_SECRET_DIR=/etc/ocservia/relay
OCSERV_SIGNER_SECRET_DIR=/etc/ocservia/signer
OCSERV_SIGNER_STATE_DIR=/var/lib/ocservia-signer
```

Integrated uses the existing explicit root lifecycle: pass `--root-lifecycle`
to bootstrap/install, and run subsequent lifecycle/Compose commands as root
with the same configuration. Provision launcher-owned paths for root. This
lets preflight inspect UID-65532-only Signer material without broadening its
permissions or adding a privileged helper. Non-root Integrated launch is
rejected before host bootstrap; standalone launcher behavior is unchanged.

Set `OCSERV_PUBLIC_HOST` to a distinct lowercase Controller DNS name. Omit
`OCSERV_RELAY_URL_A`, `OCSERV_RELAY_URL_B` and `OCSERV_CERTIFICATE_SIGNER_URL`:
the launcher derives one Relay URL and `https://signer:9443/sign`. Conflicting
values are rejected, and `OCSERV_RELAY_URL_B` must be empty: exactly one Relay is
supported, not a redundant pair. Controller's trusted proxy must remain the Gateway's
application `/32`, not Edge or an entire subnet. By default (`OCSERV_TLS_MODE=manual`)
certificates are provisioned externally; see [ACME certificates](#acme-certificates)
for the alternative. In a manual installation the operator supplies the Signer's
CA chain, issuing key and TLS identity: neither the lifecycle nor `compose.sh`
generates CAs. Only Quick generates them (`quick-materials.sh`: offline root CA,
online issuing intermediate and Signer HTTPS identity); it creates no Gateway or
Relay TLS identity either, ACME obtains those.

The Relay secret directory is launcher-owned mode 0700 and contains nonempty
single-link `tls.crt` and `tls.key` (manual mode only), launcher-owned mode 0444. Its access token
is the existing Controller `relay-access-token`, not a second Relay token.
Signer secret and state directories are UID:GID 65532:65532, mode 0700,
canonical paths with protected ancestry. Signer requires `issuer-chain.pem`,
`issuer-key.pem`, `tls-cert.pem`, `tls-key.pem` and `api-token`, single-link
65532:65532 mode 0400 files. `tls-ca.pem` has the same ownership and mode 0444
so Controller can read its individual public-CA mount. Signer's TLS leaf must
include SAN `signer`. `api-token` must exactly match Controller's
`certificate-signer-token`; no private issuer key is mounted into Controller.
See the [Signer custody contract](../../../docs/development/certificates-and-signer.md#production-signer)
for chain, token, approved key-transfer and recovery requirements.

Installation validates the deployment configuration and its source checkout,
checks the final Compose model and pulls all selected images before stopping
anything. Only first installation attempts `signer init`; its durable intent
is recorded before execution. Retry, start, upgrade and rollback inspect an
existing ledger and never recreate a missing ledger. An interrupted init with
missing/invalid state requires reconciliation, not removal of its intent file.
Mode and database selection are retained in `deployment-profile.json`; an
existing deployment cannot silently switch mode or database via environment.

Signer inspection is exclusive: lifecycle stops Controller/transportd/Signer
and retains `signer-checkpoint.json` (issuer, policy, state version and revision)
before activation. A different identity or lower revision fails closed, leaving
recoverable stopped state. This checkpoint is a lifecycle high-water mark,
not a live per-request backup; backup/restore still requires independently
reconciled newer issuance/revocation evidence. Rollback changes images, never
the ledger, CA, node identity or uncertain operation state.

`uninstall` stops the integrated services but preserves Secrets, ledger and
lifecycle evidence. Integrated `--purge-data` is refused; identity disposal is
a separate reconciled operator action. There is no automatic cross-mode
migration, automatic CRL distribution, HA or forced disconnection of existing
VPN sessions.

### ACME certificates

`OCSERV_TLS_MODE=acme` (Integrated only; `compose.sh` rejects it in Standalone,
and Quick always uses it) replaces both host certificate pairs:

```dotenv
OCSERV_TLS_MODE=acme
OCSERV_ACME_EMAIL=ops@example.com
```

Gateway (Caddy) and Relay (iroh-relay) each obtain and renew a certificate for
their own name from Let's Encrypt (the default directory) with TLS-ALPN-01. Edge already routes the
validation connection by SNI, so port 443 stays the only TCP port; nothing
listens on port 80. Both names must resolve to this host before installation.
`tls.crt` and `tls.key` are then neither required in `OCSERV_SECRET_DIR` nor in
`OCSERV_RELAY_SECRET_DIR`; the Relay secret directory itself must still exist
(it may be empty) with the same ownership and mode. Account keys and certificates live in the named
volumes `gateway-acme` and `relay-acme`; keep them across upgrades to stay
within issuance rate limits. Gateway joins an `acme-egress` network to reach
the ACME directory, so unlike manual mode it has outbound access.

`OCSERV_ACME_DIRECTORY_URL` selects another https ACME directory, and
`OCSERV_ACME_CA_FILE` (launcher-owned mode 0444, requires the directory URL)
names the CA that serves it, for a private ACME CA or a test server such as
Pebble. The first installation records ACME in `deployment-profile.json`; the
TLS mode cannot change afterwards, and releases without ACME refuse that
profile instead of starting without certificates.

## CI and publication

Manual [Release Check](../../../docs/development/release-checks.md) runs Full CI,
Security, release products (build, Controller image security and
package/image smoke), Integrated Business Smoke with four finite
single-instance recoveries, and Quick Install against a Pebble ACME directory
on merged main, each on native amd64 and arm64. Business uses fresh local
builds and a loopback registry on disposable runners of each architecture;
it does not publish GHCR images. The operator confirms the version after PASS.
Its tag then triggers fresh native builds, exact Controller image security scans,
install/image smoke on amd64 and arm64, and ordinary
Release/GHCR publication using version tags. Manual Release dispatch builds
both architectures without publishing.

Image scans retain the three explicitly approved Oracle FIPS-channel false
positive entries for the `mysql_backup` image in [the existing exemption file](../image-scan-exemptions.json),
each with review deadline `2026-10-26` (an expired entry fails the gate again);
other findings remain gated.

## Network contract

```text
public TCP443 -> Edge:8443 (SNI only, PROXY output)
  Controller name -> Gateway:8443 (PROXY then TLS) -> control-plane:8080
  Relay name      -> Edge loopback:9443 (strip PROXY) -> Relay:8443 (TLS)
public UDP7842 -----------------------------------> Relay:7842
unknown/missing SNI or plaintext -> closed connection
```

Unlike the P0 candidate's two loopback branches, Controller needs no second
hop: only Relay uses the strip listener. This avoids re-encoding client IP at
another hop. Edge has no TLS keys, Docker socket, privileged capabilities or
host networking. It runs as 65532, root filesystem read-only, with an 8 MiB
private `/tmp`. Internal ports 9443/9444 bind only loopback.

Only Edge publishes TCP443 and Relay UDP7842. `!override` removes inherited
Gateway ports, not an empty merge list. Compose >= 2.24.4 is required.
Resolve all paths relative to `deploy/production/compose.yaml`, the first file;
apply the Integrated overlay after the selected database/auth overlays, and
only the ACME overlays after it.
The launcher checks mode/image/hostname inputs and the merged published-port
set, adds `compose.signer.yaml`, and preserves the standalone path.

Render for review only, with the ordinary required environment plus
`OCSERV_EDGE_IMAGE`, `OCSERV_RELAY_IMAGE`, `OCSERV_RELAY_PUBLIC_HOST` and
`OCSERV_RELAY_SECRET_DIR` provisioned:

```bash
docker compose -f deploy/production/compose.yaml \
  -f deploy/production/compose.postgres.yaml \
  -f deploy/production/integrated/compose.yaml config
```

Both lowercase DNS names must be distinct; Edge validates labels before
restricted environment substitution. Caddy's pinned standard build includes
`caddy.listeners.proxy_protocol` without a custom module: Gateway allows the
Controller Host with no port or `:443`, executes an explicit `route` block to
reject authority mismatches before any API or static handler, preserves
API/static fallback/security headers and enables only HTTP/1.1 and HTTP/2, not
HTTP/3. Gateway accepts PROXY metadata only from Edge's dedicated `/32`
(`OCSERV_EDGE_GATEWAY_IP`, default `172.30.241.2`). Reserve that address outside
the dynamic allocation range; the subnet/range overrides must remain disjoint
from other networks. Controller still trusts its existing Gateway application
address, not the new Edge or an entire subnet.

Gateway/Relay names resolve through Docker DNS every five seconds for new
connections. Recreation is disruptive: existing streams can break and clients
must reconnect. Edge defaults to one worker, 2048 worker connections, 8192
file descriptors, 64 MiB and 35-minute stream inactivity timeout. Periodic SSE
heartbeats keep active streams open and do not test a 35-minute completely idle
Edge timeout. A relayed connection consumes multiple descriptors/hops. The
32-stream fixture is a bounded sample, not a 2048-client, latency or HA promise.

The Relay service reuses its production image, entrypoint, configuration and
health check. Its TLS files come from `OCSERV_RELAY_SECRET_DIR`; the shared
Relay access token uses the existing Controller `relay_access_token` Secret,
also mounted by transportd. No token or TLS key is mounted into Edge.
Relay requires exact SNI at Edge and protocol-level token authentication (the
locked Relay client returns `ServerDeniedAuth` with `not authorized` on a wrong
token rather than relying on HTTP Host admission on `/healthz`).
Relay has a separate network from Gateway. Relay TCP peer logs see Edge, not
the original IP. The locked Relay's receive limiter wraps individual client
streams; this is not proof of original-IP abuse controls behind Edge.

## Reproduce the bounded check

Run locally in an authorized isolated checkout with Docker and the required
build dependencies. Development builds are allowed here; current publication qualification uses manual Release Check
and freshly built local images on disposable native runners. NGINX 1.30.5 and Caddy 2.11.4 are digest-pinned; Relay uses the
existing locked iroh-relay 1.2.0 build. No upstream Relay changes are made.

```bash
docker build -f deploy/production/edge.Dockerfile -t p1-edge .
docker build -f deploy/production/relay.Dockerfile -t p1-relay .
python3 scripts/test-integrated-network.py \
  --edge-image p1-edge --relay-image p1-relay \
  --artifacts /absolute/new/evidence-directory
```

The script needs Docker Compose, Python 3 and OpenSSL. It constructs disposable
test certificates, networks and containers, uses a synthetic HTTP/SSE backend
and static page, and runs the real Caddy and Relay binaries. It does not run
the real Controller Web/login/Agent workflow. Host bindings use loopback with
ephemeral ports, not the server's public 443/7842. Two client containers have
distinct bridge addresses; they are not two external hosts. The reserved
fixture subnets `198.18.91.0/24` and `198.18.92.0/24` must be free. Resources are removed on exit;
sanitized results and logs remain in the requested evidence directory.

The [manual/branch Actions workflow](../../../.github/workflows/integrated-network.yml)
runs this same bounded check on native AMD64 and ARM64 without publishing
images. Two independent architecture jobs are not a shared-public-endpoint
test. Artifacts state their scope and retain NOT_RUN items.

For an explicitly authorized disposable public endpoint, add
`--serve-public-until /absolute/stop-file` to the fixture invocation. This binds
TCP443 and UDP7842 for at most 15 minutes, then cleans up. The caller must arrange
and restore authorized host/cloud firewall rules; the script does not change
them. Pass the endpoint IP and the public `ca_pem` from `public.json` to the
workflow's `public_address` and `public_ca_pem` inputs. Never upload private keys
or Relay tokens. Compare the two resulting observed public client IPs; two
runner labels alone are not proof of distinct sources. This uses explicit IP
routing with test-domain SNI, not public DNS or public certificate issuance.

The `relay-network-probe` target in `rust/test-runtime.Dockerfile` accepts
`URL CA_FILE TOKEN_FILE`. It uses the locked Relay client to check a valid TCP
upgrade and an explicit bad-token rejection, then runs QUIC address discovery
with HTTPS fallback probes disabled. Mount only its test CA/token, map the test
hostname to the authorized public IP, and run it from the same-host egress
network. A timeout is not an authentication rejection or a QUIC pass.

From the repository root, build and run this independent diagnostic on the
authorized disposable host. Set `RELAY_EGRESS_NETWORK` to the same-host egress
network, `RELAY_HOST` to the test certificate's hostname, `PUBLIC_IP` to the
authorized endpoint, `RELAY_URL` to its HTTPS URL, and the two file variables to
absolute paths of the public test CA and private test token:

The diagnostic runs as UID/GID 65532. Use a task-owned test-token copy owned by
65532:65532 with mode 0400, and a public CA file readable by that UID.

```sh
docker build -f rust/test-runtime.Dockerfile --target relay-network-probe \
  -t ocservia-relay-network-probe:test .
docker run --rm --network "$RELAY_EGRESS_NETWORK" \
  --add-host "$RELAY_HOST:$PUBLIC_IP" \
  -v "$RELAY_CA_FILE:/test-ca.pem:ro" -v "$RELAY_TOKEN_FILE:/test-token:ro" \
  ocservia-relay-network-probe:test "$RELAY_URL" /test-ca.pem /test-token
```

This manual diagnostic is not selected single-node recovery acceptance and
does not establish a real Agent command result.

The real single-node command and recovery chain now belongs to the signed
Business Smoke / Integration environment. Use its optional `run-resilience`
input; see [resilience coverage](../../../docs/development/resilience.md).
The former G6 engineering real-node entrypoint and its public-endpoint mode
are retired.

## Operator lifecycle and maintenance

Choose `standalone` for separately operated Relay/Signer endpoints, or
`integrated` for this single-host topology. Integrated is not HA. Use the
[pinned release installation](../../../docs/getting-started/production.md)
with `--version vX.Y.Z --root-lifecycle` only after that exact stable or RC Release
exists; on a new host, `--quick` (which implies `--root-lifecycle`) is the
alternative preset, available only in a Release that ships
`deploy/production/quick-install.sh`. For authorized nonproduction diagnostics, use the matching source
checkout and ordinary platform configuration with `controller.sh` instead.

`install.env` is data parsed by the installer's strict allowlist, not a shell
script. Do not `source` it. Direct lifecycle commands require the same explicit
operator environment used for installation, including database/auth settings,
Secret directories, public names, identity and runtime trust. Keep it in
the protected operator session; do not print it into acceptance logs.

```bash
# Root operator session, clean checkout at the configuration's source_commit.
deploy/production/controller.sh install --release-file "$MANIFEST"
deploy/production/controller.sh start
# From the next exact source checkout, using its validated platform configuration:
deploy/production/controller.sh upgrade --release-file "$NEXT_MANIFEST"
# From the current release checkout, with the previous commit available locally:
deploy/production/controller.sh rollback
```

Upgrade verifies, preflights and pulls before stopping services. Activation,
container recreation and rollback interrupt requests and streams; reconnect
clients after readiness returns. A failed activation retains current/pending
evidence. Diagnose the failure before retrying the same verified target; do
not delete pending state, replay an Unknown operation, initialize another
ledger, or automatically roll back potentially committed effects. A changed
database/deployment contract or insufficient backup/recovery state can refuse
rollback. Retain both source commits and bundles until the recovery window ends.

After first login, enroll and independently approve the real node. Export its
two distinct public sealing keys locally and the consumed approval binding
from Controller using the [controlled transfer procedure](../../../docs/development/certificates-and-signer.md#trusted-public-key-transfer).
Private node keys stay on the node. Stop affected mutations and Signer before
import, then restart and verify both sealed-password and P12 business paths.
Use a separate requester and approver principal, not self-approval.

For exclusive Signer maintenance, quiesce mutations and reconcile in-flight
results first. The existing non-purging uninstall is a full maintenance stop;
it preserves database volumes, Secrets, ledger and lifecycle evidence. This
also interrupts the public Controller and Relay, not just signing.

```bash
deploy/production/controller.sh uninstall
SIGNER_IMAGE="$(jq -er '.images.signer' \
  "$OCSERV_CONTROLLER_STATE_ROOT/current-release.json")"
# BACKUP_DIR is a new, canonical, protected directory, owned 65532:65532 mode 0700.
signer_admin() {
  docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true \
    -v "$OCSERV_SIGNER_SECRET_DIR:/run/secrets:ro" \
    -v "$OCSERV_SIGNER_STATE_DIR:/var/lib/ocservia-signer" \
    -v "$BACKUP_DIR:/backup" "$SIGNER_IMAGE" "$@"
}
umask 077
signer_admin inspect > "$BACKUP_DIR/ledger-inspect.json"
signer_admin backup --output /backup/snapshot.db
signer_admin inspect --state /backup/snapshot.db > "$BACKUP_DIR/snapshot-inspect.json"
```

The image reference above comes from the already verified protected lifecycle
manifest; do not replace it with a mutable tag. Keep the CA fingerprint,
snapshot checksum and inspection record outside the ledger volume. Back up
the issuing key separately under its existing custody policy, and take the
corresponding supported database backup. Never upload private material as CI
evidence. Failed or uninspected snapshots are not accepted backups.

To rehearse recovery while still stopped, set `MINIMUM_REVISION` from the
independently reconciled live high-water mark and later issuance/revocation,
import and disable evidence, not merely from the snapshot being restored:

```bash
signer_admin restore --state /backup/snapshot.db \
  --output /var/lib/ocservia-signer/restored.db \
  --minimum-revision "$MINIMUM_REVISION"
signer_admin inspect --state /var/lib/ocservia-signer/restored.db \
  > "$BACKUP_DIR/restored-inspect.json"
```

All destination files must be new. Compare issuer, policy, state version and
revision with independent evidence before activating the restored file. Keep
the original ledger intact until verification succeeds. Preserve UID/GID
65532:65532 and mode 0600, and the same issuer CA and node identities. Do not
lower `signer-checkpoint.json` to make an old snapshot start. If freshness
cannot be established, leave the deployment recoverably stopped.

Only after those comparisons succeed, activate within the same stopped state
directory, keeping the original under a new, non-overwriting recovery name:

```bash
test ! -e "$OCSERV_SIGNER_STATE_DIR/ledger.before-restore.db"
test "$(stat -c '%u:%g:%a' "$OCSERV_SIGNER_STATE_DIR/restored.db")" = 65532:65532:600
mv -T --no-clobber "$OCSERV_SIGNER_STATE_DIR/ledger.db" \
  "$OCSERV_SIGNER_STATE_DIR/ledger.before-restore.db"
mv -T --no-clobber "$OCSERV_SIGNER_STATE_DIR/restored.db" \
  "$OCSERV_SIGNER_STATE_DIR/ledger.db"
sync -f "$OCSERV_SIGNER_STATE_DIR"
signer_admin inspect > "$BACKUP_DIR/activated-inspect.json"
```

An interruption between these moves leaves the original recoverable; do not
run `init`. Retain the old ledger under the backup custody policy, not as a
second live replica.

While stopped, `signer_admin crl > "$BACKUP_DIR/issuer.crl.pem"` creates a new
signed CRL and advances its number. Verify its issuer/signature, number and
next-update, transfer only this public CRL to the intended ocserv node, install
it atomically at that node's configured CRL path and send SIGHUP to the correct
ocserv service. Prove a revoked certificate can no longer establish a login
and an unrevoked control still can. CRLs expire after one hour; distribution
and refresh remain explicit operator responsibilities. A revoke response is
not CRL distribution and does not force existing VPN sessions to disconnect.

After backup/restore or CRL maintenance, run
`deploy/production/controller.sh start` with the unchanged
configuration and current exact source. Check readiness/version, real login,
node reconnect and a newly approved operation. Missing/incompatible ledger or
lower revision must fail closed. Integrated `--purge-data` is intentionally
unsupported; uninstall is not authorization to erase CA or node identities.

Node retirement is a separate procedure starting with Controller online:
reconcile pending operations, request and independently approve retirement
through Controller, then stop affected mutations and Signer before running
`signer_admin disable --node "$NODE_ID"`. Restart Signer and verify both
sealing purposes reject that identity. Controller revocation alone does not
disable the offline sealing map. Disable is durable and irreversible for that
identity; re-enrollment requires a new identity and approved import. The
retired node must not reconnect; any post-maintenance reconnect/command
checks must use a different, non-retired node.
If Controller returns `Revocation committed` with HTTP 503, read the node's
authoritative trust/disconnect state until convergence; do not interpret that
response as an uncommitted mutation or automatically replay it.
