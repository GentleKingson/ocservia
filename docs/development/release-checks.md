# Release policy

CI PASS is the only release qualification. The flow is:

```text
PR -> Basic CI -> merge main
   -> manual Release Check
      - Full CI
      - Security
      - amd64 Business Smoke + four finite single-instance recoveries
   -> PASS -> operator version confirmation -> vX.Y.Z tag
   -> Release
      - amd64 build + Controller image security + install/image smoke
      - arm64 build + Controller image security + install/image smoke
      - publish Agent assets and Controller GHCR images only after both pass
```

`Basic CI Result` remains the required PR check. Release Check is a manual
`workflow_dispatch` on merged `main`; every required job must succeed. Failure,
cancellation, or an unexpected skip cannot report PASS. Each invocation runs the
checks anew. There is no inherited acceptance or candidate nomination.

After Release Check passes, the operator confirms the version and creates the
version tag. CI qualification belongs to that operator process. The tag workflow
builds from the tag, scans the exact Controller image archives on each native
architecture, and performs install/image smoke checks before publication;
it does not rerun Full CI, Security, or Business acceptance, or look up previous
workflow results or artifacts to establish release eligibility.

Release products are Agent archives, plain archive checksums, DEB/RPM packages,
Controller images, and the bootstrap assets their installers consume. A plain
checksum detects download corruption; it is not a signature or trust root.
`controller-release.json`, while needed by Integrated lifecycle consumers, is
ordinary deployment configuration. Release publication uses normal GitHub asset
replacement on reruns and version image tags. It maintains no separate signing,
provenance, registry binding, or immutable-release contract.

AgentUpgrade retains the Controller-authorized `target_version`,
`package_sha256`, and `architecture`. The upgrade verifies the archive SHA-256
against that command, refuses a mismatch, and then uses existing protected
staging, architecture checks, safe extraction, and lifecycle execution. Runtime
command authorization, approvals, owner/epoch/fence/lease, idempotency, the Agent
SQLite journal, privd receipts, Unknown reconciliation, semantic payload hashes,
command/fence/receipt signatures, and durable immutable operation intent remain
required. The operator-provisioned release catalog still supplies authorized
upgrade package digests.

## Coverage ownership

| Behavior | Owner |
| --- | --- |
| Native online node, independently authenticated requester/approver, ConfigPlan apply, production Signer sealing, real VPN and internal TLS | amd64 Integrated Business Smoke |
| Controller/transport, Agent/privd, database and sole Relay recovery | Business Smoke with `run-resilience=true` |
| OIDC positive/negative paths; CSR, issue, one-use P12, revoke and persistence; real browser; offline queue and exact root effects | Manual integration |
| DEB/RPM install, retry/removal, state preservation and unsafe-package rejection on amd64/arm64 | Native package build/install smoke |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Release Controller products |
| Source/dependency vulnerabilities and repository secrets | Security |
| Tagged-source build and GitHub/GHCR publication | Release |

Business builds local products and installs from a loopback registry on
disposable amd64 runners. Native package checks require host, daemon, image and
executed ELF architecture to agree; cross compilation is not native acceptance.
Checks retain architecture/version, protected staging and SHA validation.
Never install test packages on shared hosts or clear their binfmt handlers.
Current checks do not certify historical upgrades or mixed-version safety.

## Diagnostics and build rehearsal

For extended [Business integration](real-business-validation.md):

```bash
gh workflow run release-upgrade.yml --ref <branch> \
  -f version=0.0.0 -f purpose=integration \
  -f production_signer=true -f run-resilience=true
```

`purpose=smoke` selects the smaller scope independently of Signer selection;
`production_signer=false` uses standalone topology. Diagnostics do not publish,
write GHCR or read production Secrets, and cannot replace Release Check.

For a build-only release rehearsal:

```bash
gh workflow run release.yml --ref main -f version=0.0.0
```

Both native Agent and Controller build/security/smoke legs and asset preparation
must pass; Publish must be skipped. Focused checks use
`scripts/test-release-upgrade.sh`, package/installer tests, relevant
ShellCheck/actionlint and [documentation checks](testing.md). Cover authorized
AgentUpgrade digest success/refusal and Stage-0 download/checksum failures when
changing those paths. Do not publish or deploy production during validation.

Rerun failed owners with all their checks; selected recovery reruns its complete
Business job. Failure, cancellation, missing results and unexpected required
skips cannot pass. Caches accelerate builds but are not accepted products or
qualification. Retain evidence, then clean only task-owned resources.
