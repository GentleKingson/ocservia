# Contracts and toolchains

Runtime and generator versions are pinned in `toolchains.lock`. A supported
macOS arm64 or Linux x86-64 host with `curl`, `tar`, `unzip`, `xz`, Java 17,
`jq`, and ShellCheck can prepare the remaining tools with:

```bash
make bootstrap
```

The canonical sources are `openapi/openapi.yaml` and the schemas below
`proto/`. Generate clients and message types with `make generate`; generated
directories are replaced on every run and must not be edited manually.

Use `make verify` before opening a pull request. It runs format and lint checks,
contract compatibility, language tests, generated-output cleanliness, and the
public repository policy check.
