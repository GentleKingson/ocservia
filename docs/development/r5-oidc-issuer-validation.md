# R5: Preserve the OIDC issuer identifier

## Scope

- Starting SHA: `bdd5103db7232063852bdd2f499d9f2a5fe769b1`; clean working tree.
- Reference baseline: `7e463a34bc8363021daeae12101d3cbd215e24ae`.
- Read repository `AGENTS.md`; no scoped AGENTS/override files were found under
  the changed directories. Existing R3/R4 behavior is retained.
- No dependency upgrade, commit, PR, deployment or production identity migration.

## Change and verification chain

The only production-code change is `issuer: cfg.Issuer` in `auth.New`, replacing
`strings.TrimSuffix(cfg.Issuer, "/")`. Config loading and app wiring already
preserve the issuer. HTTPS, URL and complete-credential checks are unchanged.

The locked dependency is still `github.com/coreos/go-oidc/v3 v3.17.0`
(`golang.org/x/oauth2 v0.36.0`). Its actual cached source was inspected on
BuildServer: `oidc.NewProvider` removes one slash only when constructing the
well-known request URL, compares Discovery's issuer to the original issuer, and
retains that original value for the token verifier. Signature, audience, expiry
and issuer checks remain enabled. The dependency's preexisting Google-specific
issuer exception is unchanged; no new issuer exception or insecure context was
introduced. The application still checks state and nonce before session creation.

`CompleteLogin` uses this exact issuer with the verified subject for the
`identities` upsert. Migration 000011 defines `UNIQUE (issuer, subject)`;
`auth_sessions` and `role_bindings` reference the identity UUID. Authentication
reads the identity through that UUID and retains disabled/revoked/expiry checks.
Local uses `issuer=local`; Break-glass uses `break-glass/offline`. Neither is
rewritten, and email/display name do not select or merge identities.

## Compatibility and migration judgment

Working no-slash issuers keep their original key, identity ID, role ownership
and session behavior. A genuine slash issuer previously failed Discovery before
identity insertion; no successful old login or migration requirement can be
inferred from that failure. A token issuer mismatch likewise failed before SQL.

Old rows are possible if metadata and tokens both used the trimmed issuer, or
through separate provisioning/history. Correcting the configured issuer to the
still-authoritative no-slash value needs no row migration. A real change of
issuer changes the unique identity key and must not implicitly inherit another
identity's roles, even with equal subjects or profile claims. Production data was
not inspected, so no particular deployment is declared to need a migration.
The [operations guide](../operations/authentication.md#correcting-a-historical-issuer-configuration)
contains explicit conflict/role checks, approval, backup, transaction and rollback
steps for a separately authorized migration. No automatic migration was added.

## BuildServer evidence

All execution used `ssh BuildServer`, in `/tmp/ocservia-r5.Wj6QGg`, with a new
PostgreSQL 17 container and separate schema-owner/runtime roles. The IdP is an
isolated loopback HTTP test server serving actual Discovery, token and RSA JWKS
endpoints. HTTP is confined to this test fixture; production HTTPS configuration
and untrusted-TLS rejection are tested separately. This is not a production IdP
or browser end-to-end test. The dedicated database container is removed on exit.

`baseline.log` runs the new tests with the starting SHA's original `service.go`:
no-slash passes, while `/` and `/tenant/` fail at actual Discovery issuer matching.
`final.log` and `run.sh` record the final candidate checks:

```sh
go build -o /work/ocserv-control ./cmd/ocserv-control
/work/ocserv-control --migrate-only
go test -p 1 -race -count=1 -v ./internal/auth -run 'Test(OIDCAuthorizationCodePKCEIntegration|LocalAuthenticationIntegration|BreakGlassAlertsAuditsAndRequiresRotationIntegration|OptionalAuthenticationProviders|OIDCTLSAndIssuerOutagesFailClosed)$'
go test -count=1 -v ./internal/platform/config -run 'Test(ProductionAuthenticationConfiguration|OIDCDevelopmentConfiguration)$'
```

The candidate passes all selected tests without skips. Each of the three issuer
forms makes 12 real Discovery requests, with no injected verifier. Coverage
includes Discovery mismatch, token `iss` differing only by slash, wrong audience,
signature, nonce, state, expiry, repeated login without a new identity, preserved
legacy ID/role ownership, distinct slash/no-slash identities, bounded existing
sessions during IdP outage, disabled-identity denial, Local login and Break-glass
audit/rotation. Production configuration checks include exact issuer preservation
with a port/path/slash, existing HTTP/userinfo/query/fragment rejection and all
32 Local/OIDC completeness combinations.

Initial isolated-run setup failures (missing runtime migration role, macOS archive
metadata and copied directory ownership) were corrected in the test environment,
without weakening configuration checks or modifying production. Go formatting and
diff whitespace checks passed. No full repository suite was run.

## Sources

- [OIDC Discovery 1.0, configuration validation](https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderConfigurationValidation)
- [Locked go-oidc NewProvider implementation](https://github.com/coreos/go-oidc/blob/v3.17.0/oidc/oidc.go)
- [Locked go-oidc token verification](https://github.com/coreos/go-oidc/blob/v3.17.0/oidc/verify.go)
