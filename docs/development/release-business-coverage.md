# Release business coverage ownership

Business Smoke is a fixed release check. Integration is selected from the
complete published-release-to-candidate diff by
[`release-selection.mjs`](../../scripts/release-selection.mjs), not by the
last PR. The [Release Check](release-checks.md) owns execution and aggregation.
An unselected Integration job is not evaluated, never an inherited PASS.
When Integration is selected, its existing extended path executes the core
deployment/apply/VPN/rollback chain in the same environment. The standalone
Business Smoke job is then skipped, not run a second time; Release Check
requires Integration success instead. When Integration is not selected,
standalone Business Smoke must succeed.

| Behavior | Owner | Decision |
| --- | --- | --- |
| Signed deployment, online native node, independently authenticated requester/approver, ConfigPlan apply, real VPN, automatic rollback and VPN after rollback | Business Smoke | Fixed release path |
| OIDC positive login and issuer/signature/nonce/code/state rejection | Integration | Retain until a concrete smaller-test equivalence is demonstrated |
| CSR, issue, P12 one-use export, revoke, restart persistence | Integration | Retain real production lifecycle |
| Browser login, approvals, ConfigPlan and certificate actions | Integration | Retain real browser assertions |
| Single-Relay outage, uncertain command recovery, API/DB/journal/root-receipt cross-checks | Integration | Retain distinct behavior; cross-host Resilience is not equivalent |
| DEB/RPM installation, supported upgrade and state preservation on both architectures | Package & Upgrade | Reuse candidate products |
| Cross-host failover, fencing, reconciliation and PITR | Resilience | Change-selected, never a blanket production SLO certification |
| Production release permission and signing key | Publish | Existing protected environment |

No high-level negative assertion has been deleted merely because Go or Web
tests pass. Integration currently retains the existing extended probe; its
environment is created only when selected. Further removal requires the exact
failure mode and its effective replacement test to be recorded here.
