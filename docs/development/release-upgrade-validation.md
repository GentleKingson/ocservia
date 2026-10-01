# Current package validation

Use [Release Check](release-checks.md) on merged `main` for publication
qualification. It always runs Full CI, Security, and Integrated Business Smoke with
single-instance recovery. Historical native upgrade and mixed-version compatibility checks
remain removed; current checks do not guarantee cross-version safety.

`Release Diagnostics` uses the same Business driver for manual investigation:

```bash
gh workflow run release-upgrade.yml --ref <branch> \
  -f version=0.0.0 -f purpose=integration \
  -f production_signer=true -f run-resilience=true
```

Integrated is the default and uses the actual Signer on disposable native
amd64 runners. It freshly builds the packages and images, scans OS
vulnerabilities, and installs from a local loopback registry. It does not write
GHCR, create tags, publish GitHub Releases, or read production Secrets.
`purpose=smoke` selects smoke independently of `production_signer`;
`production_signer=false` selects the standalone topology. This diagnostic run it cannot replace the complete manual Release Check.

Both native architecture builds retain real ELF architecture/version checks,
protected archive staging, SHA checks, installation/retry/removal smoke and
unsafe-package rejection. The host, daemon, image platform and executed ELF
architecture must agree; cross compilation is not native acceptance. Do not
install test packages on a shared host or clear its binfmt handlers.

Rerun a failed owner job with all its actual checks; selected Resilience must
rerun its complete Business job. Failures, cancellations and required skips
cannot report PASS. CI caches are optional build accelerators.

For a complete build-only release rehearsal:

```bash
gh workflow run release.yml --ref main -f version=0.0.0
```

This runs both Agent build/install smoke legs, both Controller image smokes,
and prepares the ordinary package/configuration assets. Dispatch never enters
Publish. Tag publication separately builds from its tag and uses ordinary
asset replacement on reruns.

Focused contracts run through `scripts/test-release-upgrade.sh`, package and
installer tests, relevant ShellCheck/actionlint, and documentation checks.
Actual host installation belongs on authorized disposable runners. Cleanup
must remain limited to task-owned resources.
