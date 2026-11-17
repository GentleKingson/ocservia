#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
python3 - <<'PY'
import hashlib
import os
import re
import shlex
import shutil
import subprocess
import tempfile
from pathlib import Path
import yaml

products = yaml.safe_load(Path(".github/workflows/release-products.yml").read_text())
steps = products["jobs"]["build-agent-packages"]["steps"]
target = next(s for s in steps if s.get("id") == "agent-target")["run"]
restore = next(s for s in steps if s.get("id") == "agent-target-cache")
save = next(s for s in steps if s.get("uses", "").startswith("actions/cache/save@") and s.get("with", {}).get("path") == "${{ steps.agent-target.outputs.path }}")
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
validate_step = next(s for s in steps if s.get("id") == "smoke-packages")
assert (
    save.get("if") == "${{ success() && steps.agent-target-cache.outputs.cache-hit != 'true' }}"
    and steps.index(save) > steps.index(validate_step)
), "save must follow successful validation"
compile_step = next(s for s in steps if s.get("id") == "build-packages")
assert "if" not in compile_step, "a cache hit must never skip binary compilation"
# Execute the builder and its entire container command with external tool doubles.
def script(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text('#!/usr/bin/env bash\nset -euo pipefail\n' + body + '\n')
    path.chmod(0o755)


with tempfile.TemporaryDirectory(prefix='native-builder-') as tmp_dir:
    work = Path(tmp_dir)
    (work / 'scripts').mkdir()
    for name in ('build-agent-binaries.sh', 'env.sh'):
        shutil.copy2(Path('scripts') / name, work / 'scripts')
    (work / 'rust').mkdir()
    shutil.copy2('rust/agent-build.Dockerfile', work / 'rust')
    script(work / 'scripts/bootstrap.sh', '[[ "$*" == native-packages ]]')
    script(work / '.tools/bin/uname', 'echo "$FIXTURE_HOST"')
    script(work / '.tools/bin/getconf', 'echo "${FIXTURE_LIBC:-glibc 2.34}"')
    for name in ('rpm', 'rustc', 'file'):
        script(work / '.tools/bin' / name, 'exit 0')
    script(work / '.tools/bin/readelf', 'echo "${FIXTURE_ELF:-$ELF_MACHINE}"')
    script(work / '.tools/bin/cargo', '''printf '%s|%s\\n' "$CARGO_TARGET_DIR" "$*" >>"$FIXTURE_COMPILE_LOG"
[[ "${FIXTURE_COMPILE_FAIL:-false}" == false ]] || exit 19
mkdir -p "$CARGO_TARGET_DIR/release"
for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
  printf '#!/bin/sh\\necho "%s %s"\\n' "$binary" "${FIXTURE_BINARY_VERSION:-$VERSION}" >"$CARGO_TARGET_DIR/release/$binary"
  chmod 755 "$CARGO_TARGET_DIR/release/$binary"
done''')
    script(work / '.tools/bin/docker', '''case "$1" in
info) echo "${FIXTURE_DAEMON:-$PACKAGE_ARCH}" ;;
image) echo "$PACKAGE_ARCH" ;;
build) cat >/dev/null ;;
run)
  shift
  while (($#)); do
    case "$1" in
      --rm) shift ;;
      --platform|--user|--volume|--workdir|--security-opt) shift 2 ;;
      --cap-drop=*|--security-opt=*) shift ;;
      --env) if [[ "$2" == *=* ]]; then export "$2"; fi; shift 2 ;;
      *) shift; exec "$@" ;;
    esac
  done
  exit 2 ;;
*) exit 2 ;;
esac''')
    for arch, host in (('amd64', 'x86_64'), ('arm64', 'aarch64')):
        env = {**os.environ, 'PATH': f'{work / ".tools/bin"}:{os.environ["PATH"]}', 'VERSION': '1.2.3',
               'PACKAGE_ARCH': arch, 'FIXTURE_HOST': host, 'FIXTURE_COMPILE_LOG': str(work / 'compile.log')}
        def execute(extra=None):
            return subprocess.run(['bash', str(work / 'scripts/build-agent-binaries.sh')],
                                  cwd=work, env={**env, **(extra or {})}, capture_output=True, text=True)
        (work / 'compile.log').write_text('')
        proc = execute()
        assert proc.returncode == 0, proc.stdout + proc.stderr
        cache = f'rust/target/agent-{arch}-{hashlib.sha256(Path("rust/agent-build.Dockerfile").read_bytes()).hexdigest()[:16]}'
        assert (work / 'compile.log').read_text().splitlines() == [f'{work / cache}|build --locked --release --package ocservia-agent --package ocservia-privd --package ocservia-upgrader']
        proc = subprocess.run(['bash', '-euo', 'pipefail', '-c', target.replace('${{ inputs.arch }}', arch)],
                              env={**os.environ, 'GITHUB_OUTPUT': str(work / 'outputs')}, capture_output=True, text=True)
        assert proc.returncode == 0 and (work / 'outputs').read_text().splitlines()[-1] == f'path={cache}'
        for binary in ('ocservia-agent', 'ocservia-privd', 'ocservia-upgrader'):
            assert subprocess.check_output([str(work / 'rust/target/release' / binary), '--version'], text=True).strip() == f'{binary} 1.2.3'
        for failure in ({'FIXTURE_LIBC': 'glibc 2.35'}, {'FIXTURE_ELF': 'foreign'}, {'FIXTURE_BINARY_VERSION': '0.0.0'},
                        {'FIXTURE_COMPILE_FAIL': 'true'}, {'FIXTURE_DAEMON': 'foreign'}, {'FIXTURE_HOST': 'foreign'}):
            shutil.rmtree(work / 'rust/target/release', ignore_errors=True)
            proc = execute(failure)
            assert proc.returncode != 0, (failure, proc.stdout, proc.stderr)
            assert not (work / 'rust/target/release').exists(), 'failed validation published binaries'


# Inspect Dockerfile instructions and stage ownership rather than text adjacency.
def docker_instructions(path):
    instructions = []
    for line in re.sub(r'\\\r?\n', ' ', Path(path).read_text()).splitlines():
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        op, args = line.split(None, 1)
        instructions.append((op.upper(), shlex.split(args)))
    return instructions


builder_runs = [arg for op, args in docker_instructions('rust/agent-build.Dockerfile') if op == 'RUN' for arg in args]
for option in ('minrate=512k', 'timeout=15', 'max_parallel_downloads=10'):
    assert f'--setopt={option}' in builder_runs, option
assert not any(re.match(r'(?:--setopt=)?(?:retries=|fastestmirror=|mirrorlist=|baseurl=|gpgcheck=0|sslverify=0)|--nogpgcheck$', arg) for arg in builder_runs)
stages = {}
for op, args in docker_instructions('rust/transportd.Dockerfile'):
    if op == 'FROM':
        current = args[-1].lower() if len(args) >= 3 and args[-2].upper() == 'AS' else f'anonymous-{len(stages)}'
        stages[current] = []
    else:
        stages[current].append((op, args))
planner = stages['planner']
assert planner.index(('COPY', ['rust/crates', './crates'])) < planner.index(('RUN', ['cargo', 'chef', 'prepare', '--recipe-path', 'recipe.json']))
for stage in ('dependencies', 'build'):
    for source in ('.cargo', 'vendor'):
        assert ('COPY', [f'rust/{source}', f'./{source}']) in stages[stage], (stage, source)
assert ('COPY', ['rust/crates', './crates']) in stages['build']
for stage, command in (('dependencies', ['cargo', 'chef', 'cook']), ('build', ['cargo', 'build'])):
    args = next(args for op, args in stages[stage] if op == 'RUN' and args[:len(command)] == command)
    pairs = list(zip(args, args[1:]))
    assert '--locked' in args and '--release' in args and ('--package', 'ocservia-transportd') in pairs
    if stage == 'dependencies':
        assert ('--recipe-path', 'recipe.json') in pairs

print("Release Rust cache identity, unconditional rebuild and dependency layering contracts passed")

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

business = workflows["release-business-diagnostic"]["jobs"]["business"]["steps"]
products = yaml.safe_load(Path(".github/workflows/release-products.yml").read_text())["jobs"]["build-agent-packages"]["steps"]
for step_id in ("agent-target", "agent-target-cache"):
    actual = next((s for s in business if s.get("id") == step_id), None)
    expected = next((s for s in products if s.get("id") == step_id), None)
    check(actual.get("if") == "${{ !inputs.use_products }}", f"Business {step_id} must skip artifact mode")
    check({key: value for key, value in actual.items() if key != "if"} == expected, f"Business {step_id} must match the native release builder")
check(all("rust/target" not in [line.strip() for line in str((s.get("with") or {}).get("path", "")).splitlines()] for s in business), "Business must not cache all Rust targets")

with tempfile.TemporaryDirectory(prefix='controller-build-dispatch-') as tmp_dir:
    work = Path(tmp_dir)
    script(work / 'docker', '''[[ "$1 $2" != 'buildx create' ]] || exit 0
[[ "$1 $2" == 'buildx build' ]] || exit 2
printf '%s\\0' "$@" >>"$DOCKER_TRACE"
printf '\\n' >>"$DOCKER_TRACE"
for arg in "$@"; do
  if [[ "$arg" == type=docker,dest=* ]]; then printf 'fixture image\\n' >"${arg#type=docker,dest=}"; fi
done''')
    script(work / 'uname', 'echo x86_64')
    env = {**os.environ, 'PATH': f'{work}:{os.environ["PATH"]}', 'DOCKER_TRACE': str(work / 'trace'),
           'VERSION': '1.2.3', 'SOURCE_COMMIT': 'a' * 40, 'CONTROLLER_ARCH': 'amd64',
           'BUILDX_BUILDER': 'fixture', 'BUILD_CACHE_AVAILABLE': 'false', 'OUTPUT_DIR': str(work / 'images')}
    proc = subprocess.run(['bash', 'scripts/build-release-controller.sh'], env=env, capture_output=True, text=True)
    check(proc.returncode == 0, f'Controller dispatch failed: {proc.stdout}{proc.stderr}')
    targets = []
    for line in (work / 'trace').read_text().splitlines():
        args = line.split('\0')
        image = args[args.index('--tag') + 1].removeprefix('ghcr.io/gentlekingson/ocservia/').removesuffix(':1.2.3-linux-amd64')
        dockerfile = args[args.index('--file') + 1].removeprefix(str(Path.cwd()) + '/')
        check(f'type=docker,dest={work}/images/{image}-linux-amd64.tar' in args, 'Controller archive identity drifted')
        targets.append((image, dockerfile))
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
