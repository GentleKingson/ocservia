# Validate a change

Use the smallest validation that covers the code or documentation you changed.
GitHub Actions remains authoritative for the exact pull request commit.

## Most changes

```bash
make bootstrap
make verify
```

For documentation-only changes, the focused checks are:

```bash
make docs-check
make policy-check
git diff --check
```

## Choose a broader check when needed

- Database migrations or database behavior: `make database-integration`
- Go and transport local integration: `make integration`
- Browser or runtime behavior: `make e2e`
- Rust behavior or boundaries: `make rust-check`
- Web behavior: `make web-check`
- Formal release/readiness: use the G6 workflow and read [G6 readiness](g6-readiness.md)

Do not run the formal G6 harness for an ordinary documentation change unless
the change touches its acceptance contracts or execution paths.

## Authoritative references

- The current targets are defined in [`Makefile`](../../Makefile).
- The pull-request job and relevance map are in [GitHub Actions validation](github-actions.md).
- Machine-readable release-readiness contracts are in [`acceptance/`](../acceptance/README.md).
