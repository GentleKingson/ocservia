# Release business coverage ownership

The target manual [Release Check](release-checks.md) always runs Full CI,
Security, and the existing extended Business Integration with
`run-resilience=true`. Business Integration owns the core smoke chain and the
extended checks in one environment. Resilience must execute its real recovery
scenarios and return PASS; a missing, failed, cancelled, or skipped required
result fails Release Check. The migration must preserve these assertions while
removing release signing and candidate/provenance transport requirements.

| Behavior | Owner |
| --- | --- |
| Online native node, independently authenticated requester/approver, ConfigPlan apply, real VPN, automatic rollback and VPN after rollback | Business Integration |
| OIDC positive login and issuer/signature/nonce/code/state rejection | Business Integration |
| CSR, issue, P12 one-use export, revoke, restart persistence | Business Integration |
| Browser login, approvals, ConfigPlan and certificate actions | Business Integration |
| Single-Relay outage and unsent queue recovery, API/DB/journal/root-receipt cross-checks; one command succeeds with one real effect | Business Integration |
| Controller, Agent/privd, transport, database/API, and sole Relay recovery | Business Integration with Resilience |
| DEB/RPM install and state preservation on amd64/arm64 | Native package build/install smoke |
| Controller image execution on both architectures | Controller build/image smoke |
| Source and dependency vulnerability checks, necessary image vulnerability PASS/FAIL | Security / CI |
| Build tagged source and publish GitHub assets and GHCR version images | Release |

Runtime command and production PKI signing remain business behavior, independent
of package/release signing. CI does not hold the retired release signing key.
There is no separate accepted product, nomination, artifact binding, or registry
evidence prerequisite for Release. Until implementation catches up, the
migration status in the policy applies to the existing executable workflows.
