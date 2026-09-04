# Bootstrap installation closeout

This acceptance map closes the documentation and CI work for the thin
Stage-0, versioned Stage-1, guarded Controller lifecycle, and native managed
node installation path. It does not grant any installer new authority.

## Controller acceptance

| Contract | Automated evidence |
| --- | --- |
| Configuration directory is separate from the source checkout; Stage-1 prepares a durable checkout | `scripts/test-controller-bootstrap.sh` |
| Stage-0 requires an exact stable version and hands off only to that release's Stage-1 | `scripts/test-stage0-installers.sh` |
| Checkout is a clean exact tag and the manifest `source_commit` matches before activation | `scripts/test-controller-bootstrap.sh`, `scripts/test-controller-install.sh`, `scripts/test-controller-lifecycle.sh` |
| Docker-less root lifecycle and existing-Docker launcher lifecycle remain distinct | `scripts/test-controller-install.sh`, `scripts/test-controller-host-bootstrap.sh` |
| Wrong trust key or tampered release evidence fails before activation | `scripts/test-controller-release-bundle.sh`, `scripts/test-controller-install.sh` |
| Successful install reaches release smoke and readiness | `scripts/test-controller-release-smoke.sh`, `scripts/test-controller-lifecycle.sh` |
| Reruns converge without replacing a valid durable checkout | `scripts/test-controller-bootstrap.sh` |
| Upgrade, rollback, start, default uninstall, and purge lifecycle state remain guarded | `scripts/test-controller-lifecycle.sh`, `scripts/test-controller-compose-lifecycle.sh` |

## Managed-node acceptance

| Contract | Automated evidence |
| --- | --- |
| Package-first `--version` mode installs an exact release native package without a Git checkout | `scripts/test-managed-node-install.sh` |
| Wrong signature, signing-key fingerprint, or package digest fails before the package manager | `scripts/test-managed-node-install.sh` |
| A protected Bootstrap Token reaches only `PENDING_APPROVAL` | `scripts/test-managed-node-install.sh` and the enrollment Go/PostgreSQL tests |
| Same-token, same-EndpointID retry converges; another endpoint is rejected | enrollment Go/PostgreSQL tests |
| Independent approval remains mandatory before activation | approval and enrollment Go/PostgreSQL tests |
| Post-approval service activation converges to `SERVICES_ACTIVE` | `scripts/test-managed-node-install.sh` |
| Signed package upgrade, rollback snapshot, and package uninstall preserve their lifecycle contracts | native package scriptlet smoke and `scripts/test-managed-node-install.sh` |

## Supply-chain acceptance

| Contract | Automated evidence |
| --- | --- |
| Both Stage-1 scripts are immutable Release assets in the exact asset set | `scripts/prepare-bootstrap-release-assets.sh`, `.github/workflows/release.yml`, release workflow contract tests |
| Published release attestation and immutability verify with `gh release verify` | `.github/workflows/release.yml` |
| Stage-0 and Stage-1 verify the independently pinned Ed25519 key, signed checksum, and selected asset digest | `scripts/test-stage0-installers.sh`, `scripts/test-controller-release-bundle.sh`, `scripts/test-managed-node-install.sh` |
| Tampered assets fail before handoff or package installation | the same three fixture suites |
| The endpoint verifier rejects bytes that differ from the repository sources | `scripts/verify-bootstrap-endpoint.sh`, `scripts/test-stage0-installers.sh` |

GitHub Actions is the authoritative merge-time run for the exact pull-request
commit. Tests requiring Docker or root run only through the repository's
isolated CI or LocalServer conventions and must clean their scoped resources.

## Lifecycle boundary

Stage-0 is a first-install convenience entrypoint, not an upgrade manager.
Controller upgrades use an operator-prepared clean checkout of the exact target
release and `controller.sh upgrade`; rollback and uninstall use the protected
Controller lifecycle state. Managed-node upgrades use a signed native package
or the signed durable upgrader, while uninstall follows the native package
manager lifecycle.

The current Controller Stage-1 has no prepare-only upgrade mode. Adding one is
an implementation change for the Controller bootstrap PR, not this
documentation closeout.

The external Stage-0 endpoints are not deployed or monitored by this
repository. Their fixture coverage proves the fail-closed client and byte
comparison contracts, not live hosting. They must not become the default Quick
Start until the operator records successful verification of the real served
bytes.
