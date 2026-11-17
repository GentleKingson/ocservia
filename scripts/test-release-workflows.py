import copy
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml


# Release notes: owner attribution is cosmetic, and cleanup must be idempotent.
def load_workflow(name: str) -> dict:
    return yaml.safe_load(Path(f".github/workflows/{name}.yml").read_text())


def require_check(condition: object, message: str) -> None:
    if not condition:
        print(message, file=sys.stderr)
        sys.exit(1)


release = load_workflow("release")
check = load_workflow("release-check")
products = load_workflow("release-products")
diagnostic = load_workflow("release-business-diagnostic")

triggers = release[True]
require_check(
    sorted(triggers.keys()) == ["push", "workflow_dispatch"]
    and triggers.get("push", {}).get("tags") == ["v*.*.*"],
    "Release triggers changed",
)
require_check(
    list(triggers.get("workflow_dispatch", {}).get("inputs", {}).keys()) == ["version"],
    "dry-run must only select a test version",
)
require_check(release["permissions"] == {"contents": "read"}, "Release must default to read-only")
publish = release["jobs"]["publish"]
require_check(
    publish["if"] == "github.event_name == 'push'"
    and publish["environment"] == {"name": "release-publishing", "deployment": False},
    "publishing must require a tag push and the publishing environment without deployment records",
)
require_check(
    publish["permissions"] == {"contents": "write", "packages": "write", "actions": "read"},
    "publishing permissions changed",
)
require_check(
    sorted(publish["needs"]) == ["assets", "build-amd64", "build-arm64", "prepare"],
    "publishing must wait for both native build/smoke legs and assets",
)
require_check(
    any(re.search(r"gh release create[^\n]*--generate-notes", s.get("run", "")) for s in publish["steps"]),
    "Release must retain native generated notes",
)
publish_body = next(s for s in publish["steps"] if s.get("name") == "Create new draft Release")["run"]
require_check(
    "scripts/strip-release-owner-attribution.py" in publish_body
    and 'gh release edit "v$VERSION" --notes-file "$notes_after"' in publish_body,
    "release notes owner cleanup missing",
)
require_check(
    publish["steps"][0].get("uses", "").startswith("actions/checkout@"),
    "publication must check out the release notes filter",
)
require_check(
    publish["steps"][0].get("with", {}).get("ref") == "${{ github.sha }}",
    "publish checkout must use the immutable build SHA",
)
qualification = publish["steps"][1]
require_check(
    qualification.get("name") == "Require successful Release Check for the tag commit"
    and "if" not in qualification
    and "continue-on-error" not in qualification,
    "qualification must fail closed before publication steps",
)
require_check(
    qualification["env"] == {"GH_TOKEN": "${{ github.token }}", "GH_REPO": "${{ github.repository }}"},
    "qualification must query the current repository with its job token",
)

with tempfile.TemporaryDirectory(prefix="release-qualification-") as tmp_dir:
    d = Path(tmp_dir)
    (d / "git").write_text('#!/bin/sh\n[ "$*" = \'rev-parse HEAD\' ] || exit 2\nprintf \'%s\\n\' "$BUILD_SHA"\n')
    (d / "gh").write_text(
        '#!/usr/bin/env bash\n'
        'set -euo pipefail\n'
        'case "$*" in\n'
        "  'api repos/test/repo/commits/refs/tags/v1.2.3 --jq .sha')\n"
        '    [[ "${TAG_API_FAIL:-false}" == false ]] || exit 1\n'
        '    printf \'%s\\n\' "$TAG_SHA" ;;\n'
        '  "api --paginate --slurp repos/test/repo/actions/workflows/release-check.yml/runs?head_sha=$BUILD_SHA&branch=main&event=workflow_dispatch&per_page=100")\n'
        '    [[ "${RUN_API_FAIL:-false}" == false ]] || exit 1\n'
        '    cat "$RUNS_FILE" ;;\n'
        '  *) exit 2 ;;\n'
        'esac\n'
    )
    (d / "git").chmod(0o755)
    (d / "gh").chmod(0o755)
    sha = "a" * 40
    run_obj = {
        "run_number": 10,
        "head_sha": sha,
        "head_branch": "main",
        "event": "workflow_dispatch",
        "status": "completed",
        "conclusion": "success",
        "html_url": "https://github.com/test/repo/actions/runs/10",
    }
    env = {
        **os.environ,
        "PATH": f"{d}:{os.environ['PATH']}",
        "BUILD_SHA": sha,
        "TAG_SHA": sha,
        "GITHUB_SHA": sha,
        "GITHUB_REF_NAME": "v1.2.3",
        "GH_REPO": "test/repo",
        "RUNS_FILE": str(d / "runs.json"),
        "GITHUB_STEP_SUMMARY": str(d / "summary"),
    }

    def execute_qualification(pages: list[dict], extra: dict[str, str] | None = None) -> bool:
        (d / "runs.json").write_text(json.dumps(pages))
        (d / "summary").write_text("")
        proc = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", qualification["run"]],
            env={**env, **(extra or {})},
            capture_output=True,
            text=True,
        )
        return proc.returncode == 0

    pages = [{"workflow_runs": [run_obj]}]
    require_check(execute_qualification(pages), "same-SHA successful Release Check must allow publication")
    summary_text = (d / "summary").read_text()
    require_check(
        sha in summary_text and run_obj["html_url"] in summary_text,
        "qualification summary must preserve SHA and run URL",
    )
    require_check(not execute_qualification([{"workflow_runs": []}]), "missing Release Check accepted")
    invalid_cases = [
        {"head_sha": "b" * 40},
        {"head_branch": "other"},
        {"event": "push"},
        {"status": "in_progress"},
        *[{"conclusion": state} for state in ("failure", "cancelled", "skipped", "timed_out", "action_required", "neutral")],
        {"conclusion": None},
    ]
    for changes in invalid_cases:
        require_check(
            not execute_qualification([{"workflow_runs": [{**run_obj, **changes}]}]),
            f"invalid Release Check accepted: {changes}",
        )
    newer = {**run_obj, "run_number": 11, "conclusion": "failure"}
    require_check(
        not execute_qualification([{"workflow_runs": [run_obj]}, {"workflow_runs": [newer]}]),
        "older PASS must not hide a newer failed run on another page",
    )
    require_check(
        execute_qualification(
            [{"workflow_runs": [{**run_obj, "conclusion": "failure"}]}, {"workflow_runs": [{**newer, "conclusion": "success"}]}]
        ),
        "latest successful qualification must supersede an older failure",
    )
    for changes in ({"TAG_SHA": "b" * 40}, {"BUILD_SHA": "b" * 40}, {"TAG_API_FAIL": "true"}, {"RUN_API_FAIL": "true"}):
        require_check(
            not execute_qualification(pages, changes),
            f"tag/build mismatch or API failure accepted: {changes}",
        )

filter_script = str(Path("scripts/strip-release-owner-attribution.py").resolve())
original = (
    "## What's Changed\n"
    "* Fix A by @GentleKingson in https://github.com/GentleKingson/ocservia/pull/123\n"
    "* Fix B by @Other in https://github.com/GentleKingson/ocservia/pull/456\n"
    "**Full Changelog**: https://github.com/GentleKingson/ocservia/compare/a...b\n"
    "By @GentleKingson: documentation\n"
)
expected = original.replace("Fix A by @GentleKingson in", "Fix A in")
first_proc = subprocess.run(["python3", filter_script], input=original, capture_output=True, text=True)
require_check(
    first_proc.returncode == 0 and first_proc.stdout == expected,
    f"release note owner filter broke content: {first_proc.stderr}",
)
second_proc = subprocess.run(["python3", filter_script], input=first_proc.stdout, capture_output=True, text=True)
require_check(
    second_proc.returncode == 0 and second_proc.stdout == first_proc.stdout,
    "release note cleanup must be idempotent",
)

changelog = yaml.safe_load(Path(".github/release.yml").read_text())["changelog"]
require_check(
    changelog.get("exclude", {}).get("labels") == ["release/internal"],
    "internal-only PRs must be excluded from release notes",
)
categories = changelog["categories"]
expected_categories = {
    "Breaking changes": "release/breaking",
    "Security": "release/security",
    "Controller": "area/controller",
    "Agent": "area/agent",
    "Relay & Transport": "area/relay",
    "Database & Backup": "area/database",
    "Authentication & Authorization": "area/auth",
    "Web & API": "area/web",
    "Deployment & Operations": "area/deployment",
    "Dependencies": "area/dependencies",
    "Other changes": "*",
}
require_check(
    [(c["title"], c["labels"]) for c in categories] == [(title, [label]) for title, label in expected_categories.items()],
    "release-note categories, labels or catch-all order changed",
)
require_check("--clobber" not in Path(".github/workflows/release.yml").read_text(), "asset replacement must not be enabled")
require_check(
    release.get("concurrency", {}).get("group") == "release-${{ github.ref }}"
    and release.get("concurrency", {}).get("cancel-in-progress") is False,
    "same-tag releases must serialize without cancelling publication",
)
publication_steps = [s.get("run", "") for s in publish["steps"]]
preflight_index = publication_steps.index("python3 scripts/publish-release.py preflight")
draft_index = next(i for i, s in enumerate(publish["steps"]) if s.get("name") == "Create new draft Release")
finish_index = publication_steps.index("python3 scripts/publish-release.py finish")
require_check(
    preflight_index < draft_index < finish_index,
    "publication must preflight before draft and finish verification",
)
for i in (preflight_index, draft_index, finish_index):
    require_check(
        "if" not in publish["steps"][i] and "continue-on-error" not in publish["steps"][i],
        "publication failures must propagate",
    )
require_check(
    any("python3 scripts/publish-release.py check-assets" in s.get("run", "") for s in release["jobs"]["assets"]["steps"]),
    "build-only must validate the complete asset inventory",
)

for kind, job in (("agent_packages", "build-agent-packages"), ("controller_images", "build-controller-images")):
    require_check(
        products[True]["workflow_call"]["outputs"][f"{kind}_artifact_id"]["value"] == f"${{{{ jobs.{job}.outputs.artifact_id }}}}",
        f"{kind} must expose the producing job's artifact ID",
    )
    require_check(
        products["jobs"][job]["outputs"]["artifact_id"] == "${{ steps.upload.outputs.artifact-id }}",
        f"{kind} ID must come from upload-artifact",
    )

assets = release["jobs"]["assets"]
require_check(
    assets.get("outputs", {}).get("artifact_id") == "${{ steps.upload.outputs.artifact-id }}"
    and any(s.get("id") == "upload" and s.get("uses", "").startswith("actions/upload-artifact@") for s in assets["steps"]),
    "prepared assets must expose their upload ID",
)
downloads = [s for s in (*assets["steps"], *publish["steps"]) if s.get("uses", "").startswith("actions/download-artifact@")]
expected_ids = [
    ",".join(f"${{{{ needs.build-{arch}.outputs.{kind}_artifact_id || 'missing' }}}}" for arch in ("amd64", "arm64"))
    for kind in ("agent_packages", "controller_images")
] + ["${{ needs.assets.outputs.artifact_id || 'missing' }}"]
require_check(
    [s.get("with", {}).get("artifact-ids") for s in downloads] == expected_ids,
    "downloads must use exact upstream IDs and reject missing outputs",
)
require_check(
    all(
        s.get("with", {}).get("merge-multiple") is True and "name" not in s["with"] and "pattern" not in s["with"]
        for s in downloads
    ),
    "ID downloads must unpack directly without falling back to attempt names or broad patterns",
)
require_check(
    all(
        j.get("uses")
        not in ("./.github/workflows/ci.yml", "./.github/workflows/security.yml", "./.github/workflows/release-business-diagnostic.yml")
        for j in release["jobs"].values()
    ),
    "formal Release must only build, image-scan, smoke and publish",
)
for arch in ("amd64", "arm64"):
    caller = release["jobs"][f"build-{arch}"]
    require_check(
        caller.get("uses") == "./.github/workflows/release-products.yml" and caller.get("with", {}).get("arch") == arch,
        f"missing native {arch} build",
    )
require_check(
    sorted(products["jobs"].keys()) == ["build-agent-packages", "build-controller-images"],
    "products must build directly",
)
for name, job in products["jobs"].items():
    require_check("if" not in job, f"{name} must build on tags and dispatch")
    require_check("ubuntu-24.04-arm" in job["runs-on"], f"{name} missing native arm64")
    expected_smoke = "release-native-package-smoke.sh" if name == "build-agent-packages" else "release-controller-image-smoke.sh"
    require_check(
        any(expected_smoke in s.get("run", "") for s in job["steps"]),
        f"{name} missing real smoke",
    )

controller = products["jobs"]["build-controller-images"]
ci_doc = load_workflow("ci")
writer = ci_doc["jobs"]["controller-cache"]
require_check(
    ci_doc["jobs"]["ci-relevance"]["outputs"]["controller_cache_images"] == "${{ steps.relevance.outputs.controller_cache_images }}"
    and writer.get("env", {}).get("CONTROLLER_CACHE_IMAGES") == "${{ needs.ci-relevance.outputs.controller_cache_images }}",
    "Controller image selection must flow from path routing to the cache builder",
)
require_check(
    writer.get("needs") == "ci-relevance"
    and writer.get("if") == "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.ci-relevance.outputs.run_controller_cache == 'true'",
    "Controller cache writes must only run on trusted main pushes with Controller image inputs changed",
)
require_check(
    writer.get("strategy", {}).get("matrix", {}).get("arch") == ["amd64", "arm64"]
    and "ubuntu-24.04-arm" in writer["runs-on"],
    "main cache writer must cover both native architectures",
)
require_check(
    "permissions" not in writer and "environment" not in writer and "continue-on-error" not in writer,
    "cache writer must retain read-only authority and propagate export failures",
)
require_check(
    writer.get("concurrency", {}).get("group") == "controller-cache-${{ matrix.arch }}"
    and writer.get("concurrency", {}).get("cancel-in-progress") is False,
    "cache writers must be serialized per architecture",
)
require_check(
    any(s.get("uses") == "./.github/actions/build-cache-credentials" for s in writer["steps"])
    and any(s.get("run") == "bash scripts/build-release-controller.sh --cache-only" for s in writer["steps"]),
    "writer must reuse the shared native builder and credential relay",
)
require_check(
    'bash "${ROOT}/scripts/build-release-controller.sh" >' in Path("scripts/release-business-probe.sh").read_text(),
    "Business must retain the shared default restore-only path",
)
require_check(
    any("bash scripts/build-release-controller.sh" in s.get("run", "") and "--cache-only" not in s["run"] for s in controller["steps"]),
    "Release must retain product build mode",
)
steps = controller["steps"]
ordered = [
    "Build Controller images",
    "Bootstrap pinned image-security tools",
    "Scan exact Controller image archives",
    "Smoke Controller images on the native runner",
    "Upload Controller image archives",
]
indices = [next((i for i, s in enumerate(steps) if s.get("name") == name), None) for name in ordered]
require_check(all(i is not None for i in indices) and indices == sorted(indices), "exact image scan must follow build and precede smoke/upload")
require_check("continue-on-error" not in controller, "Controller scan failure must fail the product job")
for idx in indices:
    assert idx is not None
    require_check(
        "if" not in steps[idx] and "continue-on-error" not in steps[idx],
        "build/scan/smoke/upload must use normal success-only failure propagation",
    )
scan_step = steps[indices[2]]
require_check(scan_step.get("env", {}).get("CONTROLLER_ARCH") == "${{ inputs.arch }}", "scan must use the native product architecture")
roles = ["gateway", "control", "transport", "backup", "edge", "relay", "signer"]
require_check(
    f"for name in {' '.join(roles)}; do" in scan_step["run"]
    and "$RUNNER_TEMP/controller-images/$name-linux-$CONTROLLER_ARCH.tar" in scan_step["run"],
    "scan must consume every exact release archive",
)
require_check(
    'IMAGE_ARCHIVES_TSV="$scan_table" bash scripts/scan-release-images.sh' in scan_step["run"],
    "existing scan failure semantics must remain mandatory",
)
require_check(steps[indices[1]].get("run") == "scripts/bootstrap.sh image-security", "pinned scanner bootstrap missing")
cache = next(s for s in steps if s.get("name") == "Cache pinned image-security tools")["with"]
require_check(
    cache["path"].split() == [".cache/downloads", ".tools"],
    "image-security cache must contain tools only, never vulnerability DB or results",
)
for part in ("runner.os", "runner.arch", "toolchains.lock", "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh"):
    require_check(part in cache["key"], f"image-security cache identity missing {part}")
require_check("github.sha" not in cache["key"], "stable tool cache must not depend on source SHA")
probe = Path("scripts/release-business-probe.sh").read_text()
require_check(
    not re.search(r"scan-release-images|image_security|bootstrap\.sh.*image-security", probe),
    "Business must not own the image-security gate",
)
require_check(
    list(check[True].keys()) == ["workflow_dispatch"] and check["permissions"] == {"contents": "read"},
    "Release Check must be manual and read-only",
)
jobs = check["jobs"]
require_check(jobs["main"]["steps"][0]["run"] == 'test "$GITHUB_REF" = refs/heads/main', "Release Check must use main")
require_check(
    jobs["full-ci"].get("uses") == "./.github/workflows/ci.yml"
    and jobs["full-ci"].get("with", {}).get("profile") == "full",
    "Full CI missing",
)
require_check(
    check[True]["workflow_dispatch"]["inputs"]["contract_base_ref"]["required"] is True
    and jobs["full-ci"].get("with", {}).get("contract_base_ref") == "${{ inputs.contract_base_ref }}",
    "Release Check must pass its explicit contract comparison baseline to Full CI",
)
require_check(jobs["security"].get("uses") == "./.github/workflows/security.yml", "Security missing")
require_check(
    jobs["business"].get("uses") == "./.github/workflows/release-business-diagnostic.yml"
    and jobs["business"].get("with") == {
        "arch": "${{ matrix.arch }}",
        "version": "0.0.0",
        "profile": "smoke",
        "production_signer": True,
        "run-resilience": True,
    },
    "Integrated Business Smoke and recovery must run",
)
quick = jobs["quick"]
require_check(
    quick.get("needs") == "main"
    and any("bash scripts/release-quick-acceptance.sh" in s.get("run", "") for s in quick["steps"]),
    "Quick Install acceptance must run",
)
require_check(
    jobs["products"].get("uses") == "./.github/workflows/release-products.yml"
    and jobs["products"].get("with") == {"version": "0.0.0", "arch": "${{ matrix.arch }}"},
    "Release Check must build, scan and smoke the release products",
)
for job_id in ("products", "business", "quick"):
    require_check(
        jobs[job_id].get("strategy") == {"fail-fast": False, "matrix": {"arch": ["amd64", "arm64"]}}
        and "continue-on-error" not in jobs[job_id],
        f"{job_id} must run on both native architectures",
    )
require_check(
    quick["runs-on"] == "${{ matrix.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}"
    and quick["steps"][-1].get("env", {}).get("CONTROLLER_ARCH") == "${{ matrix.arch }}"
    and "--preserve-env=VERSION,CONTROLLER_ARCH,RUNNER_ARCH," in quick["steps"][-1]["run"],
    "Quick Install must target its native runner",
)
result = jobs["result"]
require_check(
    result["if"] == "always()"
    and sorted(result["needs"]) == ["business", "full-ci", "main", "products", "quick", "security"],
    "Release Check result must handle every dependency",
)
command = result["steps"][0]["run"]

with tempfile.TemporaryDirectory(prefix="release-check-") as tmp_dir:
    d = Path(tmp_dir)
    results = {job_id: {"result": "success"} for job_id in result["needs"]}
    env = {**os.environ, "GITHUB_STEP_SUMMARY": str(d / "summary")}

    def execute_check(data: dict) -> bool:
        proc = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", command],
            env={**env, "RESULTS": json.dumps(data)},
            capture_output=True,
            text=True,
        )
        return proc.returncode == 0

    require_check(execute_check(results), "all PASS must pass Release Check")
    for job_id in list(results.keys()):
        for state in ("failure", "cancelled", "skipped", "missing"):
            failed = copy.deepcopy(results)
            if state == "missing":
                del failed[job_id]
            else:
                failed[job_id]["result"] = state
            require_check(not execute_check(failed), f"Release Check accepted {job_id}/{state}")
    require_check(not execute_check({**results, "unknown": {"result": "success"}}), "Release Check accepted an unknown dependency")

    prepare = release["jobs"]["prepare"]["steps"][0]["run"]
    for event, tag, version, want in (
        ("push", "v1.2.3", "", True),
        ("workflow_dispatch", "main", "0.0.0", True),
        ("push", "main", "1.2.3", False),
        ("push", "v1.2", "", False),
        ("workflow_dispatch", "main", "v1.2.3", False),
        ("workflow_dispatch", "main", "1.2.3;false", False),
    ):
        proc = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", prepare],
            env={**os.environ, "EVENT": event, "TAG": tag, "TEST_VERSION": version, "GITHUB_OUTPUT": str(d / "outputs")},
            capture_output=True,
            text=True,
        )
        require_check((proc.returncode == 0) is want, f"version input validation failed for {tag}/{version}")

    consumer = diagnostic["jobs"]["business"]
    summary_step = next(s for s in consumer["steps"] if s.get("name") == "Summarize business timings and sanitized failures")
    require_check(summary_step.get("if") == "always()", "business timings must remain visible on failure")
    (d / "result.json").write_text(
        json.dumps(
            {
                "probe_status": "FAIL",
                "last_stage": "single_instance_recovery",
                "exit_code": 1,
                "timings": [{"stage": "controller_image_build", "seconds": 42}],
                "private_detail": "omit-this-field",
            }
        )
    )
    (d / "api-checkpoints.jsonl").write_text(
        json.dumps({"name": "real_vpn_relay_recovery", "time": "2026-01-01T00:00:00Z", "status": "PASS", "private_detail": "omit-this-field"})
        + "\n"
    )
    (d / "probe.log").write_text("unrelated log line\nRuntimeError: timeout: owner lease invalidation\n")
    summary_proc = subprocess.run(
        ["bash", "-euo", "pipefail", "-c", summary_step["run"]],
        env={**os.environ, "DIAGNOSTICS": str(d)},
        capture_output=True,
        text=True,
    )
    out_text = summary_proc.stdout
    require_check(
        summary_proc.returncode == 0
        and "controller_image_build" in out_text
        and "real_vpn_relay_recovery" in out_text
        and "RuntimeError: timeout" in out_text,
        "safe diagnostic summary missing",
    )
    require_check(
        "omit-this-field" not in out_text and "unrelated log line" not in out_text,
        "summary must select safe fields instead of dumping logs",
    )
    require_check(
        diagnostic["permissions"] == {"contents": "read"} and "environment" not in consumer,
        "Business must run without publishing authority",
    )
    require_check(
        diagnostic[True]["workflow_call"]["inputs"]["arch"] == {"type": "string", "required": True} and "strategy" not in consumer,
        "Business callers must choose one architecture per job",
    )
    require_check(
        consumer["runs-on"] == "${{ inputs.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}"
        and consumer.get("env", {}).get("CONTROLLER_ARCH") == "${{ inputs.arch }}",
        "Business must run on the native runner of its architecture",
    )
    agent_cache = next(s for s in consumer["steps"] if s.get("id") == "agent-target-cache")["with"]
    require_check(
        agent_cache["key"].startswith("release-agent-target-v1-${{ runner.os }}-${{ inputs.arch }}-")
        and agent_cache["restore-keys"].startswith("release-agent-target-v1-${{ runner.os }}-${{ inputs.arch }}-")
        and "rust/target/agent-${{ inputs.arch }}-" in next(s for s in consumer["steps"] if s.get("id") == "agent-target")["run"],
        "Agent build caches must be isolated per architecture",
    )
    require_check(
        "${{ inputs.arch }}" in next(s for s in consumer["steps"] if s.get("name") == "Preserve business timings and sanitized diagnostics")["with"]["name"],
        "Business evidence must be traceable per architecture",
    )
    gate = next(s for s in consumer["steps"] if s.get("name") == "Require completed business and actual recovery scenarios")["run"]
    require_check(not re.search(r"arch|amd64|arm64", gate), "both architectures must pass identical business checkpoints")
    recovery_match = re.search(r"node --input-type=module <<'JS'\n(.*?)\nJS\n?", gate, re.M | re.S)
    require_check(recovery_match is not None, "actual recovery result gate missing")
    assert recovery_match is not None
    recovery = recovery_match.group(1)
    names = ["controller", "agent", "database", "relay"]
    sample = {
        "resilience_requested": True,
        "resilience_result": "PASS",
        "resilience_scenarios": [{"name": f"resilience_{n}", "status": "PASS"} for n in names],
        "planned_topology": {"native_systemd_node": True},
    }

    def run_recovery(data: dict, flags: dict[str, str] | None = None) -> bool:
        (d / "result.json").write_text(json.dumps(data))
        proc = subprocess.run(
            ["node", "--input-type=module", "-e", recovery],
            env={**os.environ, "DIAGNOSTICS": str(d), "BUSINESS_RUN_RESILIENCE": "true", **(flags or {})},
            capture_output=True,
            text=True,
        )
        return proc.returncode == 0

    require_check(run_recovery(sample), "complete recoveries must pass")
    for name in names:
        failed = copy.deepcopy(sample)
        failed["resilience_scenarios"] = [r for r in failed["resilience_scenarios"] if r["name"] != f"resilience_{name}"]
        require_check(not run_recovery(failed), f"missing {name} recovery accepted")
    for state in ("FAIL", "SKIPPED"):
        require_check(not run_recovery({**sample, "resilience_result": state}), f"{state} recovery accepted")
    require_check(not run_recovery({**sample, "planned_topology": {}}), "recovery without native node accepted")
    require_check(
        run_recovery(
            {**sample, "resilience_requested": False, "resilience_result": "SKIPPED", "resilience_scenarios": []},
            {"BUSINESS_RUN_RESILIENCE": "false"},
        ),
        "unselected recovery must be skipped",
    )

print("Tag builds, dispatch write guard, Release Check failure propagation and actual recovery contracts passed")

with tempfile.TemporaryDirectory(prefix="release-rc-") as tmp_dir:
    d = Path(tmp_dir)
    prepare = release["jobs"]["prepare"]["steps"][0]["run"]
    for version in ("1.2.3", "1.2.3-rc.1", "1.2.3-rc.10"):
        for event, tag, inp in (("push", f"v{version}", ""), ("workflow_dispatch", "main", version)):
            (d / "outputs").write_text("")
            proc = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", prepare],
                env={**os.environ, "EVENT": event, "TAG": tag, "TEST_VERSION": inp, "GITHUB_OUTPUT": str(d / "outputs")},
                capture_output=True,
                text=True,
            )
            require_check(proc.returncode == 0, f"valid RC input failed: {proc.stderr}")
            want_pre = "true" if "-rc." in version else "false"
            require_check(f"is_prerelease={want_pre}\n" in (d / "outputs").read_text(), "wrong prerelease output")
    for version in ("v1.2.3", "v1.2.3-rc.1", "1.2.3-rc", "1.2.3-rc1", "1.2.3-rc.0", "1.2.3-rc.01", "1.2.3-beta.1", "1.2.3+build", "1.2.3-rc.1+build"):
        for event, tag, inp in (("push", f"v{version}", ""), ("workflow_dispatch", "main", version)):
            proc = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", prepare],
                env={**os.environ, "EVENT": event, "TAG": tag, "TEST_VERSION": inp, "GITHUB_OUTPUT": str(d / "outputs")},
                capture_output=True,
                text=True,
            )
            require_check(proc.returncode != 0, f"invalid RC input accepted: {version}")

    (d / "gh").write_text(
        '#!/usr/bin/env bash\n'
        'printf \'%s\\n\' "$*" >>"$CALLS"\n'
        'case "$1 $2" in\n'
        '  \'release list\') echo "${STABLE_BASE-v1.1.0}" ;;\n'
        '  \'api repos/test/repo/releases?per_page=100\')\n'
        '    [[ "${RELEASE_API_FAIL:-false}" == false ]] || exit 1\n'
        '    cat "$RELEASES_FILE" ;;\n'
        '  \'release view\')\n'
        '    if [[ "$*" == *isDraft* ]]; then echo "${PREVIOUS_RC_VALID:-true}"\n'
        '    elif [[ "$*" == *\'--json body\'* ]]; then printf \'%s\\n\' "${NOTES_BODY:-}"\n'
        '    else exit 1\n'
        '    fi ;;\n'
        '  \'release create\') printf \'%s\\0\' "$@" >"$CREATE_ARGS" ;;\n'
        '  \'release edit\') cp "$5" "$RUNNER_TEMP/edited-notes" ;;\n'
        '  *) exit 2 ;;\n'
        'esac\n'
    )
    (d / "gh").chmod(0o755)
    env = {
        **os.environ,
        "PATH": f"{d}:{os.environ['PATH']}",
        "GITHUB_OUTPUT": str(d / "outputs"),
        "CALLS": str(d / "calls"),
        "GH_REPO": "test/repo",
        "GH_TOKEN": "test",
        "GITHUB_ACTOR": "test",
        "RUNNER_TEMP": str(d),
        "GITHUB_SHA": "test-publish-sha",
        "CREATE_ARGS": str(d / "create-args"),
        "RELEASES_FILE": str(d / "releases.json"),
    }
    notes = next(s for s in publish["steps"] if s.get("id") == "notes")["run"]
    publication = next(s for s in publish["steps"] if s.get("name") == "Create new draft Release")["run"]
    rc_cases = (
        ("1.2.3-rc.1", "v1.1.0"),
        ("1.2.3-rc.2", "v1.2.3-rc.1"),
        ("1.2.3-rc.10", "v1.2.3-rc.9"),
        ("1.2.3", "v1.1.0"),
        ("1.3.0-rc.1", "v1.2.3"),
        ("1.3.0", "v1.2.3"),
        ("2.0.0-rc.2", "v2.0.0-rc.1"),
        ("2.0.0-rc.3", "v2.0.0-rc.2"),
        ("2.0.0-rc.4", "v2.0.0-rc.3"),
        ("2.0.0-rc.7", "v2.0.0-rc.5"),
        ("2.0.0", "v1.1.0"),
    )
    for version, base in rc_cases:
        (d / "outputs").write_text("")
        (d / "calls").write_text("")
        (d / "releases.json").write_text(
            json.dumps(
                [
                    [{"tag_name": "v2.0.0-rc.6", "draft": True, "prerelease": True}],
                    [{"tag_name": base, "draft": False, "prerelease": True}],
                ]
            )
        )
        current = {
            **env,
            "VERSION": version,
            "IS_PRERELEASE": "true" if "-rc." in version else "false",
            "STABLE_BASE": "v1.2.3" if version.startswith("1.3.0") else "v1.1.0",
        }
        notes_proc = subprocess.run(["bash", "-euo", "pipefail", "-c", notes], env=current, capture_output=True, text=True)
        require_check(
            notes_proc.returncode == 0 and f"base={base}\n" in (d / "outputs").read_text(),
            f"notes base failed {version}: {notes_proc.stderr}",
        )
        pub_proc = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", publication],
            env={**current, "NOTES_BASE": base},
            capture_output=True,
            text=True,
        )
        require_check(pub_proc.returncode == 0, f"publication failed: {pub_proc.stderr}")
        create = (d / "create-args").read_bytes().decode().split("\0")
        require_check(
            create[create.index("--notes-start-tag") + 1] == base
            and "--verify-tag" in create
            and "--generate-notes" in create
            and "--draft" in create
            and create[create.index("--target") + 1] == "test-publish-sha",
            "publication lost native notes, explicit base or tag verification",
        )
        require_check(
            "--notes" not in create and "/contents/" not in (d / "calls").read_text(),
            "publication must not add checked-in notes",
        )
        is_rc = "-rc." in version
        require_check(
            ("--prerelease" in create) == is_rc and ("--latest=false" in create) == is_rc,
            "wrong publication flags",
        )
    for version, base in (("1.3.0", "v1.2.3"), ("2.0.0-rc.2", "v2.0.0-rc.1"), ("2.0.0-rc.3", "v2.0.0-rc.2"), ("2.0.0-rc.4", "v2.0.0-rc.3")):
        for body in (original, expected):
            (d / "calls").write_text("")
            current = {
                **env,
                "VERSION": version,
                "IS_PRERELEASE": str("-rc." in version).lower(),
                "NOTES_BASE": base,
                "NOTES_BODY": body,
            }
            pub_proc = subprocess.run(["bash", "-euo", "pipefail", "-c", publication], env=current, capture_output=True, text=True)
            require_check(pub_proc.returncode == 0, f"notes cleanup publication failed: {pub_proc.stderr}")
            calls = (d / "calls").read_text()
            require_check("release create " in calls, "new publication must create a draft")
            require_check(("release edit " in calls) == (body != expected), "only matching owner entries should trigger an edit")
            if body != expected:
                require_check((d / "edited-notes").read_text() == expected + "\n", "publication changed other release notes")
    (d / "releases.json").write_text(json.dumps([[]]))
    for version, extra in (("1.2.3-rc.2", {}), ("1.2.3-rc.2", {"RELEASE_API_FAIL": "true"}), ("1.2.3-rc.1", {"STABLE_BASE": ""})):
        proc = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", notes],
            env={**env, "VERSION": version, **extra},
            capture_output=True,
            text=True,
        )
        require_check(proc.returncode != 0, "missing comparison base accepted")

rehearsal = subprocess.run(["python3", "scripts/test-release-publication.py"])
require_check(rehearsal.returncode == 0, "offline release publication rehearsal failed")
