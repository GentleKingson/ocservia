# Release business coverage ownership

This inventory assigns every former broad-probe assertion before reducing the
manually dispatched T07. "Related" component checks do not replace the same
real production-path assertion. Until equivalent coverage is demonstrated, the
`business_profile=extended` manual supplemental run owns it and must pass on
the same candidate SHA before release acceptance. The smoke and supplemental
profiles use the same real production binaries and disposable amd64 topology.

| Current T07 assertion | Other coverage found | Runtime owner now | Decision |
| --- | --- | --- | --- |
| Signed candidate Controller and native Agent installation, systemd, login, separate requester/approver, ConfigPlan apply, real OpenConnect VPN, automatic rollback, VPN after rollback | T06 covers native installation/upgrade on both architectures, but not this business chain | T07 Smoke | Keep |
| OIDC discovery/PKCE and negative issuer, signature, nonce, code, state cases | Full CI Go auth tests cover components, not the signed deployment | Specialized extended business validation | Retain the original real-path checks |
| CSR, issue, P12 one-use download, revoke and restart | Go integration, native package, and web tests cover parts, not the full production lifecycle | Specialized extended business validation | Retain the original real-path checks |
| Browser login, approvals, ConfigPlan and certificate workflow | Web E2E specs exist, but Basic CI runs `web-check.sh basic`, not the full browser suite | Specialized extended business validation | Retain the original real-browser checks |
| Controller pause, single-Relay outage, uncertain command recovery | G6 covers fault domains, Relay transitions and recovery, but not this exact single-Relay command replay | Specialized extended business validation; G6 for its own topology | Retain the distinct assertion until an equivalence mapping is reviewed |
| API, Controller DB, Agent journal and root-effect receipt cross-checks | G6 verifies its own raw evidence and receipt paths, not every business operation | Specialized extended business validation | Retain cross-source checks on supplemental operations |
| amd64/arm64 DEB/RPM upgrade and rollback matrix | T06 four native Agent/Controller cells | T06 | Do not add to T07 |
| HA, fault domains, PITR and production SLO | G6 formal production readiness | G6 | Do not add to T07 |
| Release publication approval | `release-publishing` GitHub Environment | Release workflow | Do not add to T07 |

The checks above have not been deleted: they are invoked by the extended
profile of the same manual workflow. A smoke PASS alone does not transfer their
ownership to Full CI or G6. T07 remains manual; there is no PR router or
evidence inheritance until those contracts are defined.
