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

## Bundled PostgreSQL initialization

Run the focused initializer regression only in an authorized, isolated
BuildServer environment, using a private checkout directory, host root, Python
3, `setpriv`, and a local Docker daemon. Do not use a production host, real
credentials, or existing database volumes. Select the target release's
digest-pinned PostgreSQL 17 image rather than `latest`:

```bash
python3 scripts/test-postgres-init-observation.py \
  --image "${OCSERV_POSTGRES_IMAGE:?set the target PostgreSQL 17 digest}" \
  --expect-argv no
```

The test uses generated task directories, fake credentials, an internal Docker
network and the real image entrypoint. It requires successful argv reads before
checking that passwords and their base64 forms are absent. It also exercises
special characters, existing-role preservation, both role logins, physical
backup verification, invalid passwords, error propagation and no HBA append
after an SQL error. A catalog lock extends the observation window; its duration
is not the duration of an ordinary initialization.

Its log and temporary-file observations are supplementary: collection success
is not asserted, and matching covers complete raw/base64 values, not every
SQL-escaped representation. Negative results do not establish that all output
channels are free of credentials. Keep this limitation with saved results.
Record the candidate commit, script digest, image identity, command status and
JSON output; do not relabel historical runs as executions of a new commit.
The script cleans up its task-owned container, network and temporary directory.
This privileged regression is manual; a green workspace CI does not imply it ran.

## Authoritative references

- The current targets are defined in [`Makefile`](../../Makefile).
- The pull-request job and relevance map are in [GitHub Actions validation](github-actions.md).
- Machine-readable release-readiness contracts are in [`acceptance/`](../acceptance/README.md).
