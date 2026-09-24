# Integrated network prototype

P1 development prototype against source baseline
`e861b72130d4d409883f68d01805566f26cbafbc`; not a production mode, release
artifact or completed P1 acceptance. Read the
[P0 contract](../../../docs/development/integrated-deployment-adr.md).
The existing lifecycle/manifest/environment allowlist does not admit this
overlay yet. Do not bypass `controller.sh` to activate it on production.

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
P3 must integrate this into the existing launcher, check mode/image/hostname
inputs and preserve the standalone path. `compose.sh` is deliberately unchanged.

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
digests instead. NGINX 1.28.3 and Caddy 2.11.4 are digest-pinned; Relay uses the
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

## Results and remaining gates

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

P3 may use the network/Secret/permission description, but must not report the
network ready while these required gates remain. P4 can reuse the Edge build
definition and the locked Relay/Gateway versions. P5 must prove the missing
authenticated and cross-host paths, not replace them with health checks.
Rollback here is removal of the prototype files and task-owned fixtures; no
production state or standalone launcher was changed.
