#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands python3 tar gzip xz

# These disposable command fixtures test routing and failures, not native Go.
python3 - "${ROOT}" <<'PY'
import glob
import hashlib
import os
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

root = Path(sys.argv[1])
version = None
for line in (root / "toolchains.lock").read_text().splitlines():
    if line.startswith("go="):
        version = line.split("=", 1)[1]
    elif line.startswith("node="):
        node_version = line.split("=", 1)[1]
    elif line.startswith("npm="):
        npm_version = line.split("=", 1)[1]
    elif line.startswith("rust="):
        rust_version = line.split("=", 1)[1]
    elif line.startswith("nfpm="):
        nfpm_version = line.split("=", 1)[1]
assert version is not None
checksums = (root / "scripts" / "checksums.txt").read_text().splitlines()
bash = shutil.which("bash")
assert bash is not None


def script(path: Path, body: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(f"#!/usr/bin/env bash\nset -eu\n{body}\n")
    path.chmod(0o755)


def run(env: dict[str, str], bash_bin: str, path: Path, *args: str, error: str | None = None) -> str:
    proc = subprocess.run(
        [bash_bin, str(path), *args],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    output = proc.stdout
    if error is not None:
        if proc.returncode == 0 or error not in output:
            raise AssertionError(f"expected {error!r}: {proc.returncode}\n{output}")
        if "unbound variable" in output:
            raise AssertionError(f"unbound variable diagnostic: {output}")
    elif proc.returncode != 0:
        raise AssertionError(f"unexpected failure: {proc.returncode}\n{output}")
    return output


def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def has_checksum(artifact: str) -> bool:
    return any(
        len(line) > 66 and line[64:66] == "  " and line[66:] == artifact and all(c in "0123456789abcdef" for c in line[:64])
        for line in checksums
    )


with tempfile.TemporaryDirectory(prefix="bootstrap-platforms-") as tmp_dir:
    tmp = Path(tmp_dir)
    platforms = {
        "Linux-aarch64": "linux-arm64",
        "Linux-x86_64": "linux-amd64",
        "Darwin-arm64": "darwin-arm64",
    }
    for platform, target in platforms.items():
        os_name, arch = target.split("-")
        artifact = f"go{version}.{target}.tar.gz"
        if not has_checksum(artifact):
            raise AssertionError(f"missing locked checksum for {artifact}")
        work = tmp / platform
        (work / "scripts").mkdir(parents=True, exist_ok=True)
        for name in ("bootstrap.sh", "env.sh"):
            shutil.copy2(root / "scripts" / name, work / "scripts" / name)
        shutil.copy2(root / "toolchains.lock", work / "toolchains.lock")
        payload = work / "payload"
        script(
            payload / "go" / "bin" / "go",
            f'echo executed >>"${{FIXTURE_EXECUTIONS}}"\n'
            f'[[ "$*" == \'env GOVERSION GOHOSTOS GOHOSTARCH\' ]]\n'
            f"printf 'go{version}\\n{os_name}\\n{arch}\\n'",
        )
        archive = work / "fixture.tar.gz"
        subprocess.run(["tar", "-czf", str(archive), "-C", str(payload), "go"], check=True)
        digest = sha256_file(archive)
        manifest = work / "scripts" / "checksums.txt"
        manifest.write_text(f"{digest}  {artifact}\n")
        bin_dir = work / "bin"
        sys_name, mach_name = platform.split("-")
        script(bin_dir / "uname", f'case "$1" in -s) echo {sys_name} ;; -m) echo {mach_name} ;; esac')
        script(bin_dir / "jq", "exit 0")
        script(
            bin_dir / "curl",
            'printf \'%s\\n\' "$*" >>"${FIXTURE_DOWNLOADS}"\n'
            'while (($#)); do\n'
            '  if [[ "$1" == --output ]]; then cp "${FIXTURE_ARCHIVE}" "$2"; exit; fi\n'
            '  shift\n'
            'done\n'
            'exit 1',
        )
        env = {
            **os.environ,
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "FIXTURE_ARCHIVE": str(archive),
            "FIXTURE_DOWNLOADS": str(work / "downloads"),
            "FIXTURE_EXECUTIONS": str(work / "executions"),
            "GOOS": "windows",
            "GOARCH": "386",
            "GOTOOLCHAIN": "auto",
        }
        bootstrap = work / "scripts" / "bootstrap.sh"
        run(env, bash, bootstrap, "go-test")
        if f"https://go.dev/dl/{artifact}" not in (work / "downloads").read_text():
            raise AssertionError("wrong URL or artifact")
        if sorted(p.name for p in (work / ".tools").iterdir()) != ["bin", "go"]:
            raise AssertionError("go-test installed extra tools")
        installed = work / ".tools" / "go" / "bin" / "go"
        before = installed.stat()
        (work / ".tools" / "node").mkdir(parents=True, exist_ok=True)
        (work / ".tools" / "node" / "keep").write_text("untouched")
        run(env, bash, bootstrap, "go-test")
        after = installed.stat()
        if after.st_ino != before.st_ino or after.st_mtime_ns != before.st_mtime_ns:
            raise AssertionError("valid Go reinstalled")
        if len((work / "downloads").read_text().splitlines()) != 1:
            raise AssertionError("unnecessary download")
        if (work / ".tools" / "node" / "keep").read_text() != "untouched":
            raise AssertionError("other tool changed")

        if platform != "Linux-aarch64":
            continue
        installed.write_text(installed.read_text().replace("arm64", "amd64", 1))
        run(env, bash, bootstrap, "go-test")
        if "arm64" not in installed.read_text():
            raise AssertionError("wrong host reused")
        installed.chmod(0o644)
        run(env, bash, bootstrap, "go-test")
        if not os.access(installed, os.X_OK):
            raise AssertionError("non-executable Go reused")

        old = installed.read_text().replace(f"go{version}", "go0.0.0", 1)
        installed.write_text(old)
        cache = work / ".cache" / "downloads" / artifact
        manifest.write_text("")
        run(env, bash, bootstrap, "go-test", error=f"missing or invalid SHA-256 checksum for {artifact}")
        manifest.write_text(f"{'0' * 64}  {artifact}\n")
        run(env, bash, bootstrap, "go-test", error="checksum mismatch")
        manifest.write_text(f"{digest}  {artifact}\n")
        cache.write_text("corrupt cached archive")
        run(env, bash, bootstrap, "go-test", error="checksum mismatch")
        if len((work / "downloads").read_text().splitlines()) != 1:
            raise AssertionError("cached failure downloaded a replacement")
        cache.unlink()
        manifest.write_text(f"{'0' * 64}  {artifact}\n")
        run(env, bash, bootstrap, "go-test", error="checksum mismatch")
        if cache.exists():
            raise AssertionError("unverified download promoted to cache")
        if installed.read_text() != old:
            raise AssertionError("failed verification overwrote old Go")

        for failure in ("wrong-host", "non-executable"):
            go_bin = payload / "go" / "bin" / "go"
            go_bin.write_text(installed.read_text().replace("go0.0.0", f"go{version}", 1).replace("arm64", "amd64", 1))
            go_bin.chmod(0o644 if failure == "non-executable" else 0o755)
            subprocess.run(["tar", "-czf", str(archive), "-C", str(payload), "go"], check=True)
            cache.unlink(missing_ok=True)
            manifest.write_text(f"{sha256_file(archive)}  {artifact}\n")
            run(env, bash, bootstrap, "go-test", error="invalid Go toolchain")
            if installed.read_text() != old:
                raise AssertionError("invalid toolchain replaced old Go")
            if glob.glob(str(work / ".tools" / ".go-install-*")):
                raise AssertionError("staging directory leaked")

        shutil.rmtree(work / ".tools", ignore_errors=True)
        shutil.rmtree(work / ".cache", ignore_errors=True)
        for profile in ("all", "ci-quality", "contracts", "secret-scan", "go-quality", "go-rust-integration", "native", "rust-validation", "web", "security"):
            run(env, bash, bootstrap, profile, error=f"{platform}/{profile}; artifact mappings")
            if (work / ".tools").exists() or (work / ".cache").exists():
                raise AssertionError("unsupported profile partially installed")

        script(work / ".tools" / "cargo" / "bin" / "rustc", f"echo 'rustc {rust_version} (fixture)'")
        for name in ("cargo", "rustup", "rustfmt"):
            script(work / ".tools" / "cargo" / "bin" / name, "exit 0")
        script(work / ".tools" / "bin" / "nfpm", f"echo 'GitVersion: {nfpm_version}'")
        run(env, bash, bootstrap, "rust-basic")
        run(env, bash, bootstrap, "native-packages")

        node_artifact = f"node-v{node_version}-linux-arm64.tar.xz"
        if not has_checksum(node_artifact):
            raise AssertionError("missing locked ARM64 Node checksum")
        node_payload = work / "node-payload"
        script(node_payload / "node-fixture" / "bin" / "node", f"echo v{node_version}")
        script(node_payload / "node-fixture" / "bin" / "npm", f"echo {npm_version}")
        node_archive = work / "node-fixture.tar.xz"
        subprocess.run(["tar", "-cJf", str(node_archive), "-C", str(node_payload), "node-fixture"], check=True)
        manifest.write_text(f"{sha256_file(node_archive)}  {node_artifact}\n")
        node_env = {**env, "FIXTURE_ARCHIVE": str(node_archive)}
        run(node_env, bash, bootstrap, "npm-security")
        if f"https://nodejs.org/dist/v{node_version}/{node_artifact}" not in (work / "downloads").read_text():
            raise AssertionError("wrong ARM64 Node artifact URL")
        (work / ".tools" / "node" / "bin" / "node").write_text("invalid installed node")
        manifest.write_text(f"{'0' * 64}  {node_artifact}\n")
        run(node_env, bash, bootstrap, "npm-security", error="checksum mismatch")
        script(bin_dir / "uname", "echo unsupported")
        run(env, bash, bootstrap, "go-test", error="unsupported-unsupported/go-test; no artifact mappings")

    work = tmp / "missing-tools"
    (work / "scripts").mkdir(parents=True, exist_ok=True)
    for name in (
        "env.sh", "go-test-environment.sh", "go-check.sh", "required-go-tests.sh",
        "test-required-go-tests.sh", "test-bootstrap-profiles.sh",
        "database-integration.sh", "database-postgres-smoke.sh",
    ):
        shutil.copy2(root / "scripts" / name, work / "scripts" / name)
    bin_dir = work / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    for name in ("bash", "dirname", "mktemp", "rm"):
        found = shutil.which(name)
        assert found is not None
        (bin_dir / name).symlink_to(found)
    script(
        bin_dir / "go",
        'case "$*" in\n'
        '  \'env CGO_ENABLED\') echo "${FIXTURE_CGO:-1}" ;;\n'
        '  \'env CC\') echo "${FIXTURE_CC:-cc}" ;;\n'
        '  *) echo \'expensive Go invocation reached\' >>"${FIXTURE_LAUNCHED}"; exit 99 ;;\n'
        'esac',
    )
    for name in ("gofmt", "jq", "setsid", "tee", "python3", "openssl", "curl", "sha256sum", "patch", "timeout"):
        script(bin_dir / name, "exit 0")
    script(bin_dir / "docker", '[[ "${FIXTURE_DOCKER:-0}" == 0 ]]')
    script(bin_dir / "cc", "exit 1")
    env = {"PATH": str(bin_dir), "FIXTURE_LAUNCHED": str(work / "launched"), "DATABASE_TEST_SCOPE": "regression"}
    preflight_cases = [
        ("go-check.sh", "standard", "go"),
        ("go-check.sh", "standard", "gofmt"),
        ("required-go-tests.sh", "unit", "setsid"),
        ("test-required-go-tests.sh", None, "python3"),
        ("test-bootstrap-profiles.sh", None, "python3"),
        ("database-integration.sh", None, "python3"),
        ("database-postgres-smoke.sh", None, "python3"),
        ("database-postgres-smoke.sh", None, "docker"),
    ]
    for name, argument, missing in preflight_cases:
        (bin_dir / missing).rename(bin_dir / f"hidden-{missing}")
        args = (argument,) if argument is not None else ()
        run(env, bash, work / "scripts" / name, *args, error=f"required test command is missing: {missing}")
        (bin_dir / f"hidden-{missing}").rename(bin_dir / missing)
    run({**env, "FIXTURE_CGO": "0"}, bash, work / "scripts" / "go-check.sh", "race", error="-race requires CGO_ENABLED=1")
    run({**env, "FIXTURE_CC": "missing-cc"}, bash, work / "scripts" / "go-check.sh", "race", error="required test command is missing: missing-cc")
    run(env, bash, work / "scripts" / "go-check.sh", "race", error="C compiler that can compile and link")
    run({**env, "FIXTURE_DOCKER": "1"}, bash, work / "scripts" / "database-postgres-smoke.sh", error="access to a running Docker daemon")
    for major in ("18", "all"):
        run(
            {**env, "DATABASE_TEST_SCOPE": "full", "PG_MAJOR": major, "FIXTURE_DOCKER": "1"},
            bash,
            work / "scripts" / "database-integration.sh",
            error="access to a running Docker daemon",
        )
    if (work / "launched").exists():
        raise AssertionError("preflight started Go tests")
print("Simulated bootstrap platforms, reuse, verification failures and entrypoint preflights passed (not native execution)")
PY
