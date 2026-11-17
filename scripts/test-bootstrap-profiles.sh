#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/scripts/bootstrap.sh"
WORKFLOW="${ROOT}/.github/workflows/ci.yml"
RELEASE_WORKFLOW="${ROOT}/.github/workflows/release.yml"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands python3 jq
bash "${ROOT}/scripts/test-bootstrap-platforms.sh"

set +e
output="$(GITHUB_ACTIONS=true "${BOOTSTRAP}" 2>&1)"
status=$?
set -e
if [[ ${status} -ne 2 ]] || [[ "${output}" != *"bootstrap profile must be explicit in GitHub Actions"* ]]; then
  echo "bootstrap without a profile must fail clearly in GitHub Actions" >&2
  exit 1
fi

python3 - "${ROOT}" "${WORKFLOW}" "${RELEASE_WORKFLOW}" <<'PY'
import copy
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
import yaml

root, workflow_path, release_workflow_path = (Path(p) for p in sys.argv[1:4])
workflow = yaml.safe_load(workflow_path.read_text())
release_workflow = yaml.safe_load(release_workflow_path.read_text())
jobs = workflow["jobs"]


def reject(message: str) -> None:
    print(message, file=sys.stderr)
    sys.exit(1)


worker_flags = {
    "docs": "run_docs",
    "go": "run_go",
    "rust": "run_rust",
    "web": "run_web",
    "web-browser": "run_web_browser",
    "database-smoke": "run_database",
}
if sorted(workflow[True].keys()) != ["pull_request", "push", "workflow_call", "workflow_dispatch"]:
    reject("Basic CI triggers drifted")
if workflow[True]["workflow_call"]["inputs"]["profile"] != {"type": "string", "required": True}:
    reject("reusable Basic CI must require an explicit profile")
for trigger in ("workflow_call", "workflow_dispatch"):
    inp = workflow[True][trigger]["inputs"].get("contract_base_ref")
    if not (inp and inp.get("type") == "string" and inp.get("required") is True):
        reject("manual/reusable contracts need one explicit baseline")
if workflow["permissions"] != {"contents": "read"}:
    reject("Basic CI permissions must be read-only")
for job_id, flag in worker_flags.items():
    condition = f"needs.ci-relevance.outputs.{flag} == 'true'"
    if job_id == "go":
        condition += " || needs.ci-relevance.outputs.run_ci_tools == 'true'"
    if job_id == "rust":
        condition += " || needs.ci-relevance.outputs.run_installers == 'true'"
    if jobs[job_id]["if"] != condition:
        reject(f"{job_id} must follow routing")
for job_id in ("go", "rust"):
    steps = jobs[job_id]["steps"]
    heavy = [s for s in steps if re.search(r"scripts/(go-check|rust-check)\.sh|bootstrap\.sh rust-basic", s.get("run", ""))]
    if not heavy or not all(s.get("if") == f"needs.ci-relevance.outputs.run_{job_id} == 'true'" for s in heavy):
        reject(f"{job_id} heavy steps must not run for tools/installers alone")
database = jobs["database-smoke"]
if (
    database["strategy"]["matrix"] != "${{ fromJSON(needs.ci-relevance.outputs.database_matrix) }}"
    or database["env"]["DATABASE_TEST_SCOPE"] != "${{ needs.ci-relevance.outputs.database_scope }}"
):
    reject("matrix must use the router's Quick/Full selection")
if jobs["database-recovery-full"]["strategy"]["matrix"] != {"engine": ["postgres"]}:
    reject("Full retains recovery on PostgreSQL 18")
if "database-history-full" in jobs:
    reject("heavy history must not be a Basic CI job")
if "inputs.profile || 'quick'" not in workflow["concurrency"]["group"] or "github.ref" not in workflow["concurrency"]["group"]:
    reject("Quick and Full must have separate concurrency groups")
guard = next(s for s in jobs["go"]["steps"] if s.get("name") == "CI guard and workflow contracts")
if guard["if"] != "needs.ci-relevance.outputs.run_ci_tools == 'true'":
    reject("CI self-tests must be path selected")
for job_id, job in jobs.items():
    timeout = job["timeout-minutes"]
    if job_id == "database-smoke":
        if timeout != "${{ needs.ci-relevance.outputs.database_scope == 'full' && 90 || 25 }}":
            reject("database runner must bound both full and smoke profiles")
    elif not (isinstance(timeout, int) and timeout > 0):
        reject(f"{job_id} needs a bounded runner")
    if job.get("continue-on-error"):
        reject(f"{job_id} must propagate failures")
    for step in job.get("steps") or []:
        if step.get("continue-on-error"):
            reject(f"{job_id} must propagate step failures")
        uses = step.get("uses")
        if not uses:
            continue
        if not (re.search(r"@[0-9a-f]{40}$", uses) or (job_id == "controller-cache" and uses == "./.github/actions/build-cache-credentials")):
            reject(f"{job_id} has an unpinned action")

result = jobs["basic-ci-result"]
if (
    result["name"] != "Basic CI Result"
    or result["if"] != "always()"
    or sorted(result["needs"]) != sorted([*worker_flags.keys(), "ci-relevance", "database-recovery-full"])
):
    reject("required check identity or dependencies changed")

summary = result["steps"][0]["run"]
for profile in ("quick", "full"):
    if profile == "full":
        selections = [[*worker_flags.keys(), "ci_tools"]]
    else:
        selections = [
            ["docs"], ["web"], ["rust"], ["go", "database-smoke"],
            ["ci_tools"], ["installers"], ["ci_tools", "installers"],
            list(worker_flags.keys()),
        ]
    for selected_in in selections:
        flags = {flag: str(jid in selected_in).lower() for jid, flag in worker_flags.items()}
        flags["run_ci_tools"] = str("ci_tools" in selected_in).lower()
        flags["ci_suites"] = "source-style contracts" if profile == "full" else ("guards" if "ci_tools" in selected_in else "")
        flags["run_installers"] = str("installers" in selected_in).lower()
        flags["run_controller_cache"] = str("web" in selected_in or "installers" in selected_in).lower()
        selected = list(selected_in)
        if flags["run_ci_tools"] == "true":
            selected.append("go")
        if flags["run_installers"] == "true":
            selected.append("rust")
        needs = {"ci-relevance": {"result": "success", "outputs": {**flags, "profile": profile}}}
        for job_id in worker_flags:
            needs[job_id] = {"result": "success" if job_id in selected else "skipped"}
        needs["database-recovery-full"] = {"result": "success" if profile == "full" else "skipped"}

        def check_summary(data: dict) -> bool:
            proc = subprocess.run(
                ["bash", "-eo", "pipefail", "-c", summary],
                env={**os.environ, "RESULTS": json.dumps(data)},
                capture_output=True,
                text=True,
            )
            return proc.returncode == 0

        if not check_summary(needs):
            reject(f"summary rejected valid {profile} selection")
        for job_id in list(needs.keys()):
            for state in ("failure", "cancelled", "skipped", "missing"):
                if state == needs[job_id]["result"]:
                    continue
                broken = copy.deepcopy(needs)
                if state == "missing":
                    del broken[job_id]
                else:
                    broken[job_id]["result"] = state
                if check_summary(broken):
                    reject(f"summary accepted {job_id}/{state}")
        if profile == "full":
            for suite in ("source-style", "contracts"):
                broken = copy.deepcopy(needs)
                broken["ci-relevance"]["outputs"]["ci_suites"] = suite
                if check_summary(broken):
                    reject("Full summary accepted an omitted style/contract suite")
        del needs["ci-relevance"]["outputs"]["run_database"]
        if check_summary(needs):
            reject("summary accepted missing routing")
        needs["ci-relevance"]["outputs"]["run_database"] = flags["run_database"]
        del needs["ci-relevance"]["outputs"]["run_controller_cache"]
        if check_summary(needs):
            reject("summary accepted missing Controller cache routing")

standard = next((s for s in jobs["go"]["steps"] if s.get("run") == "scripts/go-check.sh standard"), None)
if not (standard and standard.get("if") == "needs.ci-relevance.outputs.run_go == 'true'"):
    reject("standard Go checks must follow run_go")
if {k: guard["env"][k] for k in ("CI_SUITES", "CI_RUN_GO")} != {
    "CI_SUITES": "${{ needs.ci-relevance.outputs.ci_suites }}",
    "CI_RUN_GO": "${{ needs.ci-relevance.outputs.run_go }}",
}:
    reject("tool checks must receive the same Go selection")
if guard["env"]["CONTRACT_BASE_REF"] != "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.event_name == 'push' && github.event.before || inputs.contract_base_ref }}":
    reject("contract baseline must use event history or the manual input")
if (
    guard["env"]["CONTRACT_HEAD_SHA"] != "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"
    or guard["env"]["CONTRACT_EVENT"] != "${{ github.event_name }}"
):
    reject("contract PR comparison must receive the actual head")
for suite in ("source-style", "contracts"):
    step = next((item for item in jobs["go"]["steps"] if item.get("run") == f"scripts/bootstrap.sh {suite}"), None)
    if not (step and step.get("if") == f"contains(needs.ci-relevance.outputs.ci_suites, '{suite}')"):
        reject(f"{suite} dependencies must follow their exact suite")

with tempfile.TemporaryDirectory(prefix="ci-entrypoints-") as tmp_dir:
    tmp = Path(tmp_dir)
    work = tmp / "work"
    files = [p for p in (root / "scripts").iterdir() if p.is_file()]
    files.extend(
        root / rel
        for rel in (
            ".github/release.yml",
            ".github/workflows/ci.yml",
            ".github/workflows/release.yml",
            ".github/workflows/release-products.yml",
            ".github/workflows/release-business-diagnostic.yml",
            ".github/workflows/release-check.yml",
            "rust/agent-build.Dockerfile",
            "Makefile",
        )
    )
    for src in files:
        dst = work / src.relative_to(root)
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
    for path in (work / "scripts").glob("test-*"):
        if path.suffix not in (".sh", ".mjs", ".py"):
            continue
        if path.name in ("test-release-upgrade.sh", "test-release-upgrade.mjs", "test-release-workflows.py"):
            continue
        if path.suffix == ".sh":
            stub = '#!/usr/bin/env bash\nprintf \'%s\\n\' "${0##*/}" >> "${CI_TRACE}"\n'
        elif path.suffix == ".py":
            stub = "import os, sys\nopen(os.environ['CI_TRACE'], 'a').write(os.path.basename(sys.argv[0]) + '\\n')\n"
        else:
            stub = f"import fs from 'node:fs'; fs.appendFileSync(process.env.CI_TRACE, {json.dumps(path.name + chr(10))});\n"
        path.write_text(stub)
    for name in ("lint.sh", "check-breaking.sh", "generated-clean.sh"):
        (work / "scripts" / name).write_text('#!/usr/bin/env bash\nprintf \'%s|%s\\n\' "${0##*/}" "$*" >> "${CI_TRACE}"\n')
    for cmd in (("init", "-q"), ("config", "user.name", "fixture"), ("config", "user.email", "fixture@invalid"), ("commit", "--allow-empty", "-qm", "base")):
        subprocess.run(["git", "-C", str(work), *cmd], check=True)
    work_base = subprocess.check_output(["git", "-C", str(work), "rev-parse", "HEAD"], text=True).strip()
    subprocess.run(["git", "-C", str(work), "commit", "--allow-empty", "-qm", "candidate"], check=True)
    for rel in ("bin", "control-plane", "signer"):
        (work / rel).mkdir(parents=True, exist_ok=True)
    for name in ("go", "gofmt"):
        p = work / "bin" / name
        p.write_text('#!/usr/bin/env bash\nprintf \'%s|%s|%s\\n\' "${0##*/}" "${PWD}" "$*" >> "${CI_TRACE}"\n')
        p.chmod(0o755)
    route = tmp / "route"
    route.mkdir()

    def run(env: dict[str, str], *command: str, cwd: Path | None = None) -> str:
        proc = subprocess.run(command, env={**os.environ, **env}, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        if proc.returncode != 0:
            reject(f"{' '.join(command)} failed: {proc.stdout}")
        return proc.stdout

    def git(*args: str) -> str:
        return run({}, "git", "-C", str(route), *args).strip()

    git("init", "-q")
    git("config", "user.name", "test")
    git("config", "user.email", "test@example.invalid")
    git("commit", "--allow-empty", "-qm", "base")
    base = git("rev-parse", "HEAD")
    shared_path = "scripts/buildx-cache.sh"
    scenarios = {
        "Release-only": [".github/workflows/release.yml"],
        "shared tooling": [shared_path],
        "Controller+shared": ["control-plane/internal/platform/app/run.go", shared_path],
        "shared bootstrap": ["scripts/bootstrap.sh"],
    }
    for name, paths in scenarios.items():
        git("checkout", "-q", "--detach", base)
        for p_rel in paths:
            target = route / p_rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("change\n")
        git("add", ".")
        git("commit", "-qm", name)
        out = tmp / "routing"
        out.write_text("")
        run({}, "bash", str(root / "scripts" / "ci-relevance.sh"), "pull_request", base, git("rev-parse", "HEAD"), str(out), cwd=route)
        routing = dict(line.split("=", 1) for line in out.read_text().splitlines() if "=" in line)
        if routing["run_go"] != str(name not in ("Release-only", "shared tooling")).lower():
            reject(f"{name} selected the wrong Go owner")
        trace = tmp / "trace"
        trace.write_text("")
        env = {
            k: routing[guard["env"][k].removeprefix("${{ needs.ci-relevance.outputs.").removesuffix(" }}")]
            for k in ("CI_SUITES", "CI_RUN_GO")
        }
        env.update(
            {
                "CI_TRACE": str(trace),
                "PATH": f"{work / 'bin'}:{os.environ['PATH']}",
                "CONTRACT_BASE_REF": work_base,
                "CONTRACT_EVENT": "workflow_dispatch",
            }
        )
        if routing["run_ci_tools"] != "true":
            reject(f"{name} omitted tool contracts")
        run(env, "bash", "-eo", "pipefail", "-c", guard["run"], cwd=work)
        trace_lines = trace.read_text().splitlines()
        if routing["run_go"] == "true" and any(re.match(r"^go(fmt)?\|", line) for line in trace_lines):
            reject(f"{name} tool suite duplicated standard Go checks")
        if routing["run_go"] == "true":
            run(env, "bash", "-eo", "pipefail", "-c", standard["run"], cwd=work)
        calls = trace.read_text().splitlines()
        if routing["run_go"] == "true" and not all(f"go|{work / mod}|test -count=1 ./..." in calls for mod in ("control-plane", "signer")):
            reject(f"{name} skipped a Go workspace module")
        if any(line.startswith("test-g6-") for line in calls):
            reject(f"{name} retained retired G6 contract checks")
        if not all(calls.count(t) == 1 for t in ("test-build-cache-credentials.sh", "test-buildx-cache-fallback.sh", "test-secret-scan-config.sh")):
            reject(f"{name} lost shared tooling checks")
        if name == "shared bootstrap" and not all(calls.count(t) == 1 for t in ("test-real-e2e-hosted.py", "test-real-e2e-business.py", "test-real-e2e-artifact.sh")):
            reject(f"{name} skipped offline Cross-VM contracts")
        if name == "shared bootstrap":
            expected = ["lint.sh|source-style", "lint.sh|contracts", f"check-breaking.sh|{work_base}", "generated-clean.sh|"]
            if [c for c in calls if re.match(r"^(lint|check-breaking|generated-clean)\.sh\|", c)] != expected:
                reject("contract suite order or single execution drifted")
            head = run({}, "git", "-C", str(work), "rev-parse", "HEAD").strip()
            tree = run({}, "git", "-C", str(work), "rev-parse", f"{work_base}^{{tree}}").strip()
            diverged = run({}, "git", "-C", str(work), "commit-tree", tree, "-p", work_base, "-m", "base-branch-only").strip()
            trace.write_text("")
            pr_env = {**env, "CI_SUITES": "contracts", "CONTRACT_EVENT": "pull_request", "CONTRACT_BASE_REF": diverged, "CONTRACT_HEAD_SHA": head}
            run(pr_env, "bash", "-eo", "pipefail", "-c", guard["run"], cwd=work)
            if trace.read_text().splitlines() != expected[1:]:
                reject("PR contract comparison did not use its merge base")
            missing_proc = subprocess.run(
                ["bash", "-eo", "pipefail", "-c", guard["run"]],
                cwd=work,
                env={**os.environ, **env, "CI_SUITES": "contracts", "CONTRACT_BASE_REF": ""},
                capture_output=True,
                text=True,
            )
            if missing_proc.returncode == 0:
                reject("manual contracts accepted an omitted baseline")
        if name == "Release-only":
            if "test-bootstrap-profiles.sh" in calls:
                reject("Release-only must not run unrelated guards")
            rc_path = work / ".github/workflows/release-check.yml"
            original = rc_path.read_text()
            rc_path.write_text(original.replace("uses: ./.github/workflows/security.yml", "uses: ./.github/workflows/ci.yml", 1))
            proc = subprocess.run(["bash", "-eo", "pipefail", "-c", guard["run"]], cwd=work, env={**os.environ, **env}, capture_output=True, text=True)
            if proc.returncode == 0:
                reject("Release-only accepted a substituted security entrypoint")
            rc_path.write_text(original)
        print(f"{name}: selected entrypoints and shared tooling counts passed")

security = yaml.safe_load((root / ".github/workflows/security.yml").read_text())
if sorted(security[True].keys()) != ["schedule", "workflow_call", "workflow_dispatch"]:
    reject("security checks must support scheduled, manual, and release runs")
if security["permissions"] != {"contents": "read"}:
    reject("security scans must be read-only")
scan = security["jobs"]["scan"]
if scan["strategy"]["fail-fast"] is not False:
    reject("security scans must not cancel siblings on failure")
checks = scan["strategy"]["matrix"]["include"]
if sorted(c["profile"] for c in checks) != ["go-security", "npm-security", "rust-security", "secret-scan"]:
    reject("security scans must cover secrets and all three dependency ecosystems")
if not any((s.get("with") or {}).get("fetch-depth") == "${{ matrix.profile != 'secret-scan' && 1 || 0 }}" for s in scan["steps"]):
    reject("secret scans need complete history")
bootstrap_text = (root / "scripts/bootstrap.sh").read_text()
dispatch = 'case "${PROFILE}" in' + bootstrap_text.split('case "${PROFILE}" in')[-1]
stubs = "\n".join(f"{name}() {{ echo {name}; }}" for name in re.findall(r"^(install_\w+|verify_\w+)\(\)", bootstrap_text, re.M))
expected_profiles = {
    "source-style": ["verify_host_command", "verify_host_command", "install_ruff"],
    "contracts": ["install_node", "install_npm", "install_contract_tools", "verify_java", "install_web_dependencies"],
    "go-security": ["install_go", "install_govulncheck"],
    "rust-security": ["install_rust", "install_cargo_audit", "install_cargo_deny"],
    "npm-security": ["install_node", "install_npm"],
    "package-tools": ["install_nfpm"],
    "image-security": ["install_syft", "install_grype", "verify_host_command"],
}
for profile, expected in expected_profiles.items():
    proc = subprocess.run(["bash", "-eu", "-c", stubs + "\n" + dispatch], env={**os.environ, "PROFILE": profile}, capture_output=True, text=True)
    if proc.returncode != 0 or proc.stdout.split() != expected:
        reject(f"{profile} installs unrelated tools/dependencies: {proc.stdout}")
rust_scan = next(c for c in checks if c["profile"] == "rust-security")["command"]
if [line.strip() for line in rust_scan.splitlines()] != [
    "cd rust",
    "cargo audit",
    "cargo audit --file ../deploy/production/relay.Cargo.lock",
    "cargo deny --locked check advisories",
]:
    reject("workspace and Relay advisory scans must remain fresh and fail closed")

products = yaml.safe_load((root / ".github/workflows/release-products.yml").read_text())
build_steps = products["jobs"]["build-agent-packages"]["steps"]
restore = next((s for s in build_steps if s.get("name") == "Restore native-package tool cache"), None)
save = next((s for s in build_steps if s.get("name") == "Save native-package tool cache"), None)
release_build = next((s for s in build_steps if "bash scripts/build-release-agent.sh" in s.get("run", "")), None)
if not (restore and restore.get("id") == "native-package-tools-cache"):
    reject("release tool cache restore must expose its primary key")
if not (save and save["with"]["path"] == restore["with"]["path"] and save["with"]["key"] == "${{ steps.native-package-tools-cache.outputs.cache-primary-key }}"):
    reject("release tool cache save must reuse restore path and primary key")
if not ("success()" in save["if"] and "steps.native-package-tools-cache.outputs.cache-hit != 'true'" in save["if"]):
    reject("release tool cache save must require a successful miss")
if not (release_build and build_steps.index(release_build) < build_steps.index(save)):
    reject("release tool cache must not save before native build success")
publish = release_workflow["jobs"]["publish"]
if publish["environment"] != {"name": "release-publishing", "deployment": False}:
    reject("release publishing environment must disable deployment records")
if publish["permissions"].get("contents") != "write":
    reject("release publishing must retain contents write as a job-local permission")
PY

if "${ROOT}/scripts/go-check.sh" unsupported >/dev/null 2>&1; then
  echo "go-check.sh accepted an unsupported execution mode" >&2
  exit 1
fi

# Every go.work module must reach gofmt, vet and the unit/race tests in go-check.sh.
workspace_modules="$(sed -n 's#^[[:space:]]*\./\([^[:space:]]*\)$#\1#p' "${ROOT}/go.work" | sort)"
checked_modules="$(sed -n 's/^GO_MODULES=(\(.*\))$/\1/p' "${ROOT}/scripts/go-check.sh" | tr ' ' '\n' | sort)"
if [[ -z "${workspace_modules}" || "${workspace_modules}" != "${checked_modules}" ]]; then
  echo "go-check.sh GO_MODULES must match the go.work modules" >&2
  exit 1
fi
