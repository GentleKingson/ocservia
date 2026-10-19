# Cross-VM enrollment validation

Cross-VM enrollment runs manually through the `deploy/real-e2e` fixtures and
helper scripts. The opt-in `real-e2e.yml` workflow runs two independent
`ubuntu-24.04` GitHub-hosted VMs, outside Basic CI and Release Check.

Public endpoint address lookup is now disabled in Agent and transportd. The
default-Relay, EndpointID-only rendezvous in this profile cannot complete
enrollment without explicit Relay addressing. The instructions below describe
that existing fixture, not acceptance for the dedicated-Relay deployment.

## GitHub-hosted execution

```sh
gh workflow run real-e2e.yml --ref <candidate-branch>
```

Controller and Agent jobs have no dependency on each other. They check native
systemd PID 1, Docker/Compose, Ubuntu version and required utilities, exchange
boot IDs and environment evidence, and reject same-host or mixed-commit peers.
The Controller runs real PostgreSQL, Control Plane and transportd; the Agent
builds and runs the real enrollment binary using Iroh's default public relay
configuration. No public inbound listener is required on either runner.

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
This profile alone leaves `CROSS_VM_PRODUCTION` and
`DEDICATED_RELAY_RECOVERY` BLOCKED: installation, approval, Online, native
ocserv/privd/Signer and dedicated-relay recovery require separate authorized
network infrastructure. A public-relay enrollment run cannot prove those claims.

Local checks (not live acceptance):

```sh
python3 scripts/test-real-e2e-hosted.py  # requires PyYAML
bash scripts/test-real-e2e-artifact.sh
make real-e2e-check
```

Prepare two separate Linux VMs that have different boot IDs (read from
`/proc/sys/kernel/random/boot_id`), the same repository commit, and outbound
Internet access to Iroh's default relay discovery. Both machines need the shell
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
mutations, dedicated relays, or production failure domains.

In an authorized validation environment, you can check script syntax alone:

```bash
make real-e2e-check
```

This target runs `bash -n` on `real-e2e-controller.sh`, `real-e2e-node.sh`, and
`real-e2e-artifact.sh`. It does not execute `scripts/test-real-e2e-artifact.sh`,
start any VM, container, or Agent, or verify that enrollment works.
