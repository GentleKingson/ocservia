# Cross-VM enrollment validation

The hosted fixture runs three independent Ubuntu 24.04 VMs: Controller with
PostgreSQL and transportd, the real Agent, and one disposable iroh HTTPS Relay.
It reuses the locked Relay Dockerfile and enrollment protocol. Production Relay
configuration and the default transportd image target are unchanged.

**Status: BLOCKED_INFRA / NOT_PROVEN until the administrator setup below and an
authorized run on the exact merged main SHA succeed.** Local contracts, Basic CI
and CodeQL do not prove three-VM enrollment. Earlier two-VM results do not qualify
this topology. There is no new Release Check gate.

## One-time administrator setup

1. Use a dedicated test Tailnet without Tailnet Lock (the pinned Action's WIF
   mode does not support Tailnet Lock). Create `tag:ocservia-ci-peer` and
   `tag:ocservia-ci-relay`, owned only by authorized administrators. Configure
   two separate OIDC federated identities, with writable `auth_keys` scope and
   only their respective tag. No persistent auth key or OAuth secret is needed.
2. For **both** identities, use issuer
   `https://token.actions.githubusercontent.com`, and the exact subject:
   `repo:GentleKingson@72605370/ocservia@1319465232:environment:cross-vm-test-relay`.
   The repository's OIDC customization API currently reports `use_default=true`,
   `use_immutable_subject=true` and the ID-bearing prefix above. Recheck this
   before installation. The Environment replaces the branch suffix in default
   `sub`; the subject alone does **not** constrain the branch.
   Add exact custom claim rules:

   | Claim | Required value |
   |---|---|
   | `repository` | `GentleKingson/ocservia` |
   | `repository_id` | `1319465232` |
   | `repository_owner_id` | `72605370` |
   | `ref` | `refs/heads/main` |
   | `workflow_ref` | `GentleKingson/ocservia/.github/workflows/real-e2e.yml@refs/heads/main` |
   | `environment` | `cross-vm-test-relay` |
   | `runner_environment` | `github-hosted` |

   Use each identity's generated audience exactly. Do not use a wildcard subject,
   repository, ref, workflow or audience. The local pre-join guard also compares
   workflow SHA to the checked-out SHA and rejects a different reusable callee.
   First qualification uses direct `workflow_dispatch`; callers with another
   `workflow_ref` are intentionally rejected by this identity policy.
3. Apply the following grant to the test Tailnet policy:

   ```json
   {
     "tagOwners": {
       "tag:ocservia-ci-peer": ["autogroup:admin"],
       "tag:ocservia-ci-relay": ["autogroup:admin"]
     },
     "grants": [{
       "src": ["tag:ocservia-ci-peer"],
       "dst": ["tag:ocservia-ci-relay"],
       "ip": ["tcp:3443"]
     }]
   }
   ```

   Review the **entire effective policy**, including existing ACLs and grants:
   rules are additive. Remove overlapping allow-all, wildcard, group, tag or
   address-range rules that allow peer-to-peer, relay-to-peer, other Relay ports
   or access to unrelated devices. Do not merely append this grant to the default
   allow-all policy. Use Tailscale policy tests to require peer → relay TCP 3443
   and reject peer → peer, relay → peer and peer → relay TCP 22/443/8080 and UDP
   7842. Do not grant routes, exit nodes, SSH or Funnel. Existing networks must
   not be silently broadened or disrupted; use the isolated test Tailnet.
4. Preserve GitHub Environment `cross-vm-test-relay` with **only branch `main`**
   allowed, no tag patterns. Do not relax its protection rules. Keep the existing
   `CROSS_VM_RELAY_TOKEN` secret only there (32–512 non-whitespace bytes); never
   put it in variables or artifacts. The old URL/CA secrets are no longer read
   by this workflow; removal can wait until no older/manual consumers need them.
5. Set these **Environment variables**, not secrets: `TS_PEER_CLIENT_ID`,
   `TS_PEER_AUDIENCE`, `TS_RELAY_CLIENT_ID`, `TS_RELAY_AUDIENCE`. Only after the
   above effective-policy/identity review, set `TS_POLICY_REVIEWED=true` there.
   This is an administrator attestation, **not** an independent API policy audit.
   Missing configuration fails preflight with `BLOCKED_INFRA` before any join.
6. Set repository variable `CROSS_VM_SCHEDULE_ENABLED=false`. In addition, this
   candidate hardcodes the admission schedule input to `false` so an inaccessible
   or stale variable cannot enable an unqualified topology. Reconnecting the
   variable requires a separate change after the first real success; do not do
   that during initial acceptance. Keep Release Check unchanged.

No administrator Tailnet API credential is passed to CI. Consequently, CI cannot
independently enumerate all policy rules or confirm server-side device deletion.
Do not label either as API-verified. If stronger deletion evidence is needed,
an administrator can inspect the three run-scoped hostnames in the device API
before/after the run without adding credentials to this workflow.

## Hosted execution and evidence

After review and explicit merge authorization, record the merged SHA and run:

```sh
gh workflow run real-e2e.yml --ref main
```

Check the run's `head_sha` equals that recorded SHA. The graph is
`admission → preflight → relay/controller/node → verdict`. Admission is
uncredentialed and checks repository, main ref, defining workflow ref/SHA and
event before checkout. All three role jobs use the protected Environment and
job-local `id-token: write`. Tailscale Action v4.1.3 is fixed at
`780049a30b6ff5c378a9e7b389d15ece7a204888`; it requests ephemeral, preapproved
nodes with in-memory state. No long-lived Tailscale key is supplied.

The Relay generates a one-day CA and DNS-SAN leaf certificate for
`relay.ocservia.test`, disables QUIC discovery in its disposable configuration,
and publishes only its Tailnet IPv4 TCP 3443. No public port, external VPS,
public DNS or public Relay is used. Its readiness artifact contains only the
URL, Tailnet IP, public CA and fingerprint, source SHA, run ID, attempt and boot
ID. Peers reject stale, mixed-source, same-VM or malformed readiness. They map
the fixed hostname locally; a test-only Compose overlay also maps it inside
transportd and endpoint bootstrap. Production Compose networks are unchanged.

The Relay job remains alive, checking its container and verified HTTPS health,
until **both full peer jobs complete**, including cleanup and post steps. Failed
peers fail qualification, and waits have deadlines. The Controller's actual
transportd container uses an opt-in image target containing the existing iroh
probe. Its real authenticated connection and typed wrong-token denial must pass
before controller-ready is published. The Agent checks the correct CA and token,
requires curl error **60** for the unrelated CA (timeouts/DNS errors fail), and
requires typed `ServerDeniedAuth` for the wrong token before real enrollment.
TLS/hostname verification remains enabled everywhere.

Existing OAEP exchange, real Agent enrollment and Controller database assertions
remain intact: UUIDv7 Pending Node, EndpointID, workspace/environment, and
consumed token → node binding. Controller and Agent compare distinct boot IDs,
source/run identity, common Relay metadata and a third distinct Relay boot ID.
A compact post-traffic Tailscale peer observation records `CurAddr`, `Relay`,
`PeerRelay` and `Active`; an absent path is unknown, not proof of direct traffic.
Tailscale transport (direct/DERP/peer relay) is separate from application iroh
Relay use. `tailscale ping` is not used as enrollment evidence.

Cleanup steps run with `always()`: Controller services/volumes/networks, Agent
process/build/private files, Relay container/image/TLS/token files, local hosts
mapping, Tailscale logout, daemon shutdown and in-memory credentials. Explicit
cleanup failures fail their job, even though the upstream Action's post hook
only warns on cleanup errors. That post hook may warn that an explicitly stopped
daemon is absent; the checked cleanup step owns the result. Runner cancellation
or loss remains best effort; ephemeral server removal is expected by Tailscale
but always reported `SERVER_DEVICE_DELETION=NOT_INDEPENDENTLY_VERIFIED` without
independent API evidence.

Verdict deletes all five run-attempt rendezvous artifact kinds, re-lists them
and requires zero remaining. Three allowlisted evidence artifacts remain for
seven days. Any failed, cancelled, unexpectedly skipped, timed-out or cleanup-
failed dependency prevents PASS; disabled schedules report NOT_RUN, never PASS.
The final positive markers are only for the exact successful run:

```text
CROSS_VM_ENROLLMENT=PASS
CLEANUP=PASS
CROSS_VM_PRIVATE_BUSINESS=NOT_PROVEN
DEDICATED_RELAY_RECOVERY=NOT_PROVEN
CROSS_WAN_PUBLIC_INGRESS=NOT_TESTED
```

Record run ID/attempt, SHA, three boot IDs, TLS/token positive and negative
results, DB binding, path observation, effective-policy review basis, artifact
cleanup and local/server cleanup distinctions. Measure wall time and summed job
seconds from the Jobs API; report runner minutes separately from billed minutes.
No successful three-VM cost or duration is known before the real run.

Local checks (no Tailnet credentials or actual joins):

```sh
make real-e2e-check
python3 scripts/test-real-e2e-hosted.py
bash scripts/test-real-e2e-artifact.sh
actionlint .github/workflows/real-e2e.yml
shellcheck -x scripts/real-e2e-{hosted,controller,node,artifact,relay-host}.sh
```

Official references checked for this implementation:
[Tailscale Action at the pinned SHA](https://github.com/tailscale/github-action/tree/780049a30b6ff5c378a9e7b389d15ece7a204888),
[workload identity federation](https://tailscale.com/docs/features/workload-identity-federation),
[additive Grants/ACL behavior](https://tailscale.com/docs/reference/grants-vs-acls),
[GitHub OIDC claims and immutable subjects](https://docs.github.com/en/actions/reference/security/oidc).

## Manual execution

Before starting, export a unique `RUN_ID` (restricted to letters, digits, dots,
underscores, and hyphens) and an absolute, private `RUNNER_TEMP` directory on
each VM. On the Controller VM, also export a unique, Docker-compatible
`COMPOSE_PROJECT`. Reuse the same environment values across every phase through
cleanup. Set `ARTIFACT_DIR` if you want diagnostics written outside the default
location under `RUNNER_TEMP`.

On each VM, supply `REAL_E2E_RELAY_URL`, `REAL_E2E_RELAY_CA` and
`REAL_E2E_RELAY_TOKEN` from protected local configuration, then run
`python3 scripts/real-e2e-relay.py "$RUNNER_TEMP/ocservia-real-e2e-<role>-$RUN_ID/relay"`,
replacing `<role>` with `controller` or `node`. This creates a new private
directory; it refuses to replace an existing one. Unset the input variables
afterward. Controller startup uses sudo only to assign its copied token to
container UID/GID 65532; the node's copy stays owned by its Agent process user.
These manual scripts must also run only on explicitly authorized test hosts.

1. On the Controller, run `scripts/real-e2e-controller.sh start`. On the node,
   run `scripts/bootstrap.sh native` (unsupported on Linux ARM64; see
   [Linux ARM64 dependencies](testing.md#linux-arm64-go-validation)), source
   `scripts/env.sh`, and run `scripts/real-e2e-node.sh build`.
2. Copy
   `${RUNNER_TEMP}/ocservia-real-e2e-controller-${RUN_ID}/outbox/controller-ready`
   from the Controller to the node, then run
   `scripts/real-e2e-node.sh prepare CONTROLLER_DIR` against the received copy.
3. Copy
   `${RUNNER_TEMP}/ocservia-real-e2e-node-${RUN_ID}/outbox/agent-endpoint`
   from the node to the Controller and run
   `scripts/real-e2e-controller.sh issue-token AGENT_ENDPOINT_DIR`.
4. Copy the Controller's `outbox/enrollment-token` directory to the node and run
   `scripts/real-e2e-node.sh enroll TOKEN_DIR` before the ten-minute token TTL
   elapses.
5. Copy the node's `outbox/enrollment-result` directory back to the Controller,
   then run `scripts/real-e2e-controller.sh verify-enrollment RESULT_DIR` to
   confirm the pending UUIDv7 node record and its EndpointID binding.
6. Run the `diagnostics` phase and then the `cleanup` phase of the local script
   on each VM, including after a failed run, and delete any directories copied
   between the machines. Keep the enrollment token, private keys, and
   unredacted logs private.

Each `outbox` path above is relative to that role's working directory under
`RUNNER_TEMP`, whereas the directory arguments passed to the scripts point to
the copies placed on the receiving VM. Transfer those directories over a secure
channel such as SSH or SCP; the scripts do not automate file exchange, artifact
uploads, retention, or workflow cleanup.

The repository retains `scripts/real-e2e-artifact.sh` for GitHub Actions
artifact operations. Its download mode expects a live workflow run with
`GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`, `GITHUB_REPOSITORY`, `GITHUB_API_URL`,
and `GITHUB_TOKEN` set, so the manual transfer procedure above does not invoke
it.

## Scope

This profile verifies enrollment across separate VMs with a persistent Agent
identity, Controller-bound enrollment, and a live Iroh path over the Internet.
It does not cover Agent package installation, Agent or privd systemd units,
native ocserv or OpenConnect packages, two-principal OIDC approval, privileged
mutations, dedicated Relay outage/recovery, or production failure domains.

In an authorized validation environment, you can check script syntax alone:

```bash
make real-e2e-check
```

This target runs `bash -n` on `real-e2e-controller.sh`, `real-e2e-node.sh`,
`real-e2e-artifact.sh` and `real-e2e-hosted.sh`. It does not execute `scripts/test-real-e2e-artifact.sh`,
start any VM, container, or Agent, or verify that enrollment works.
