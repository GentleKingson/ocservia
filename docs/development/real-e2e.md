# Cross-VM real E2E validation

Cross-VM enrollment is script-level manual acceptance, not a GitHub Actions
workflow or a prerequisite of Basic CI. The underlying scripts and
`deploy/real-e2e` fixtures remain available for suitable local or dedicated
test environments.

Use two distinct Linux VMs with different boot IDs, the same repository commit,
and Internet access to Iroh's default relay discovery. The Controller needs
Docker Compose; the managed node needs the repository's Rust toolchain. Both
need the shell utilities used by the scripts, including jq. This profile starts
real PostgreSQL, Control Plane, and transportd processes and builds the real Agent.

## Manual execution

On each VM, set a unique `RUN_ID` (letters, digits, dots, underscores, and
hyphens only) and an absolute, private `RUNNER_TEMP` directory. On the Controller,
also set a unique, Docker-compatible `COMPOSE_PROJECT`. Keep these values for
all phases, including cleanup. `ARTIFACT_DIR` optionally overrides the default
diagnostics directory under `RUNNER_TEMP`.

1. On the Controller, run `scripts/real-e2e-controller.sh start`. On the node,
   run `scripts/bootstrap.sh native`, source `scripts/env.sh`, and run
   `scripts/real-e2e-node.sh build`.
2. Securely copy the Controller's
   `${RUNNER_TEMP}/ocservia-real-e2e-controller-${RUN_ID}/outbox/controller-ready`
   directory to the node. Run `scripts/real-e2e-node.sh prepare CONTROLLER_DIR`
   with that received directory.
3. Copy the node's
   `${RUNNER_TEMP}/ocservia-real-e2e-node-${RUN_ID}/outbox/agent-endpoint`
   directory to the Controller. Run
   `scripts/real-e2e-controller.sh issue-token AGENT_ENDPOINT_DIR`.
4. Securely copy the Controller's `outbox/enrollment-token` directory to the
   node. Run `scripts/real-e2e-node.sh enroll TOKEN_DIR` before the ten-minute
   token expires.
5. Copy the node's `outbox/enrollment-result` directory to the Controller. Run
   `scripts/real-e2e-controller.sh verify-enrollment RESULT_DIR` to verify the
   pending UUIDv7 node and its EndpointID binding.
6. On each VM, run the respective script's `diagnostics` phase, then its
   `cleanup` phase even after a failure. Remove any manually copied exchange
   directories as well. Protect the enrollment token and private keys; do not
   publish them or unredacted logs.

Each `outbox` belongs to the role-specific working directory shown above;
directory arguments refer to received copies on the destination VM. Transfer
files using a secure channel such as SSH/SCP. There is no automatic artifact
exchange, upload, retention policy, or workflow cleanup.

`scripts/real-e2e-artifact.sh` is retained as a GitHub Actions artifact helper.
Its download mode still requires a real run and the `GITHUB_RUN_ID`,
`GITHUB_RUN_ATTEMPT`, `GITHUB_REPOSITORY`, `GITHUB_API_URL`, and `GITHUB_TOKEN`
environment variables. It is not needed for the manual file-transfer flow above.

## Scope

This profile checks distinct VMs, persistent Agent identity, Controller-bound
enrollment, and a real Iroh Internet path. It does not install the signed Agent
package, start Agent or privd systemd units, install native ocserv/OpenConnect,
approve a node through two OIDC principals, execute privileged mutations, or
prove dedicated-relay and production failure-domain behavior.

For a syntax-only check on `LocalServer`, without starting any services:

```bash
make real-e2e-check
```
