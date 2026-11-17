#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
python3 - <<'PY'
import hashlib
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path
import yaml

products = yaml.safe_load(Path(".github/workflows/release-products.yml").read_text())
steps = products["jobs"]["build-agent-packages"]["steps"]
target = next(s for s in steps if s.get("id") == "agent-target")["run"]
build = Path("scripts/build-agent-binaries.sh").read_text()
assert (
    'sha256sum rust/agent-build.Dockerfile | cut -c1-16' in target
    and 'sha256sum "${ROOT}/rust/agent-build.Dockerfile" | cut -c1-16' in build
    and 'path=rust/target/agent-${{ inputs.arch }}-${builder_hash}' in target
    and 'rust/target/agent-${BUILD_CACHE_KEY}' in build
), "target must use the same builder hash as compilation"
restore = next(s for s in steps if s.get("id") == "agent-target-cache")
save = next(s for s in steps if s.get("name") == "Save Rocky Agent compilation cache")
assert (
    restore["uses"].startswith("actions/cache/restore@")
    and save["uses"].startswith("actions/cache/save@")
), "cache must be restored and saved explicitly"
assert (
    restore["with"]["path"] == "${{ steps.agent-target.outputs.path }}"
    and save["with"]["path"] == restore["with"]["path"]
), "cache must contain only the isolated target directory"
key = restore["with"]["key"]
for boundary in (
    "runner.os", "inputs.arch", "rust/agent-build.Dockerfile", "toolchains.lock",
    "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh",
    "scripts/build-agent-binaries.sh", "rust/rust-toolchain.toml",
    "rust/.cargo/**", "rust/**/Cargo.toml", "rust/Cargo.lock",
):
    assert boundary in key, f"cache identity lost {boundary}"
assert restore["with"]["restore-keys"] == key.removesuffix("${{ github.sha }}"), "fallback must not cross build identities"
assert save["with"]["key"] == "${{ steps.agent-target-cache.outputs.cache-primary-key }}", "save must use the new source identity, not the restored key"
validate_step = next(s for s in steps if s.get("name", "").startswith("Validate native package lifecycle"))
assert (
    save.get("if") == "${{ success() && steps.agent-target-cache.outputs.cache-hit != 'true' }}"
    and steps.index(save) > steps.index(validate_step)
), "save must follow successful validation"
compile_step = next(s for s in steps if "scripts/build-release-agent.sh" in s.get("run", ""))
assert "if" not in compile_step, "a cache hit must never skip binary compilation"
assert (
    "cargo build --locked --release" in build
    and "glibc 2.34" in build
    and '[[ "$("${path}" --version)" == "${binary} ${VERSION}" ]]' in build
), "native ABI and package version checks must remain"
builder = Path("rust/agent-build.Dockerfile").read_text()
for option in ("minrate=512k", "timeout=15", "max_parallel_downloads=10"):
    assert f"--setopt={option}" in builder, f"DNF slow-mirror bound missing {option}"
assert not re.search(
    r"retries=|fastestmirror=|mirrorlist=|baseurl=|gpgcheck=0|sslverify=0|--nogpgcheck",
    builder,
), "keep official mirror selection, retry defaults and package verification"
dockerfile = Path("rust/transportd.Dockerfile").read_text()
assert (
    "COPY rust/crates ./crates\nRUN cargo chef prepare --recipe-path recipe.json" in dockerfile
), "recipe must include the complete workspace, not a hand-maintained crate list"
assert (
    "cargo chef cook --locked --release --package ocservia-transportd --recipe-path recipe.json" in dockerfile
    and "cargo build --locked --release --package ocservia-transportd" in dockerfile
), "cook must match the real build package/profile/lock"
_, application = dockerfile.split("FROM dependencies AS build", 1)
dependencies = dockerfile.split("FROM dependencies AS build", 1)[0]
assert all(
    line in application for line in ("COPY rust/crates ./crates", "COPY rust/vendor ./vendor", "COPY rust/.cargo ./.cargo")
), "real sources, patched dependencies and rustflags must replace the recipe skeleton"
assert (
    "COPY rust/.cargo ./.cargo" in dependencies and "COPY rust/vendor ./vendor" in dependencies
), "dependency layer must retain config and patched source inputs"
print("Release Rust cache identity, unconditional rebuild and dependency layering contracts passed")
PY
python3 - <<'PY'
import os
import re
import subprocess
import tempfile
from collections import defaultdict
from pathlib import Path
import yaml


def check(value, message: str) -> None:
    if not value:
        raise SystemExit(message)


workflows = {
    name: yaml.safe_load(Path(f".github/workflows/{name}.yml").read_text())
    for name in ("ci", "security", "release-business-diagnostic")
}
for name, workflow in workflows.items():
    for job in workflow["jobs"].values():
        for step in job.get("steps", []):
            if not step.get("uses", "").startswith("actions/cache"):
                continue
            options = step["with"]
            paths = [line.strip() for line in options["path"].splitlines()]
            check(all("node_modules" not in path for path in paths), f"{name} must not cache installed npm dependencies")
            key = options["key"]
            if not key.startswith("tooling-"):
                continue
            check("github.sha" not in key, f"{name} stable tools must survive source changes")
            for inp in ("runner.os", "runner.arch", "toolchains.lock", "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh"):
                check(inp in key, f"{name} tool cache lost {inp}")
            check("rust/target" not in paths, f"{name} tools must not include compilation output")

ci_jobs = workflows["ci"]["jobs"]
rust_cache = next((s for s in ci_jobs["rust"]["steps"] if (s.get("with") or {}).get("path") == "rust/target/debug"), None)
check(rust_cache and rust_cache.get("if") == "needs.ci-relevance.outputs.run_rust == 'true'", "Rust object cache must follow Rust routing")
key = rust_cache["with"]["key"]
for inp in ("toolchains.lock", "rust/rust-toolchain.toml", "rust/Cargo.lock", "rust/**/Cargo.toml", "rust/.cargo/**", "rust/vendor/**", "scripts/rust-check.sh"):
    check(inp in key, f"Rust object cache lost {inp}")
check(rust_cache["with"]["restore-keys"] == key.removesuffix("${{ github.sha }}"), "Rust objects must restore only within their build identity")
for job in (ci_jobs["web"], workflows["security"]["jobs"]["scan"]):
    cache = next((s for s in job["steps"] if (s.get("with") or {}).get("path") == ".cache/npm"), None)
    check(cache and "web/**/package-lock.json" in cache["with"]["key"], "npm download cache must track lockfiles")
check('(cd "${ROOT}/web" && npm ci)' in Path("scripts/bootstrap.sh").read_text(), "deterministic npm install must remain")

business = workflows["release-business-diagnostic"]["jobs"]["business"]["steps"]
products = yaml.safe_load(Path(".github/workflows/release-products.yml").read_text())["jobs"]["build-agent-packages"]["steps"]
for step_id in ("agent-target", "agent-target-cache"):
    actual = next((s for s in business if s.get("id") == step_id), None)
    expected = next((s for s in products if s.get("id") == step_id), None)
    check(actual == expected, f"Business {step_id} must match the native release builder")
check(all("rust/target" not in [line.strip() for line in str((s.get("with") or {}).get("path", "")).splitlines()] for s in business), "Business must not cache all Rust targets")

targets = []
for line in Path("scripts/build-release-controller.sh").read_text().splitlines():
    if re.match(r"^\s*build_image\s", line):
        match = re.match(r"^\s*build_image ([a-z_]+) ([\w./-]+Dockerfile)\s*$", line)
        check(match, f"unsupported Controller build_image call: {line.strip()}")
        targets.append(match.groups())
check(len(targets) == 7 and len({img for img, _ in targets}) == len(targets), "Controller builder must declare all seven distinct image targets")

probes: dict[str, list[str]] = defaultdict(list)
for image, dockerfile in targets:
    probes[dockerfile].append(image)
    for line in Path(dockerfile).read_text().splitlines():
        check(not re.match(r"^\s*ADD\b", line, re.I), f"ADD inputs need explicit Controller routing coverage: {dockerfile}")
        if not re.match(r"^\s*COPY\b", line, re.I):
            continue
        words = line.strip().split()[1:]
        options = []
        while words and words[0].startswith("--"):
            options.append(words.pop(0))
        check(all(re.match(r"^--(?:from=[\w.-]+|chmod=[0-7]{3,4})$", opt) for opt in options), f"unsupported COPY options in {dockerfile}: {line.strip()}")
        if any(opt.startswith("--from=") for opt in options):
            continue
        check(len(words) >= 2 and all(re.match(r"^[\w./-]+$", word) for word in words), f"unsupported COPY format in {dockerfile}: {line.strip()}")
        for source in words[:-1]:
            check(".." not in source.split("/") and (Path(source).is_file() or Path(source).is_dir()), f"unsupported COPY source in {dockerfile}: {source}")
            path = str(Path(source) / "ci-cache-input-probe.txt") if Path(source).is_dir() else source
            probes[path].append(image)

router = str(Path("scripts/ci-relevance.sh").resolve())
with tempfile.TemporaryDirectory(prefix="controller-cache-inputs-") as tmp_dir:
    tmp = Path(tmp_dir)
    fixture = tmp / "repo"
    fixture.mkdir()

    def run(*command: str) -> str:
        proc = subprocess.run(command, cwd=fixture, capture_output=True, text=True)
        check(proc.returncode == 0, f"Controller cache routing fixture failed: {' '.join(command)}\n{proc.stderr}")
        return proc.stdout.strip()

    run("git", "init", "-q")
    run("git", "config", "user.name", "test")
    run("git", "config", "user.email", "test@example.invalid")
    run("git", "commit", "--allow-empty", "-qm", "base")
    base = run("git", "rev-parse", "HEAD")
    for path, images in probes.items():
        run("git", "checkout", "-q", "--detach", base)
        file_path = fixture / path
        file_path.parent.mkdir(parents=True, exist_ok=True)
        file_path.write_text("change\n")
        run("git", "add", "-f", "--", path)
        run("git", "commit", "-qm", "change")
        output = tmp / "routing"
        output.write_text("")
        run("bash", router, "push", base, run("git", "rev-parse", "HEAD"), str(output), "quick")
        routing = dict(line.split("=", 1) for line in output.read_text().splitlines() if "=" in line)
        selected = routing["controller_cache_images"].split()
        check(
            routing["run_controller_cache"] == "true" and set(images).issubset(selected),
            f"Controller cache routing lost {' '.join(dict.fromkeys(images))} for COPY/Dockerfile input {path}: {' '.join(selected)}",
        )

print(f"Controller cache routing covers {len(probes)} direct COPY and Dockerfile inputs")
print("CI tools, Rust objects, npm downloads and Business isolated target cache contracts passed")
PY
