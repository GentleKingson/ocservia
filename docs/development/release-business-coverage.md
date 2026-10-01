# Release business coverage ownership

The manual [Release Check](release-checks.md) always runs Full CI,
Security, and amd64 Integrated Business Smoke with the real production Signer,
internal TLS and `run-resilience=true`. A missing, failed, cancelled, or skipped
required result fails Release Check. Manual Release Diagnostics selects
`purpose=integration` for extended coverage independently of the Signer option.
Each run builds and tests local products on disposable native runners.

| Behavior | Owner |
| --- | --- |
| Online native node, independently authenticated requester/approver, ConfigPlan apply, production Signer password sealing, real VPN and internal TLS | Business Smoke |
| OIDC positive login and issuer/signature/nonce/code/state rejection | Manual integration |
| CSR, issue, P12 one-use export, revoke, restart persistence | Manual integration |
| Browser login, approvals, ConfigPlan and certificate actions | Manual integration |
| Single-Relay outage and unsent queue recovery, API/DB/journal/root-receipt cross-checks; one command succeeds with one real effect | Manual integration |
| Controller, Agent/privd, transport, database/API, and sole Relay recovery | Business Smoke with recovery |
| DEB/RPM install and state preservation on amd64/arm64 | Native package build/install smoke |
| Controller image execution on both architectures | Controller build/image smoke |
| Source and dependency vulnerability checks, necessary image vulnerability PASS/FAIL | Security / CI |
| Build tagged source and publish GitHub assets and GHCR version images | Release |

Runtime command and production PKI signing remain business behavior, independent
of package/release signing. CI does not hold the retired release signing key.
Release uses the CI result as its qualification and performs only
build/install/image smoke and ordinary publication.
