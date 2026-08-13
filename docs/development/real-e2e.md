# Cross-VM real E2E validation

`.github/workflows/real-e2e.yml` is a manual, two-runner validation profile. It
does not replace the pull-request CI workflow or its required checks.

The Controller runner starts PostgreSQL, applies migrations, starts the Go
Control Plane, and runs the repository's real `ocservia-transportd` image with
Iroh's default Internet relay discovery. The managed-node runner builds the
real Agent, persists a fresh identity, and exchanges its EndpointID through a
run-attempt-scoped artifact. The Controller issues an EndpointID-bound,
ten-minute enrollment token; the Agent then enrolls through the real Iroh
enrollment ALPN. The Controller requires a pending UUIDv7 node whose stored
EndpointID matches the node runner's identity.

The four rendezvous artifacts include both `github.run_id` and
`github.run_attempt`. The enrollment-token artifact is retained for one day;
all other exchanged files contain public identifiers only. Diagnostic uploads
are scanned against the generated token and private-key material before upload.
Every Docker and temporary resource is scoped to the run attempt and removed by
an `always()` cleanup step.

This first profile proves two distinct GitHub-hosted VMs, real Control Plane and
transportd processes, persistent Agent identity, Controller-bound enrollment,
and a real Iroh Internet path. It does not yet install the signed Agent package,
start Agent or privd systemd units, install native ocserv/OpenConnect, approve a
node through two OIDC principals, execute privileged mutations, or prove
dedicated-relay and production failure-domain behavior. Those capabilities must
not be inferred from a green run.

Run the static harness locally with:

```bash
make real-e2e-check
```

The live profile is intentionally `workflow_dispatch` only and requires a
standard GitHub-hosted `ubuntu-24.04` runner for each job.
