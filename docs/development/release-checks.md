# Release policy

CI PASS is the only release qualification. The flow is:

```text
PR -> Basic CI -> merge main
   -> manual Release Check (Full CI + Security + Business Integration + Resilience)
   -> PASS -> human version confirmation -> GitHub Release / vX.Y.Z tag
   -> Release (build + smoke + publish Agent assets and Controller GHCR images)
```

`Basic CI Result` remains the required PR check. Release Check is a manual
`workflow_dispatch` on merged `main`; every required job must succeed. Failure,
cancellation, or an unexpected skip cannot report PASS. Each invocation runs the
checks anew. There is no inherited acceptance or candidate nomination.

After Release Check passes, the operator confirms the version and creates the
Release/tag. CI qualification belongs to that operator process. The tag workflow
builds from the tag and performs build/install smoke checks before publication;
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

## Validation

Before completing the refactor, run Basic CI, Full CI, Security, Business
Integration with `run-resilience=true`, both native Agent build/install smokes,
Controller build/image smoke, and a dispatch of Release that performs no
production writes. Verify AgentUpgrade digest success/refusal and Stage-0
download success/failure and checksum mismatch if a plain Stage-0 checksum is retained. Audit the retired release-chain
references across the repository. Do not publish a version or deploy production
as part of this validation.

Use [business coverage ownership](release-business-coverage.md) for the checks
that must survive the migration and [validation guidance](testing.md) for focused
PR checks. Build caches remain optional compilation accelerators; they are never
release qualification or accepted products.
