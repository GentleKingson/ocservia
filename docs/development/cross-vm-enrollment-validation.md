# Cross-VM enrollment validation

The `deploy/real-e2e` fixture uses one explicitly configured HTTPS test Relay
for two independent `ubuntu-24.04` GitHub-hosted VMs. Agent and transportd
retain disabled public endpoint discovery and public DNS fallback. The old
default-Relay fixture's historical results do not qualify this configuration.

Status: **NOT_PROVEN / BLOCKED_INFRA** until an authorized test network is
configured and this candidate passes a real hosted run. No dedicated test
Relay or overlay identity is provisioned by the workflow. Three-VM native VPN,
sole-Relay outage/recovery and revocation acceptance remain unimplemented;
Release Check has no cross-VM requirement.

## Infrastructure prerequisite

An operator must first authorize a disposable or isolated test-only Relay,
configure its authenticated token and TLS certificate with the correct SAN,
and make its HTTPS endpoint reachable from both hosted VMs. The Relay name
must resolve through system DNS on both VMs and inside the Controller's Docker
network. There is no public Relay, public endpoint lookup or DNS fallback.
Do not point this fixture at an existing production service.

Create a dedicated GitHub Environment named `cross-vm-test-relay` before enabling
credentialed execution. Under **Deployment branches and tags**, select only
the **branch** `main` (no tags). Disable required reviewers, wait timers and
custom protection rules. The preflight, Controller and Agent jobs use this
Environment with `deployment: false`, preserving its access rules without
creating deployment records. The verdict job has no Environment or Relay
secrets. These controls require repository-administration permission; declaring
the Environment in YAML does not establish that its server-side policy exists.

Store these secrets only in that Environment, without placing values in source:

| Secret | Value |
|---|---|
| `CROSS_VM_RELAY_URL` | One credential-free `https://host[:port]` URL |
| `CROSS_VM_RELAY_CA` | PEM test CA certificate, with no private key |
| `CROSS_VM_RELAY_TOKEN` | Matching Relay token, 32–512 non-whitespace bytes |

Do not store copies under repository or organization secrets. If migrating
existing values, configure and verify the restricted Environment before removing
the other copies. A non-main branch can change its own workflow YAML, so a shell
check or job condition cannot replace the Environment's server-side restriction.

Leave the non-sensitive repository variable `CROSS_VM_SCHEDULE_ENABLED` unset
or set to `false` until real dual-VM acceptance succeeds. Only the exact value
`true` enables scheduled qualification. The switch does not disable manual or
reusable execution. Missing or unexpected values disable scheduled execution
before any Environment job is eligible or any build starts. The final job
requires all three credential jobs to be skipped in that case and reports
`CROSS_VM_ENROLLMENT=NOT_RUN_SCHEDULE_DISABLED`; this is no acceptance result.
Cleanup failure still fails the workflow. Any other unexpected skip fails
qualification.

The operator owns token expiry/rotation and the external Relay's lifecycle.
The fixture copies credentials into each run's private directory and deletes
those copies; it does not create or revoke external credentials. Missing or
malformed configuration reports `BLOCKED_INFRA` and fails before building the
Controller or Agent. A Secrets/Variables API 403 alone does not establish
whether configuration exists. No new account, paid VM or persistent network is
created. A private-overlay implementation requires separate authorized identity
and ACL setup; it is not included here.

## GitHub-hosted execution

```sh
gh workflow run real-e2e.yml --ref main
```

The workflow supports dispatch, daily schedule (`17 3 * * *`, 11:17 Hong Kong
time), and `workflow_call`. Enabled scheduling becomes active only on the default
branch and may be delayed or missed by GitHub. A reusable caller must explicitly
pass the three named secrets and grant `contents: read` and `actions: write`;
the latter is narrowed to `actions: read` except in the final cleanup job.
Invoke this fixture once per run attempt: rendezvous/evidence names are scoped
to that attempt. Jobs API lookup accepts reusable caller prefixes but rejects
ambiguous peers.
Environment secrets take precedence over repository/organization values with
the same names; explicit reusable mappings remain required even when values
exist only in the Environment. Call the workflow from trusted main with the
local `./.github/workflows/real-e2e.yml` path. There is no `secrets: inherit`,
OIDC identity or publishing permission.

An uncredentialed admission job runs first, with no checkout, Environment,
Relay secrets or token permissions. It checks the caller repository/ref, the
defining workflow's main ref and SHA, and the event, then compares the schedule
switch in Bash. Actions expression equality ignores case; it is not used to
interpret the variable. Only a constant `mode=run` output makes preflight
eligible; a disabled schedule produces `mode=disabled`. Only manual, scheduled and main push
caller events are accepted; `pull_request_target` and `workflow_run` callers
are rejected. GitHub's `job.workflow_ref`/`job.workflow_sha` identify the reusable
callee, while `github` identifies its caller. These GitHub.com fields are read
through the job JSON context so absent fields fail closed. Environment policies
evaluate the caller's ref; trusted main must never invoke an untrusted workflow
revision or pass credentials to untrusted code. See GitHub's
[reusable workflow guidance](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows).

Non-main dispatch demonstrates `BLOCKED_AUTH`, with no credentialed job or
checkout. Missing configuration on authorized main reports `BLOCKED_INFRA`.
Checkout must equal `GITHUB_SHA`. Concurrency is scoped to the run and attempt,
with a different prefix from Release Check.

Controller and Agent jobs start concurrently after infrastructure preflight.
They check native
systemd PID 1, Docker/Compose, Ubuntu version and required utilities, exchange
boot IDs and environment evidence, and reject same-host or mixed-commit peers.
The Controller runs real PostgreSQL, Control Plane and transportd; the Agent
builds and runs the real enrollment binary with the same single custom Relay,
CA and token as transportd. They compare the source SHA, run attempt, Relay
URL and CA fingerprint as well as distinct boot IDs. No public inbound
listener is required on either runner.

Before enrollment, the Agent VM checks TLS using the configured CA. An unrelated
CA must produce curl certificate-verification error 60; DNS/connection errors
and timeouts fail the test. The existing `ocservia-relay-network-probe` runs in
TCP-only mode and requires a real authenticated connection followed by typed
`ServerDeniedAuth` rejection of a wrong token. QUIC discovery is not required
for this enrollment fixture. Raw probe output stays private; only fixed result
markers enter evidence. These checks do not prove that a later business session
cannot upgrade to a direct path, or that sole-Relay recovery works.

The existing run-attempt-scoped artifact helper coordinates bounded waits and
stops on a peer failure. The Agent generates a fresh RSA-3072 key for each run;
only its public key is uploaded. The Controller encrypts the short-lived bound
enrollment token with RSA-OAEP SHA-256. Only ciphertext is uploaded; decrypted
tokens, endpoint private keys, and raw process/build logs remain private in
`RUNNER_TEMP` and are removed by unconditional cleanup. Uploaded evidence is
limited to environment, process/network, binding and cleanup records; raw logs
are never uploaded. The final job deletes rendezvous artifacts with narrowly
scoped Actions write permission, verifies both role jobs succeeded, then reports
`CROSS_VM_ENROLLMENT=PASS`. A timeout, missing binding, token-consumption mismatch
or failed cleanup cannot pass. Evidence artifacts are retained for seven days.
Job cancellation can prevent final cleanup; in that case no PASS is valid and
rendezvous artifacts must be deleted through the Actions API before closing the
run. Hosted VMs are disposable; no persistent server is provisioned.

The token is the existing EndpointID-bound development enrollment token, not an
unbound production node bootstrap token. Its consumed node, expected endpoint,
workspace and environment are checked against the pending UUIDv7 node record.
This profile leaves `CROSS_VM_PRIVATE_BUSINESS` and
`DEDICATED_RELAY_RECOVERY` at `NOT_PROVEN`, and
`CROSS_WAN_PUBLIC_INGRESS=NOT_TESTED`: installation, approval, Online, native
ocserv/privd/Signer, independent OpenConnect and sole-Relay recovery need separate
implementation and real evidence. A private overlay does not prove public ingress.

There are five jobs when configured: admission, a short preflight, two build/test
VM roles (60-minute deadline each), and a short verdict/cleanup. Missing
infrastructure runs admission, preflight and verdict and fails qualification;
non-main and disabled schedules never start credentialed jobs. Actual runner
minutes, build/queue time and network costs must be measured from a real run;
no successful-run cost or acceptance result is established by static checks.

Local checks (not live acceptance):

```sh
python3 scripts/test-real-e2e-hosted.py  # requires PyYAML
bash scripts/test-real-e2e-artifact.sh
make real-e2e-check
```

Prepare two separate Linux VMs that have different boot IDs (read from
`/proc/sys/kernel/random/boot_id`), the same repository commit, and outbound
access to the authorized test Relay. Both machines need the shell
utilities used by the scripts, including jq. The Controller VM also requires
Docker Compose, which starts real PostgreSQL, Control Plane, and transportd
processes. The managed-node VM requires OpenSSL and the repository's Rust
toolchain to build the real Agent.

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
