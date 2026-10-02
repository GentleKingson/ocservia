# Contracts and toolchains

Use the [contract inventory](../reference/stable-contracts.md) for public and
matched-version boundaries, and [validation guidance](testing.md) to select
checks. Neither source checks nor matching versions establish release acceptance.

Runtime and generator versions are pinned in `toolchains.lock`. A supported
macOS arm64 or Linux x86-64 host with `curl`, `tar`, `unzip`, `xz`, Java 17,
`jq`, and ShellCheck can prepare the remaining tools with:

```bash
make bootstrap
```

The canonical sources are `openapi/openapi.yaml` and the schemas below
`proto/`. Generate clients and message types with `make generate`; generated
directories are replaced on every run and must not be edited manually.

`scripts/docs-check.sh` checks line endings, nonempty tracked Markdown and a
small set of public bootstrap entrypoint requirements. It does not validate
SQL, links/anchors, authentication modes or database support. Changed executable
examples need focused validation on BuildServer; passing this check alone is
not operational acceptance.

GitHub Actions on the pinned `ubuntu-24.04` hosted runner is the authoritative
pull-request validation environment. The workflow bootstraps from
`toolchains.lock`, verifies downloaded tools against `scripts/checksums.txt`,
and selects the basic docs, Go, Rust, Web, and database checks from a
fresh checkout. Contract compatibility and generated-clean checks remain
manual commands, not Basic CI jobs.

Use `make verify` only when broader validation is needed; reuse prepared tools
instead of bootstrapping every change. Local success does not replace
[required Actions checks](github-actions.md).
