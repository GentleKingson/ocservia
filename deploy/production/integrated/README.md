# Integrated deployment

The P1 network prototype was developed against source baseline
`e861b72130d4d409883f68d01805566f26cbafbc`. P3 connects it and the production
Signer to the existing lifecycle. P4 adds native signed candidates; P5 checks
the real deployment rather than inferring acceptance from configuration tests.
Historical prototype results below retain their original scope. Read the
[P0 contract](../../../docs/development/integrated-deployment-adr.md).
Do not bypass the signed-manifest checks in `controller.sh` for deployment.

## Lifecycle configuration

Use the existing bootstrap, `install.sh`, `controller.sh` and `compose.sh`,
not a second installer. Standalone remains the default. Integrated requires a
v2 platform manifest, Compose >= 2.24.4, and these additional `install.env`
settings alongside the normal database, authentication and Controller Secrets:

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
values are rejected. Controller's trusted proxy must remain the Gateway's
application `/32`, not Edge or an entire subnet. Certificates are provisioned
externally; this installer does not request ACME certificates or generate CAs.

The Relay secret directory is launcher-owned mode 0700 and contains nonempty
single-link `tls.crt` and `tls.key`, launcher-owned mode 0444. Its access token
is the existing Controller `relay-access-token`, not a second Relay token.
Signer secret and state directories are UID:GID 65532:65532, mode 0700,
canonical paths with protected ancestry. Signer requires `issuer-chain.pem`,
`issuer-key.pem`, `tls-cert.pem`, `tls-key.pem` and `api-token`, single-link
65532:65532 mode 0400 files. `tls-ca.pem` has the same ownership and mode 0444
so Controller can read its individual public-CA mount. Signer's TLS leaf must
include SAN `signer`. `api-token` must exactly match Controller's
`certificate-signer-token`; no private issuer key is mounted into Controller.
See the [Signer custody contract](../../../docs/development/production-signer.md)
for chain, token, approved key-transfer and recovery requirements.

Installation verifies the existing signed bundle and exact source checkout,
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

## Candidate pipeline

Dispatch the existing `release-upgrade.yml` with `purpose=integration`,
`production_signer=true` and the next plain `X.Y.Z` version. This is an explicit
candidate publication, not a stable tag or Release. Native AMD64/ARM64 producers
build Agent packages and the nine first-party Controller/Integrated images once.
The existing scanner gates both platforms before GHCR publication. Ordinary
diagnostics and clean consumers have read-only permissions; only the candidate
publication job has `packages:write`.

The `integrated-candidate-<run>-<attempt>` artifact contains signed v2 platform
manifests, the ephemeral candidate public key, scanned config/Registry digest
bindings and original producer artifact identities. Retain it, both native
product artifacts and acceptance evidence before repository retention expires.
The consumer checks producer-provided checksum/key hashes before pulling; an
operator must obtain the candidate public key fingerprint through a trusted
channel, install that key **outside** the bundle, and check out the manifest's
exact `source_commit`. With the documented Secrets/configuration provisioned,
use the existing root `controller.sh install --release-file "$MANIFEST"` entry
point, with `MANIFEST` set to the absolute platform-manifest path. Do not point stable bootstrap at a
fabricated tag or weaken its published-release source checks. Candidate packages
may require a read-only GHCR login; never distribute the CI publisher token.

Stable tag builds reuse the original products of a successful main-branch
Integrated run at the **same SHA and version**, rather than rebuilding binaries
or images. Missing/expired acceptance artifacts fail closed. Existing stable
tag binding and release-publishing approval remain required. Stable manifests
retain accepted per-platform digests, and all nine first-party images must pass
anonymous Registry reads before a stable Release can be published. A source
change, including a squash merge, requires a new candidate; never rewrite the
old manifest's SHA. A successful candidate workflow is not evidence that the
additional P5 public-network scenarios or a stable Release have run.

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
apply the Integrated overlay last, after the selected database/auth overlays.
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
restricted environment substitution. Gateway allows the Controller Host with
no port or `:443`, rejects mismatches before routing, preserves API/static
fallback/security headers and enables only HTTP/1.1 and HTTP/2, not HTTP/3.
Gateway accepts PROXY metadata only from Edge's dedicated `/32`
(`OCSERV_EDGE_GATEWAY_IP`, default `172.30.241.2`). Reserve that address outside
the dynamic allocation range; the subnet/range overrides must remain disjoint
from other networks. Controller still trusts its existing Gateway application
address, not the new Edge or an entire subnet.

Gateway/Relay names resolve through Docker DNS every five seconds for new
connections. Recreation is disruptive: existing streams can break and clients
must reconnect. Edge defaults to one worker, 2048 worker connections, 8192
file descriptors, 64 MiB and 35-minute stream inactivity timeout. A relayed
connection consumes multiple descriptors/hops. The 32-stream fixture is a
bounded sample, not a 2048-client, latency or HA promise.

The Relay service reuses its production image, entrypoint, configuration and
health check. Its TLS files come from `OCSERV_RELAY_SECRET_DIR`; the shared
Relay access token uses the existing Controller `relay_access_token` Secret,
also mounted by transportd. No token or TLS key is mounted into Edge.
Relay has a separate network from Gateway. Relay TCP peer logs see Edge, not
the original IP. The locked Relay's receive limiter wraps individual client
streams; this is not proof of original-IP abuse controls behind Edge.

## Reproduce the bounded check

Run locally only on BuildServer, in an isolated checkout. Development builds
are allowed here; final production-path acceptance must pull approved Registry
digests instead. NGINX 1.30.5 and Caddy 2.11.4 are digest-pinned; Relay uses the
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

The `relay-network-probe` target in `rust/g6-runtime.Dockerfile` accepts
`URL CA_FILE TOKEN_FILE`. It uses the locked Relay client to check a valid TCP
upgrade and an explicit bad-token rejection, then runs QUIC address discovery
with HTTPS fallback probes disabled. Mount only its test CA/token, map the test
hostname to the authorized public IP, and run it from the same-host egress
network. A timeout is not an authentication rejection or a QUIC pass.

The existing real-node chain supports `SINGLE_INTEGRATED_PUBLIC_IP` together with
`SINGLE_EDGE_IMAGE` and `SINGLE_NETWORK_PROBE_IMAGE`. In that mode it publishes Edge TCP443 and Relay UDP7842,
uses the public IP for both Agent and transportd, blocks non-loopback Agent UDP
inside its disposable namespace, and stops after the independently approved
real ocserv reload result. Its normal signed-package and image inputs remain
required. This mode does not run its separate outage/recovery scenarios.

## Historical P1 results and gates

BuildServer ARM64 development verification on 2026-09-24 passed the merged port
set and mount paths, both TLS branches, two bridge client-IP propagation and
spoof rejection, Controller Host/SNI negatives, static routing/no HTTP3,
32 concurrent short SSE streams, changed-IP Gateway/Relay recreation and Edge
restart/new-connection recovery. Caddy's pinned standard build includes
`caddy.listeners.proxy_protocol`; no custom module was required.

The script was corrected during development for interpolated Compose ports,
a probe image without Python, Relay HTTPS `/healthz` versus HTTP-only
`/generate_204`, and ephemeral host-port reassignment after Docker restart.
The Host-port negative also exposed Caddy directive ordering; an explicit
`route` now runs authority rejection before any API/static handler.
The first Actions attempt rejected the fixture's static address reservation
on a Docker-managed subnet; the fixture now declares its Relay subnet
explicitly. This changes only the test topology, not production networking.
These failed attempts were not P1 passes. Check the final artifact for the
exact candidate, image identities and added negative-case results.

Public validation on 2026-09-25 (Hong Kong time) additionally exercised two
GitHub-hosted sources against one BuildServer endpoint. The real-node run
`p1public2f9sbng` used a signed test Agent package, one custom Relay URL, and
namespace rules rejecting non-loopback IPv4 UDP and all IPv6 UDP on the Agent.
The connection probe reported `Relay(https://relay.p1.test/)`; independently
approved real ocserv reload operation `01a0d451-e760-75da-94a6-31dda8c0ad17`
reached `succeeded`. The locked Relay client accepted the correct token and
returned `ServerDeniedAuth` with `not authorized` for the wrong token. This is
a protocol-level denial, not necessarily an HTTP 401/403 response.

The initial combined run failed: public UDP7842 QAD timed out.
The same binary, Relay, CA and token passed QAD through the private host-bridge
address (reported address `172.18.0.1:48712`). A bounded packet capture saw
UDP7842 requests leave the host's physical interface for its public IP, with
no matching inbound UDP response. The cloud UDP rules/public NAT return path
were not independently inspected, so that capture alone did not identify which
cloud component dropped the traffic. After the operator confirmed opening the
cloud ports, a focused same-host public retest at 2026-09-25 01:01 Hong Kong time
passed: QAD returned `161.118.198.240:56303`, with a measured latency of 2.215397 ms.
Authenticated TCP and explicit wrong-token rejection passed again in that run.
The original failed artifacts remain failures; the later public retest supplies
the missing evidence. No private-route workaround was counted as a public pass.
All task containers and added host firewall rules were removed after each run;
operator-managed cloud rules were not changed by the harness.

| Gate | Status / next evidence |
| --- | --- |
| Public two-name TLS and two external client sources | PASS with explicit public IP and private test CA in [Actions run 36026179708](https://github.com/GentleKingson/ocservia/actions/runs/36026179708): observed sources `52.234.44.112` and `135.232.215.240`, one shared endpoint, both TLS branches and spoof rejection. Public DNS/ACME remain untested. |
| Exact published port set | PASS in rendered overlay; the authorized public fixture bound TCP443 and UDP7842. The real-node engineering harness also has its existing loopback-only backend publications, not a production deployment. |
| Same-host return path | TCP and public IPv4 QUIC PASS after the operator's cloud-port change. Authenticated TCP and real Agent commands traversed the public IP; the subsequent public UDP7842 QAD retest returned the actual public address with HTTPS probe fallback disabled. IPv6 was not validated. |
| Authenticated Relay-only command / wrong token / no public fallback | PASS for real approved ocserv reload with direct Agent UDP excluded and exactly one custom Relay URL in both process argv. Wrong token was explicitly rejected by the Relay protocol. Outage/recovery remains a separate unrun public-path scenario. |
| Relay Host / SNI boundary | Contract amendment approved 2026-09-25: strict Relay SNI and normal Token authentication, not HTTP Host rejection. The locked Relay returned `HTTP/1.1 200 OK` for `/healthz` with wrong Host and valid SNI. Gateway keeps strict Host checks; the independent real Token check now passes. |
| Idle and business reconnect | Short synthetic SSE and new TLS connections checked; 35-minute idle boundary, OIDC callback, real SSE authorization and established Agent reconnection remain NOT_RUN. |
| Registry production artifacts | NOT_RUN; P4/P5 responsibility. Local image IDs are not published pull references. |

These are the original P1 gates, not current Integrated candidate results.
P3 reused this network/Secret/permission contract; P4 reused the Edge build
and locked Relay/Gateway versions. The later real Controller/Agent acceptance
must be read with its own exact source and Registry identities. Prototype
rollback meant removal of task-owned fixtures; it is not the production
lifecycle rollback procedure below.

## Integrated acceptance history

P3 [PR #272](https://github.com/GentleKingson/ocservia/pull/272) merged as
`81a5ea348c1c1b5af13b611b65263b261d419206`; P4
[PR #273](https://github.com/GentleKingson/ocservia/pull/273) merged as
`7f0cc4f72a6861296db50ef2b742ef0d375a049e`; P5
[PR #274](https://github.com/GentleKingson/ocservia/pull/274) merged as
`368906c65d23bc7abff088dc4bbbdbf160012b32`. Each required Basic CI passed before
squash merge. Squash identities are not the tested branch identities below.

| Candidate source | Run / attempt | Result and boundary |
| --- | --- | --- |
| `b4fca50177c0f41eabdc6d693531a79d39968d50` | [36205708022 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36205708022) | P4 version 1.0.2: native build/scan/publication, full AMD64 business and ARM64 installation PASS; private network. |
| `6136dcc0f96a017209b8ce51fa7d59e8faa56fee` | [36208295345 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36208295345) | Both architecture scopes PASS; short SSE/private network only. |
| `092c34d408cab3e6426bc2dcf8b615fca5db0494` | [36209897298 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36209897298) | Private acceptance PASS. Separate public attempts FAILED on disposable Docker vfs disk use, incomplete artifact transfer (correctly rejected), then backup health wait. No public pass. |
| `733d0716f9e8e7ccd8d95e8959759295aff50cd4` | [36212450801 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36212450801) | Build/scan/publication and ARM64 smoke PASS; AMD64 enrollment FAIL from structured logs on UUID stdout. Public attempt FAIL from fixture CA missing strict keyUsage. |
| `82e9964118664633c5748de6bc72595627ad56f8` | [36213389600 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36213389600) | stderr logging fix and both private architecture scopes PASS; no new public run. |
| `0c341e7cc3ac02bd5ff62973ac36adce275d63a4` | [36214198954 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36214198954) | Strict fixture CA and both private architecture scopes PASS; public configuration rollback FAIL because its test executable was on noexec `/run`. |
| `144c1b71bfa92ef7eef9f7d94deda8120648d497` | [36216389171 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36216389171) | Fixed executable location; native builds/scans, AMD64 full private business and ARM64 smoke PASS. Separate BuildServer ARM64 complete public acceptance PASS, 2026-09-26 05:19:30 UTC. |

The last branch candidate used the signed 1.0.2 baseline above, actual
first install/upgrade, tampered-signature rejection before stop, a real TCP443
activation conflict with preserved current/pending state, retry, rollback and
re-upgrade. It preserved issuer CA, node keys, revocation and minimum revision.
Container recreation retained the established Agent process and a newly
approved operation had one journal execution and one privd receipt.

[Public client run 36218732086 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36218732086)
used two external sources, `172.214.44.1` and `4.246.151.208`, against the same
BuildServer endpoint `161.118.198.240`. Controller audit correlation proved
their original IPs and rejected spoofed forwarding headers. The locked Relay
probe passed authenticated TCP, explicit wrong-token denial and same-host
public UDP7842 QAD with HTTPS fallback disabled. Authorized SSE ran 2160 seconds
with 216 heartbeats, one 1800-second application lifetime and a successful
reconnect; anonymous SSE returned 401. Heartbeats prevent Edge inactivity, so
this is **not** a 35-minute completely idle Edge timeout test.

Failures above remain failures. Fixes created new source-bound candidates;
none was repaired by replacing a binary or rewriting its manifest SHA.

## Main candidate identity

The post-squash candidate is version `1.0.3`, source
`368906c65d23bc7abff088dc4bbbdbf160012b32`,
[run 36220524783, attempt 1](https://github.com/GentleKingson/ocservia/actions/runs/36220524783).
It is a new native build, not a relabeling of the P5 branch products. The signed
`integrated-candidate-36220524783-1` artifact is ID `10898458447`;
its ZIP SHA-256 is
`ac4370958b5dae691b3624b18f5dea8fcad9f8d5f2bcc1b7675a132dc4b22958`.

| Signed file | SHA-256 |
| --- | --- |
| `controller-release-amd64.json` | `72e1829e4f70460889981f3f06f35b899c8a1ef6434b7980f7e1d71a4f2886ec` |
| `controller-release-arm64.json` | `804d62f5c256ae079b2fc63df723c9bc3e61e1faa5dfb02e109fbf4bb2ab697d` |
| `SHA256SUMS` | `ef64102569598b0b21d7803145788b3d91aa884b5884bdad6395ad1f1911a0ba` |
| Candidate public-key PEM | `6cf72c95ffaf3f5b2febfd9a61db15954e47f75fa8b1167ae705ad1257a5c6fa` |

Each platform manifest binds all eleven image roles. The added Integrated
roles below are under `ghcr.io/gentlekingson/ocservia/<role>@sha256:<digest>`:

| Platform | Role | Registry digest |
| --- | --- | --- |
| AMD64 | Edge | `646a68d7d1df73cbc726b4480ed640aaf846848154e7bddce7c7259e4d2ef255` |
| AMD64 | Relay | `962ec189caf984dc32892b8feed94d25f20aed89d81ea470628c3b4b611fb04e` |
| AMD64 | Signer | `8e339005fb946bfef24932781762b3759d2c322b1453b15e507bc3ab031215b8` |
| ARM64 | Edge | `c686d575914f5e5f804831298f61cd94b52d3898ee49ac042ffe9f46d9fde494` |
| ARM64 | Relay | `f72a25b2fcbc4dfcbff42ec4e8f7526e545daf3097a2c66a36fab2801b5bb165` |
| ARM64 | Signer | `c4c30df2750e8d5d274c81f385d9084605a5fcd47a19475d334ef5e16c55d1c3` |

Use lowercase role names in pull references. Do not reconstruct a smaller
manifest from this table; consume and verify the complete signed bundle.
Candidate signing keys are ephemeral, not the stable Release trust root.
Retain the original producer artifacts and protected key fingerprint before
the repository's one-day artifact retention expires. Stable promotion still
requires those accepted products, exact source/version and existing release
approvals; missing artifacts fail closed rather than triggering a rebuild.

### Measured support and maintenance

Run 36220524783 completed successfully on main. A fresh BuildServer isolated
ARM64 Docker/systemd deployment consumed only its Registry digests and signed
native package, finishing at **2026-09-26 07:00:41 UTC**. It repeated the full
public business and lifecycle checks above; no compilation, image build,
replacement Signer or edited Compose file was used in consumption. The
prebuilt locked Relay network probe was independently SHA-256 pinned.

| Path | Result / tested scope |
| --- | --- |
| AMD64, Ubuntu 24.04 native Runner | PASS: build, smoke, scan, signed Registry pull, P3 installation, full Controller/Agent/privd, Local/OIDC, two-purpose sealing, P12/CRL, browser/config/VPN, offline queue and single-Relay recovery, recreation and short authorized SSE. Private network. |
| ARM64, Ubuntu 24.04 native Runner | PASS: build, smoke, scan, clean signed Registry pull/install/start, entry/internal TLS, non-purging uninstall/start and identity preservation. |
| ARM64, BuildServer disposable Debian 13/systemd | PASS: public full chain, same-host TCP/UDP, baseline install/upgrade/failure recovery/rollback/re-upgrade, established Agent reconnect, authorized SSE and documented maintenance below. |
| External public clients | [36225418379 / 1](https://github.com/GentleKingson/ocservia/actions/runs/36225418379) PASS: distinct sources `20.97.199.51` and `57.151.137.34`; both independently matched Controller auth audit source IP after maintenance, with spoofed forwarding headers ignored. |
| Database/auth deployment adaptation | P3 focused rendering/preflight matrix PASS for bundled/external PostgreSQL, external MySQL/MariaDB and Local/OIDC/dual modes. This candidate's full real business used bundled PostgreSQL and Local + OIDC; it is not a new full MySQL/MariaDB business matrix. |

The main public SSE sample ran 2161 seconds, with 215 heartbeats, two
connections and a first application lifetime of 1800.000068 seconds. The
same-host public QAD result was `161.118.198.240:44150`, not a private bridge
address. No direct Agent UDP path was counted as Relay business.

The maintenance commands below were rehearsed in that clean deployment after
the SSE check, before the two new external requests. `controller.sh uninstall`
stopped the deployment; the actual digest-pinned Signer backed up and inspected
state version 1/revision 12. Restore with floor 13 was rejected without creating
a destination. Restore with independently captured floor 12 retained issuer,
policy and revision, activated with UID/GID 65532 and mode 0600, then normal
`controller.sh start` restored login and node connectivity. A newly approved
reload produced exactly one journal execution and one privd receipt.
CA and private node identity hashes stayed unchanged. The full maintenance
stop includes the configured backup startup health wait; it is not zero downtime.

CRL refresh advanced to 13, and the native ocserv verifier again rejected the
revoked P12 while accepting the unrevoked control. Controller retirement was
independently approved, the node became revoked/offline, and offline Signer
`disable` advanced revision to 14. Both sealing purposes then returned 403.
The private snapshot and keys stayed in the disposable deployment and were
removed with its normal fixture cleanup; only sanitized results were retained.

NOT_RUN / not claimed: stable tag/Release publication and download/bootstrap,
an online production target, public IPv6 or public CA/ACME issuance, 35-minute
completely idle Edge timeout, HA, long-duration load, automatic CRL delivery,
or forced disconnection of existing VPN sessions. Distinct requester/approver
principals were real, but do not prove two independently responsible humans.
Image scans retain the three explicitly approved Oracle FIPS-channel false
positive entries in [the existing exemption file](../image-scan-exemptions.json),
with review deadline `2026-10-26`; other findings remain gated.
Subsequent documentation-only changes do not retest or relabel this source.

## Operator lifecycle and maintenance

Choose `standalone` for separately operated Relay/Signer endpoints, or
`integrated` for this single-host topology. Integrated is not HA. Use the
[pinned release installation](../../../docs/getting-started/production.md)
with `--version vX.Y.Z --root-lifecycle` only after that exact stable Release
exists. A candidate is not a published Release: use its exact source checkout,
independently trusted public key and platform manifest with the existing
`controller.sh` entry instead.

`install.env` is data parsed by the installer's strict allowlist, not a shell
script. Do not `source` it. Direct lifecycle commands require the same explicit
operator environment used for installation, including database/auth settings,
Secret directories, public names, identity and release public key. Keep it in
the protected operator session; do not print it into acceptance logs.

```bash
# Root operator session, clean checkout at the signed manifest's source_commit.
deploy/production/controller.sh install --release-file "$MANIFEST"
deploy/production/controller.sh start
# From the next exact source checkout, using its verified platform bundle:
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
from Controller using the [controlled transfer procedure](../../../docs/development/production-signer.md#trusted-public-key-transfer).
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
