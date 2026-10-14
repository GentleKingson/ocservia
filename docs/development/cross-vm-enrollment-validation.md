# Cross-VM enrollment validation

Cross-VM enrollment runs manually through the `deploy/real-e2e` fixtures and
helper scripts; it stays outside Basic CI and has no dedicated Actions workflow.

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
